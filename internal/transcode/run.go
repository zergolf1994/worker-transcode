package transcode

import (
	"context"
	goerrors "errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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

// Run executes one claimed transcode job, then finalizes the per-process log.
func Run(jobCtx context.Context, job *models.VideoProcess) error {
	err := run(jobCtx, job)
	finalizeProcessLog(jobCtx, job, err)
	return err
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

	// ─── STEP 1: DOWNLOAD original จาก storage-node ───────────
	originalMedia, err := findOriginalMedia(ctx, fileID)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	sourceStorage, err := models.StorageModel.FindByID(ctx, derefStr(originalMedia.StorageID))
	if err != nil {
		return fmt.Errorf("prepare: source storage not found: %w", err)
	}

	originalPath := filepath.Join(workDir, enums.FileNameOriginal)
	if cachedOriginalValid(originalPath, originalMedia) {
		utils.LogMain("📥 [%s] Original already cached — skipping download", slug)
		completeStep(ctx, job.ID, "download")
		updateOverallPercent(ctx, job.ID, 10)
	} else {
		os.Remove(originalPath)
		startStep(ctx, job.ID, "download")

		var sourceURL string
		if sourceStorage.Type == enums.StorageTypeS3 {
			sourceURL, err = sourceStorage.GetOriginObjectPathURL(originalMedia.ObjectPath())
			if err != nil {
				return fmt.Errorf("download: resolve S3 origin: %w", err)
			}
		} else {
			hostPort := sourceStorage.GetHostPort()
			if hostPort == "" {
				return fmt.Errorf("download: source storage has no host")
			}
			sourceURL = fmt.Sprintf("http://%s/%s.mp4", hostPort, originalMedia.Slug)
		}
		utils.LogMain("📥 [%s] Downloading %s", slug, sourceURL)

		write := stepThrottle(1)
		logProgress := pctLogger64(slug, "download")
		if err := downloader.DownloadURL(ctx, sourceURL, originalPath, func(done, total int64) {
			logProgress(done, total)
			if total > 0 {
				pct := float64(done) / float64(total) * 100
				if write(pct) {
					updateStepAndOverall(ctx, job.ID, "download", pct, pct*0.10)
				}
			}
		}); err != nil {
			return fmt.Errorf("download: %w", err)
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
	resolveTemp := func() (*models.Storage, error) {
		if tempS3 != nil {
			return tempS3, nil
		}
		storage, resolveErr := resolveS3TempStorage(ctx)
		if resolveErr != nil {
			return nil, resolveErr
		}
		tempS3 = storage
		return tempS3, nil
	}
	if permanentS3 == nil {
		if _, err := resolveTemp(); err != nil {
			return fmt.Errorf("no permanent S3 or S3 temp available: %v: %w", err, queue.ErrJobRequeue)
		}
	}

	// ─── STEP 4: ENCODE + UPLOAD ต่อ resolution ───────────────
	// Progress: 10 (download) + 85 (encode+upload เฉลี่ยตาม res) + 5 (finish)
	perRes := 85.0 / float64(len(pending))
	highest := 0

	for idx, res := range pending {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}

		fileName := enums.ResolutionToFileName[res]
		outputPath := filepath.Join(workDir, fileName)
		shortTarget := enums.ResolutionToShortSide[res]
		targetW, targetH := transcoder.GetScaleDimensions(videoInfo.Width, videoInfo.Height, shortTarget)
		base := 10.0 + float64(idx)*perRes

		// ── encode (retry resume skips only a finalized, full-duration MP4) ──
		encodeStep := fmt.Sprintf("encode_%s", res)
		if info, statErr := os.Stat(outputPath); statErr == nil && info.Size() > 0 {
			validateErr := transcoder.ValidateEncodedOutput(outputPath, videoInfo.DurationF)
			if validateErr == nil && config.AppConfig.MediaLayout == "separated" {
				validateErr = transcoder.ValidateVideoOnlyOutput(outputPath)
			}
			if validateErr == nil {
				utils.LogMain("⏭️  [%s] %sp already encoded and verified (retry resume) — skipping encode", slug, res)
				completeStep(ctx, job.ID, encodeStep)
			} else {
				utils.LogMain("⚠️  [%s] Removing incomplete %sp retry output: %v", slug, res, validateErr)
				if removeErr := os.Remove(outputPath); removeErr != nil && !os.IsNotExist(removeErr) {
					return fmt.Errorf("remove incomplete %sp output: %w", res, removeErr)
				}
				if err := encodeResolution(ctx, job, slug, res, encodeStep, originalPath, outputPath,
					targetW, targetH, videoInfo, int(shortSide), base, perRes); err != nil {
					return err
				}
			}
		} else {
			if err := encodeResolution(ctx, job, slug, res, encodeStep, originalPath, outputPath,
				targetW, targetH, videoInfo, int(shortSide), base, perRes); err != nil {
				return err
			}
		}

		// ── upload → permanent S3 media; fallback S3 temp + ingest ──
		uploadStep := fmt.Sprintf("upload_%s", res)
		startStep(ctx, job.ID, uploadStep)

		writeUp := stepThrottle(1)
		logProgress := pctLogger64(slug, uploadStep)
		onUploadProgress := func(done, total int64) {
			logProgress(done, total)
			if total > 0 {
				pct := float64(done) / float64(total) * 100
				if writeUp(pct) {
					updateStepAndOverall(ctx, job.ID, uploadStep, pct, base+perRes*0.7+pct/100*perRes*0.3)
				}
			}
		}

		size := transcoder.GetFileSize(outputPath)
		installedDirect := false
		if permanentS3 != nil {
			objectKey := fmt.Sprintf("%s/%s", fileID, fileName)
			utils.LogMain("📤 [%s] Uploading %s → permanent S3 %s", slug, fileName, permanentS3.Name)
			if uploadErr := uploader.UploadToS3(ctx, permanentS3, outputPath, objectKey, onUploadProgress); uploadErr != nil {
				if cause := context.Cause(ctx); cause != nil {
					return cause
				}
				utils.LogMain("⚠️  [%s] Permanent S3 upload failed: %v — falling back to Temp → Local", slug, uploadErr)
			} else {
				if err := createPermanentVideoMedia(
					ctx, fileID, slug, res, fileName, objectKey, permanentS3,
					size, targetW, targetH, videoInfo.DurationF,
				); err != nil {
					return fmt.Errorf("create permanent media %s: %w", fileName, err)
				}
				installedDirect = true

				// Cache invalidation is disabled; content-node/CDN TTL controls
				// freshness. Flip cacheInvalidationEnabled to restore this call.
				if cacheInvalidationEnabled {
					invalidatePlaylistCaches(ctx, fileID, slug)
				}
			}
		}

		if !installedDirect {
			tempStorage, tempErr := resolveTemp()
			if tempErr != nil {
				return fmt.Errorf("no S3 temp fallback for %s: %v: %w", fileName, tempErr, queue.ErrJobRequeue)
			}
			objectKey := fmt.Sprintf("%s/%s_%s", time.Now().Format("2006-01-02"), fileID, fileName)
			utils.LogMain("📤 [%s] Uploading %s → S3 temp %s", slug, fileName, tempStorage.Name)
			if err := uploader.UploadToS3(ctx, tempStorage, outputPath, objectKey, onUploadProgress); err != nil {
				return fmt.Errorf("upload %s to S3 temp: %w", fileName, err)
			}
			layout := config.AppConfig.MediaLayout
			metadata := &models.MediaMetadata{Size: size, Width: targetW, Height: targetH, Duration: videoInfo.DurationF, MediaLayout: &layout}
			if err := createTranscodeIngest(ctx, fileID, tempStorage, fileName, objectKey, res, metadata, size); err != nil {
				return fmt.Errorf("create ingest %s: %w", fileName, err)
			}
		}
		completeStep(ctx, job.ID, uploadStep)

		if resInt, convErr := strconv.Atoi(res); convErr == nil && resInt > highest {
			highest = resInt
		}

		// ลบ output ทันทีหลัง upload + media/cache เสร็จ คืนพื้นที่ก่อน
		// encode resolution ถัดไป; ถ้าลบไม่ได้ให้หยุดเพื่อกัน disk เต็ม
		if err := os.Remove(outputPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove uploaded output %s: %w", fileName, err)
		}
		utils.LogMain("🧹 [%s] Removed local %s after upload", slug, fileName)
		models.VideoProcessModel.UpdateByID(ctx, job.ID, bson.M{"$push": bson.M{"completed": res}})
		if installedDirect {
			utils.LogMain("✅ [%s] %sp ready on permanent S3", slug, res)
		} else {
			utils.LogMain("✅ [%s] %sp done (ingest created — worker-transfer will install)", slug, res)
		}
	}

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

func encodeResolution(ctx context.Context, job *models.VideoProcess, slug, res, encodeStep,
	originalPath, outputPath string, targetW, targetH int, videoInfo *transcoder.VideoInfo,
	shortSide int, base, perRes float64,
) error {
	startStep(ctx, job.ID, encodeStep)
	utils.LogMain("🎬 [%s] Encoding %sp (%dx%d)...", slug, res, targetW, targetH)

	write := stepThrottle(1)
	logEncodeProgress := pctLogger(slug, encodeStep)
	err := transcoder.EncodeResolution(ctx, originalPath, outputPath, targetW, targetH,
		videoInfo.DurationF, videoInfo.VideoBitrate, shortSide,
		config.AppConfig.MediaLayout != "separated", func(percent int) {
			logEncodeProgress(percent)
			if write(float64(percent)) {
				// encode กิน 70% ของช่วง res นี้ upload อีก 30%
				updateStepAndOverall(ctx, job.ID, encodeStep, float64(percent), base+float64(percent)/100*perRes*0.7)
			}
		})
	if err != nil {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		return fmt.Errorf("encode %sp: %w", res, err)
	}
	completeStep(ctx, job.ID, encodeStep)
	utils.LogMain("✅ [%s] Encoded %sp", slug, res)
	return nil
}

// cachedOriginalValid — ไฟล์ที่โหลดค้างไว้จากรอบก่อนใช้ได้ไหม (ขนาดตรง metadata)
func cachedOriginalValid(path string, media *models.Media) bool {
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		return false
	}
	var expected int64
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
	return expected == 0 || info.Size() == expected
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
