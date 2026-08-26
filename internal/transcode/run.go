package transcode

import (
	"context"
	goerrors "errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"worker-transcode/internal/config"
	"worker-transcode/internal/core/enums"
	"worker-transcode/internal/core/utils"
	"worker-transcode/internal/db/models"
	"worker-transcode/internal/downloader"
	"worker-transcode/internal/queue"
	"worker-transcode/internal/transcoder"
	"worker-transcode/internal/uploader"

	"go.mongodb.org/mongo-driver/bson"
)

// ─── Transcode pipeline ──────────────────────────────────────
//
// One job = one file: โหลด original จาก local storage-node หรือ S3 origin → probe →
// encode ทุก resolution ที่ต่ำกว่าต้นฉบับ (360 เสมอ, 480/720/1080 ตาม
// short side, YouTube-style 95% tolerance) → permanent S3 + media/clone
// ทันที; ถ้าไม่มี permanent S3 จึง fallback ไป Temp/worker-transfer
//
// จบงาน: file.metadata.highest = resolution สูงสุด (marker ให้ enqueuer
// ไม่หยิบซ้ำ) + กระจายไป cloned files
//
// GPU: auto-detect nvenc/amf/qsv (ปิดได้ผ่าน transcode_config.gpuEnabled)
// ล้มเหลว → fallback CPU อัตโนมัติ
//
// Progress: download 10% → encode+upload 85% (เฉลี่ยตาม resolution) → 100

// Run executes one claimed transcode job. Per-process logs remain local and
// are removed by the seven-day retention cleanup.
func Run(jobCtx context.Context, job *models.VideoProcess) error {
	return run(jobCtx, job)
}

