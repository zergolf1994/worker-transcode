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
// One job = one file: โหลด original จาก storage-node (HTTP) → probe →
// encode ทุก resolution ที่ต่ำกว่าต้นฉบับ (360 เสมอ, 480/720/1080 ตาม
// short side, YouTube-style 95% tolerance) → อัพ S3 temp + สร้าง ingest
// processed (key มีวันที่) → worker-transfer ติดตั้ง + สร้าง media เอง
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

		hostPort := sourceStorage.GetHostPort()
		if hostPort == "" {
			return fmt.Errorf("download: source storage has no host")
		}
		url := fmt.Sprintf("http://%s/%s.mp4", hostPort, originalMedia.Slug)
		utils.LogMain("📥 [%s] Downloading %s", slug, url)

		write := stepThrottle(5)
		if err := downloader.DownloadURL(ctx, url, originalPath, func(done, total int64) {
			pctLogger64(slug, "download")(done, total)
			if total > 0 {
				pct := float64(done) / float64(total) * 100
				if write(pct) {
					updateTimelineStep(ctx, job.ID, "download", pct)
					updateOverallPercent(ctx, job.ID, pct*0.10)
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

	s3Storage, err := resolveS3TempStorage(ctx)
	if err != nil {
		// ไม่มี S3 temp = ปัญหาสภาพแวดล้อม ไม่ใช่ความผิดของงาน — คืนคิว
		return fmt.Errorf("%v: %w", err, queue.ErrJobRequeue)
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

		// ── encode (ข้ามถ้าไฟล์ค้างจากรอบก่อนและขนาด > 0 — retry resume) ──
		encodeStep := fmt.Sprintf("encode_%s", res)
		if info, statErr := os.Stat(outputPath); statErr == nil && info.Size() > 0 {
			utils.LogMain("⏭️  [%s] %sp already encoded (retry resume) — skipping encode", slug, res)
			completeStep(ctx, job.ID, encodeStep)
		} else {
			startStep(ctx, job.ID, encodeStep)
			utils.LogMain("🎬 [%s] Encoding %sp (%dx%d)...", slug, res, targetW, targetH)

			write := stepThrottle(5)
			err := transcoder.EncodeResolution(ctx, originalPath, outputPath, targetW, targetH,
				videoInfo.DurationF, videoInfo.VideoBitrate, int(shortSide), func(percent int) {
					if write(float64(percent)) {
						updateTimelineStep(ctx, job.ID, encodeStep, float64(percent))
						// encode กิน 70% ของช่วง res นี้ upload อีก 30%
						updateOverallPercent(ctx, job.ID, base+float64(percent)/100*perRes*0.7)
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
		}

		// ── upload → S3 temp + ingest ──
		uploadStep := fmt.Sprintf("upload_%s", res)
		startStep(ctx, job.ID, uploadStep)

		// key แบบมีวันที่ เหมือน worker-download ({date}/{fileId}_{fileName})
		objectKey := fmt.Sprintf("%s/%s_%s", time.Now().Format("2006-01-02"), fileID, fileName)
		utils.LogMain("📤 [%s] Uploading %s → %s", slug, fileName, objectKey)

		writeUp := stepThrottle(5)
		if err := uploader.UploadToS3(ctx, s3Storage, outputPath, objectKey, func(done, total int64) {
			pctLogger64(slug, uploadStep)(done, total)
			if total > 0 {
				pct := float64(done) / float64(total) * 100
				if writeUp(pct) {
					updateTimelineStep(ctx, job.ID, uploadStep, pct)
					updateOverallPercent(ctx, job.ID, base+perRes*0.7+pct/100*perRes*0.3)
				}
			}
		}); err != nil {
			return fmt.Errorf("upload %s: %w", fileName, err)
		}

		size := transcoder.GetFileSize(outputPath)
		if err := createTranscodeIngest(ctx, fileID, s3Storage, fileName, objectKey, size); err != nil {
			return fmt.Errorf("create ingest %s: %w", fileName, err)
		}
		completeStep(ctx, job.ID, uploadStep)

		if resInt, convErr := strconv.Atoi(res); convErr == nil && resInt > highest {
			highest = resInt
		}

		// ลบ output ที่อัพเสร็จแล้ว — ประหยัดดิสก์ระหว่างทำ res ถัดไป
		os.Remove(outputPath)
		models.VideoProcessModel.UpdateByID(ctx, job.ID, bson.M{"$push": bson.M{"completed": res}})
		utils.LogMain("✅ [%s] %sp done (ingest created — worker-transfer will install)", slug, res)
	}

	// ─── STEP 5: FINISH — ประทับ metadata.highest ─────────────
	if h := highestCovered(ctx, fileID); h > highest {
		highest = h
	}
	setHighest(ctx, fileID, slug, highest)
	updateOverallPercent(ctx, job.ID, 100)

	success = true
	utils.LogMain("✅ [%s] TRANSCODE COMPLETE (%v)", slug, pending)
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