func run(ctx context.Context, job *models.VideoProcess) error {
	fileID := derefStr(job.FileID)
	slug := derefStr(job.Slug)
	if fileID == "" {
		return fmt.Errorf("job has no fileId")
	}

	procLogger := utils.NewProcessLogger(slug)
	defer procLogger.Close()

	exePath, _ := os.Executable()
	baseDir := filepath.Dir(exePath)
	if strings.Contains(exePath, "go-build") {
		baseDir, _ = os.Getwd()
	}
	workDir := filepath.Join(baseDir, "transcode", slug)
	os.MkdirAll(workDir, 0755)

	// encode แพง — เก็บ workDir ไว้ตอน fail ให้ retry ใช้ของเดิมต่อ
	// (original cache + ไฟล์ที่ encode เสร็จแล้ว)
	var success bool
	defer func() {
		cancelled := goerrors.Is(context.Cause(ctx), queue.ErrJobCancelled)
		if success || cancelled {
			os.RemoveAll(workDir)
			utils.LogMain("🧹 [%s] Cleaned up temp dir", slug)
		} else {
			utils.LogMain("⚠️  [%s] Keeping temp dir for retry: %s", slug, workDir)
		}
	}()

	utils.LogMain("🎬 [%s] START TRANSCODE", slug)

	// GPU kill switch จาก setting (default เปิด — auto-detect เอง)
	transcoder.SetGPUEnabled(gpuEnabledFromSetting(ctx))
	// A GPU failure falls back to CPU only for this job. Probe the GPU again on
	// the next job instead of leaving this worker process on CPU forever.
	defer transcoder.ResetForcedCPU()

	// ─── STEP 1: DOWNLOAD original ─────────────────────────────
	// Prefer the retained Temp Original. This avoids pulling the same large file
	// back through storage-node after download/transfer has already staged it.
	// If the Temp object is missing or unreadable, fall back to installed media.
	tempIngest, tempStorage, tempErr := findTempOriginalIngest(ctx, fileID)
	var originalMedia *models.Media
	var sourceStorage *models.Storage
	var expectedSize int64
	useTemp := tempErr == nil
	if useTemp {
		expectedSize = tempIngest.Size
	}
	resolveInstalledSource := func() error {
		var resolveErr error
		originalMedia, resolveErr = findOriginalMedia(ctx, fileID)
		if resolveErr != nil {
			return resolveErr
		}
		sourceStorage, resolveErr = models.StorageModel.FindByID(ctx, derefStr(originalMedia.StorageID))
		if resolveErr != nil {
			return fmt.Errorf("source storage not found: %w", resolveErr)
		}
		expectedSize = originalMediaSize(originalMedia)
		return nil
	}
	if !useTemp {
		if err := resolveInstalledSource(); err != nil {
			return fmt.Errorf("prepare: no retained Temp Original and %w", err)
		}
	}

	originalPath := filepath.Join(workDir, enums.FileNameOriginal)
	if cachedOriginalValid(originalPath, expectedSize) {
		utils.LogMain("📥 [%s] Original already cached — skipping download", slug)
		completeStep(ctx, job.ID, "download")
		updateOverallPercent(ctx, job.ID, 10)
	} else {
		os.Remove(originalPath)
		startStep(ctx, job.ID, "download")

		write := stepThrottle(1)
		logProgress := pctLogger64(slug, "download")
		onProgress := func(done, total int64) {
			logProgress(done, total)
			if total > 0 {
				pct := float64(done) / float64(total) * 100
				if write(pct) {
					updateStepAndOverall(ctx, job.ID, "download", pct, pct*0.10)
				}
			}
		}

		downloaded := false
		if useTemp {
			objectKey := ingestObjectKey(tempIngest, fileID)
			utils.LogMain("📥 [%s] Downloading retained Temp Original (key=%s)", slug, objectKey)
			if tempDownloadErr := uploader.DownloadFromS3(
				ctx, tempStorage, objectKey, originalPath, onProgress,
			); tempDownloadErr == nil {
				downloaded = true
			} else {
				utils.LogMain("⚠️  [%s] Temp Original unavailable: %v — falling back to installed media", slug, tempDownloadErr)
				_ = os.Remove(originalPath)
				if resolveErr := resolveInstalledSource(); resolveErr != nil {
					return fmt.Errorf("download temp original: %v; fallback: %w", tempDownloadErr, resolveErr)
				}
			}
		}

		if !downloaded {
			var sourceURL string
			if sourceStorage.Type == enums.StorageTypeS3 {
				var resolveErr error
				sourceURL, resolveErr = sourceStorage.GetOriginObjectPathURL(originalMedia.ObjectPath())
				if resolveErr != nil {
					return fmt.Errorf("download: resolve S3 origin: %w", resolveErr)
				}
			} else {
				hostPort := sourceStorage.GetHostPort()
				if hostPort == "" {
					return fmt.Errorf("download: source storage has no host")
				}
				sourceURL = fmt.Sprintf("http://%s/%s.mp4", hostPort, originalMedia.Slug)
			}
			utils.LogMain("📥 [%s] Downloading installed Original %s", slug, sourceURL)
			if err := downloader.DownloadURL(ctx, sourceURL, originalPath, onProgress); err != nil {
				return fmt.Errorf("download: %w", err)
			}
		}
		completeStep(ctx, job.ID, "download")
		updateOverallPercent(ctx, job.ID, 10)
	}

	// ─── STEP 2: PROBE ────────────────────────────────────────
	videoInfo, err := transcoder.ProbeVideoInfo(originalPath)
	if err != nil {
		return fmt.Errorf("probe: %w", err)
	}
	shortSide := transcoder.ShortSide(videoInfo.Width, videoInfo.Height)
	utils.LogMain("📐 [%s] %dx%d %ds codec=%s bitrate=%dkbps shortSide=%d",
		slug, videoInfo.Width, videoInfo.Height, videoInfo.Duration, videoInfo.Codec, videoInfo.VideoBitrate, shortSide)

	// In separated mode, legacy originals can still contain embedded audio.
	// Extract missing tracks once before encoding video-only renditions. New
	// separated originals normally contain no audio, so this is a cheap no-op.
	audioTrackCount := -1
	if config.AppConfig.MediaLayout == "separated" {
		if err := rejectLegacyMuxedRenditions(ctx, fileID); err != nil {
			return err
		}
		audioTrackCount, err = ensureSeparatedAudio(ctx, job, fileID, slug, originalPath, workDir)
		if err != nil {
			return fmt.Errorf("separate audio: %w", err)
		}
	}

	// ─── STEP 3: DETERMINE resolutions ────────────────────────
	var pending []string
	for _, res := range transcoder.DetermineResolutions(shortSide) {
		if covered, reason := resolutionCovered(ctx, fileID, res); covered {
			utils.LogMain("⏭️  [%s] Skip %sp — %s", slug, res, reason)
			continue
		}
		pending = append(pending, res)
	}

	if len(pending) == 0 {
		// ทุก resolution ครบแล้ว — จบงาน + ประทับ highest กัน enqueuer หยิบซ้ำ
		utils.LogMain("✅ [%s] All resolutions covered — completing", slug)
		setHighest(ctx, fileID, slug, highestCovered(ctx, fileID))
		if audioTrackCount >= 0 {
			setSeparatedFileMetadata(ctx, fileID, audioTrackCount)
		}
		success = true
		return nil
	}

	models.VideoProcessModel.UpdateByID(ctx, job.ID, bson.M{"$set": bson.M{"resolutions": pending}})
	utils.LogMain("🎯 [%s] Target resolutions: %v", slug, pending)

	// init timeline ราย resolution
	timelineInit := bson.M{}
	for _, res := range pending {
		timelineInit[fmt.Sprintf("timeline.encode_%s.status", res)] = enums.StepStatusPending
		timelineInit[fmt.Sprintf("timeline.upload_%s.status", res)] = enums.StepStatusPending
	}
	models.VideoProcessModel.UpdateByID(ctx, job.ID, bson.M{"$set": timelineInit})

	// Prefer durable S3 and create playable media immediately. Temp/transfer is
	// retained as a fallback when no permanent S3 is available or its upload
	// fails temporarily.
	var permanentS3 *models.Storage
	if !hasAvailableLocalStorage(ctx) {
		permanentS3, _ = resolveS3VideoStorage(ctx)
	}
	var tempS3 *models.Storage
	if permanentS3 == nil {
		tempS3, err = resolveS3TempStorage(ctx)
		if err != nil {
			return fmt.Errorf("no permanent S3 or S3 temp available: %v: %w", err, queue.ErrJobRequeue)
		}
	}

	// ─── STEP 4: ADAPTIVE ENCODE + UPLOAD ─────────────────────
	// CPU always stays sequential. GPU fanout decodes the original once. For
	// long 720p+ inputs, 360p is encoded/published first while the remaining
	// renditions fan out in a second decode pass.
	tasks := makeRenditionTasks(pending, workDir, videoInfo)
	targets := &uploadTargets{permanent: permanentS3, temp: tempS3}
	if err := runAdaptivePipeline(
		ctx, job, fileID, slug, originalPath, videoInfo, int(shortSide), tasks, targets,
	); err != nil {
		return err
	}
	highest := highestTaskResolution(tasks)

	// ─── STEP 5: FINISH — ประทับ metadata.highest ─────────────
	if h := highestCovered(ctx, fileID); h > highest {
		highest = h
	}
	setHighest(ctx, fileID, slug, highest)
	if audioTrackCount >= 0 {
		setSeparatedFileMetadata(ctx, fileID, audioTrackCount)
	}
	updateOverallPercent(ctx, job.ID, 100)

	success = true
	utils.LogMain("✅ [%s] TRANSCODE COMPLETE (%v)", slug, pending)
	return nil
}

// cachedOriginalValid — ไฟล์ที่โหลดค้างไว้จากรอบก่อนใช้ได้ไหม (ขนาดตรง metadata)
func cachedOriginalValid(path string, expected int64) bool {
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		return false
	}
	return expected == 0 || info.Size() == expected
}

func originalMediaSize(media *models.Media) int64 {
	var expected int64
	if media == nil {
		return expected
	}
	if media.Metadata != nil {
		switch v := media.Metadata.Size.(type) {
		case int64:
			expected = v
		case float64:
			expected = int64(v)
		case int:
			expected = int64(v)
		case int32:
			expected = int64(v)
		}
	}
	return expected
}

// highestCovered — resolution สูงสุดที่มี media หรือ ingest ค้างอยู่แล้ว
func highestCovered(ctx context.Context, fileID string) int {
	highest := 0
	for _, res := range enums.TranscodedResolutions {
		if covered, _ := resolutionCovered(ctx, fileID, res); covered {
			if resInt, err := strconv.Atoi(res); err == nil && resInt > highest {
				highest = resInt
			}
		}
	}
	return highest
}

// setHighest ประทับ file.metadata.highest (marker ให้ enqueuer ไม่หยิบซ้ำ)
// + กระจายไป cloned files
func setHighest(ctx context.Context, fileID, slug string, highest int) {
	if highest <= 0 {
		highest = 360 // DetermineResolutions มี 360 เสมอ — กันค่าเพี้ยน
	}
	models.FileModel.UpdateByID(ctx, fileID, bson.M{"$set": bson.M{
		"metadata.highest": highest,
	}})
	utils.LogMain("✅ [%s] file.metadata.highest = %d", slug, highest)
	updateClonedFilesHighest(ctx, fileID, highest, slug)
}
