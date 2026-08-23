package transcode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"worker-transcode/internal/config"
	"worker-transcode/internal/core/enums"
	"worker-transcode/internal/core/utils"
	"worker-transcode/internal/db/models"
	"worker-transcode/internal/queue"
	"worker-transcode/internal/transcoder"
	"worker-transcode/internal/uploader"

	"go.mongodb.org/mongo-driver/bson"
)

type renditionTask struct {
	resolution string
	fileName   string
	outputPath string
	width      int
	height     int
}

func makeRenditionTasks(pending []string, workDir string, videoInfo *transcoder.VideoInfo) []renditionTask {
	tasks := make([]renditionTask, 0, len(pending))
	for _, resolution := range pending {
		fileName := enums.ResolutionToFileName[resolution]
		width, height := transcoder.GetScaleDimensions(
			videoInfo.Width,
			videoInfo.Height,
			enums.ResolutionToShortSide[resolution],
		)
		tasks = append(tasks, renditionTask{
			resolution: resolution, fileName: fileName,
			outputPath: filepath.Join(workDir, fileName), width: width, height: height,
		})
	}
	return tasks
}

// adaptiveProgress keeps overallPercent correct while several encode/upload
// steps advance concurrently. Database writes remain serialized so a slower
// update cannot overwrite a newer overall percentage.
type adaptiveProgress struct {
	mu        sync.Mutex
	processID string
	perRes    float64
	encode    map[string]float64
	upload    map[string]float64
}

func newAdaptiveProgress(processID string, tasks []renditionTask) *adaptiveProgress {
	return &adaptiveProgress{
		processID: processID,
		perRes:    85.0 / float64(len(tasks)),
		encode:    make(map[string]float64, len(tasks)),
		upload:    make(map[string]float64, len(tasks)),
	}
}

func (p *adaptiveProgress) overallLocked() float64 {
	overall := 10.0
	for resolution, encodePercent := range p.encode {
		overall += p.perRes * 0.7 * encodePercent / 100
		overall += p.perRes * 0.3 * p.upload[resolution] / 100
	}
	if overall > 95 {
		return 95
	}
	return overall
}

func (p *adaptiveProgress) startEncode(ctx context.Context, tasks []renditionTask) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	set := bson.M{}
	for _, task := range tasks {
		p.encode[task.resolution] = 0
		step := "encode_" + task.resolution
		set[fmt.Sprintf("timeline.%s.status", step)] = enums.StepStatusProcessing
		set[fmt.Sprintf("timeline.%s.percent", step)] = 0
		set[fmt.Sprintf("timeline.%s.startedAt", step)] = now
	}
	set["overallPercent"] = p.overallLocked()
	models.VideoProcessModel.UpdateByID(ctx, p.processID, bson.M{"$set": set})
}

func (p *adaptiveProgress) updateEncode(ctx context.Context, tasks []renditionTask, percent float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	set := bson.M{}
	for _, task := range tasks {
		if percent < p.encode[task.resolution] {
			continue
		}
		p.encode[task.resolution] = percent
		step := "encode_" + task.resolution
		set[fmt.Sprintf("timeline.%s.status", step)] = enums.StepStatusProcessing
		set[fmt.Sprintf("timeline.%s.percent", step)] = percent
	}
	set["overallPercent"] = p.overallLocked()
	models.VideoProcessModel.UpdateByID(ctx, p.processID, bson.M{"$set": set})
}

func (p *adaptiveProgress) completeEncode(ctx context.Context, task renditionTask) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.encode[task.resolution] = 100
	step := "encode_" + task.resolution
	models.VideoProcessModel.UpdateByID(ctx, p.processID, bson.M{"$set": bson.M{
		fmt.Sprintf("timeline.%s.status", step):  enums.StepStatusCompleted,
		fmt.Sprintf("timeline.%s.percent", step): 100,
		fmt.Sprintf("timeline.%s.endedAt", step): time.Now(),
		"overallPercent":                         p.overallLocked(),
	}})
}

func (p *adaptiveProgress) startUpload(ctx context.Context, task renditionTask) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.upload[task.resolution] = 0
	step := "upload_" + task.resolution
	models.VideoProcessModel.UpdateByID(ctx, p.processID, bson.M{"$set": bson.M{
		fmt.Sprintf("timeline.%s.status", step):    enums.StepStatusProcessing,
		fmt.Sprintf("timeline.%s.percent", step):   0,
		fmt.Sprintf("timeline.%s.startedAt", step): time.Now(),
		"overallPercent": p.overallLocked(),
	}})
}

func (p *adaptiveProgress) updateUpload(ctx context.Context, task renditionTask, percent float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if percent < p.upload[task.resolution] {
		return
	}
	p.upload[task.resolution] = percent
	step := "upload_" + task.resolution
	models.VideoProcessModel.UpdateByID(ctx, p.processID, bson.M{"$set": bson.M{
		fmt.Sprintf("timeline.%s.status", step):  enums.StepStatusProcessing,
		fmt.Sprintf("timeline.%s.percent", step): percent,
		"overallPercent":                         p.overallLocked(),
	}})
}

func (p *adaptiveProgress) completeUpload(ctx context.Context, task renditionTask) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.upload[task.resolution] = 100
	step := "upload_" + task.resolution
	models.VideoProcessModel.UpdateByID(ctx, p.processID, bson.M{"$set": bson.M{
		fmt.Sprintf("timeline.%s.status", step):  enums.StepStatusCompleted,
		fmt.Sprintf("timeline.%s.percent", step): 100,
		fmt.Sprintf("timeline.%s.endedAt", step): time.Now(),
		"overallPercent":                         p.overallLocked(),
	}})
}

type uploadTargets struct {
	permanent *models.Storage
	temp      *models.Storage
	tempMu    sync.Mutex
}

func (t *uploadTargets) resolveTemp(ctx context.Context) (*models.Storage, error) {
	t.tempMu.Lock()
	defer t.tempMu.Unlock()
	if t.temp != nil {
		return t.temp, nil
	}
	storage, err := resolveS3TempStorage(ctx)
	if err != nil {
		return nil, err
	}
	t.temp = storage
	return t.temp, nil
}

func choosePipeline(mode, encoder string, durationSeconds float64, tasks []renditionTask, fanoutMaxMinutes int) string {
	if encoder == transcoder.EncoderCPU || mode == "sequential" {
		return "sequential"
	}
	if mode == "fanout" {
		return "fanout"
	}
	if durationSeconds <= float64(fanoutMaxMinutes)*60 {
		return "fanout"
	}
	has360, hasHigh := false, false
	for _, task := range tasks {
		switch task.resolution {
		case "360":
			has360 = true
		case "720", "1080":
			hasHigh = true
		}
	}
	if has360 && hasHigh {
		return "priority360"
	}
	return "fanout"
}

func shouldOverlapUploads(mode string, enabled bool) bool {
	return enabled && mode != "sequential"
}

func runAdaptivePipeline(
	ctx context.Context,
	job *models.VideoProcess,
	fileID, slug, originalPath string,
	videoInfo *transcoder.VideoInfo,
	srcShortSide int,
	tasks []renditionTask,
	targets *uploadTargets,
) error {
	progress := newAdaptiveProgress(job.ID, tasks)
	encoder := transcoder.DetectEncoder()
	pipeline := choosePipeline(
		config.AppConfig.TranscodePipelineMode,
		encoder,
		videoInfo.DurationF,
		tasks,
		config.AppConfig.FanoutMaxMinutes,
	)
	utils.LogMain("🧭 [%s] Pipeline: %s (encoder=%s duration=%.1fm threshold=%dm)",
		slug, pipeline, encoder, videoInfo.DurationF/60, config.AppConfig.FanoutMaxMinutes)

	switch pipeline {
	case "fanout":
		fellBackToCPU, err := ensureEncodedFanout(
			ctx, job, slug, originalPath, videoInfo, srcShortSide, tasks, progress, encoder,
		)
		if err != nil {
			return err
		}
		if fellBackToCPU {
			if shouldOverlapUploads(config.AppConfig.TranscodePipelineMode, config.AppConfig.TranscodeUploadOverlap) {
				return runPipelinedSequential(
					ctx, job, fileID, slug, originalPath, videoInfo, srcShortSide,
					tasks, targets, progress, nil,
				)
			}
			if err := ensureEncodedSequential(ctx, job, slug, originalPath, videoInfo, srcShortSide, tasks, progress); err != nil {
				return err
			}
		}
		return uploadRenditionsConcurrent(ctx, job, fileID, slug, videoInfo, tasks, targets, progress)

	case "priority360":
		var priority renditionTask
		rest := make([]renditionTask, 0, len(tasks)-1)
		for _, task := range tasks {
			if task.resolution == "360" {
				priority = task
			} else {
				rest = append(rest, task)
			}
		}
		if err := ensureEncodedOne(ctx, job, slug, originalPath, videoInfo, srcShortSide, priority, progress); err != nil {
			return err
		}

		uploadDone := make(chan error, 1)
		go func() {
			uploadDone <- uploadRendition(ctx, job, fileID, slug, videoInfo, priority, targets, progress)
		}()

		encoder = transcoder.DetectEncoder()
		if encoder == transcoder.EncoderCPU && shouldOverlapUploads(
			config.AppConfig.TranscodePipelineMode, config.AppConfig.TranscodeUploadOverlap,
		) {
			// Encode the first remaining rendition while 360p uploads, then keep
			// one CPU encoder busy while bounded uploads run in the background.
			return runPipelinedSequential(
				ctx, job, fileID, slug, originalPath, videoInfo, srcShortSide,
				rest, targets, progress, uploadDone,
			)
		}
		var encodeErr error
		if encoder == transcoder.EncoderCPU {
			encodeErr = ensureEncodedSequential(ctx, job, slug, originalPath, videoInfo, srcShortSide, rest, progress)
		} else {
			var fellBackToCPU bool
			fellBackToCPU, encodeErr = ensureEncodedFanout(
				ctx, job, slug, originalPath, videoInfo, srcShortSide, rest, progress, encoder,
			)
			if encodeErr == nil && fellBackToCPU {
				if shouldOverlapUploads(config.AppConfig.TranscodePipelineMode, config.AppConfig.TranscodeUploadOverlap) {
					return runPipelinedSequential(
						ctx, job, fileID, slug, originalPath, videoInfo, srcShortSide,
						rest, targets, progress, uploadDone,
					)
				}
				encodeErr = ensureEncodedSequential(ctx, job, slug, originalPath, videoInfo, srcShortSide, rest, progress)
			}
		}
		uploadErr := <-uploadDone
		if encodeErr != nil {
			return encodeErr
		}
		if uploadErr != nil {
			return uploadErr
		}
		// Do not publish higher renditions before 360p is durable/playable.
		return uploadRenditionsConcurrent(ctx, job, fileID, slug, videoInfo, rest, targets, progress)

	default:
		if shouldOverlapUploads(config.AppConfig.TranscodePipelineMode, config.AppConfig.TranscodeUploadOverlap) {
			return runPipelinedSequential(
				ctx, job, fileID, slug, originalPath, videoInfo, srcShortSide,
				tasks, targets, progress, nil,
			)
		}
		for _, task := range tasks {
			if err := ensureEncodedOne(ctx, job, slug, originalPath, videoInfo, srcShortSide, task, progress); err != nil {
				return err
			}
			if err := uploadRendition(ctx, job, fileID, slug, videoInfo, task, targets, progress); err != nil {
				return err
			}
		}
		return nil
	}
}

// runPipelinedSequential keeps exactly one encoder active while completed
// outputs upload in the background. The semaphore provides backpressure so a
// slow S3 destination cannot accumulate an unbounded number of local files.
// When firstUploadGate is non-nil, the first higher-quality upload waits until
// the priority 360p upload has been published successfully.
func runPipelinedSequential(
	ctx context.Context,
	job *models.VideoProcess,
	fileID, slug, originalPath string,
	videoInfo *transcoder.VideoInfo,
	srcShortSide int,
	tasks []renditionTask,
	targets *uploadTargets,
	progress *adaptiveProgress,
	firstUploadGate <-chan error,
) error {
	maxUploads := config.AppConfig.MaxParallelUploads
	if maxUploads < 1 {
		maxUploads = 1
	}
	utils.LogMain("🔀 [%s] Sequential encode with upload overlap (max uploads=%d)", slug, maxUploads)

	semaphore := make(chan struct{}, maxUploads)
	errCh := make(chan error, len(tasks))
	var wg sync.WaitGroup
	gateWaited := firstUploadGate == nil

	for _, task := range tasks {
		if err := ensureEncodedOne(ctx, job, slug, originalPath, videoInfo, srcShortSide, task, progress); err != nil {
			if !gateWaited {
				<-firstUploadGate
				gateWaited = true
			}
			wg.Wait()
			return err
		}

		if !gateWaited {
			if err := <-firstUploadGate; err != nil {
				wg.Wait()
				return err
			}
			gateWaited = true
		}

		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return context.Cause(ctx)
		}
		task := task
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-semaphore }()
			if err := uploadRendition(ctx, job, fileID, slug, videoInfo, task, targets, progress); err != nil {
				errCh <- err
			}
		}()
	}

	if !gateWaited {
		if err := <-firstUploadGate; err != nil {
			wg.Wait()
			return err
		}
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

func ensureEncodedSequential(
	ctx context.Context,
	job *models.VideoProcess,
	slug, originalPath string,
	videoInfo *transcoder.VideoInfo,
	srcShortSide int,
	tasks []renditionTask,
	progress *adaptiveProgress,
) error {
	for _, task := range tasks {
		if err := ensureEncodedOne(ctx, job, slug, originalPath, videoInfo, srcShortSide, task, progress); err != nil {
			return err
		}
	}
	return nil
}

func validRetryOutput(task renditionTask, videoInfo *transcoder.VideoInfo) error {
	info, err := os.Stat(task.outputPath)
	if err != nil {
		return err
	}
	if info.Size() <= 0 {
		return fmt.Errorf("output is empty")
	}
	if err := transcoder.ValidateEncodedOutput(task.outputPath, videoInfo.DurationF); err != nil {
		return err
	}
	if config.AppConfig.MediaLayout == "separated" {
		return transcoder.ValidateVideoOnlyOutput(task.outputPath)
	}
	return nil
}

func prepareEncodeTask(slug string, task renditionTask, videoInfo *transcoder.VideoInfo) (bool, error) {
	if err := validRetryOutput(task, videoInfo); err == nil {
		utils.LogMain("⏭️  [%s] %sp already encoded and verified (retry resume)", slug, task.resolution)
		return false, nil
	} else if !os.IsNotExist(err) {
		utils.LogMain("⚠️  [%s] Removing incomplete %sp retry output: %v", slug, task.resolution, err)
	}
	if err := os.Remove(task.outputPath); err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("remove incomplete %sp output: %w", task.resolution, err)
	}
	return true, nil
}

func ensureEncodedOne(
	ctx context.Context,
	job *models.VideoProcess,
	slug, originalPath string,
	videoInfo *transcoder.VideoInfo,
	srcShortSide int,
	task renditionTask,
	progress *adaptiveProgress,
) error {
	needsEncode, err := prepareEncodeTask(slug, task, videoInfo)
	if err != nil {
		return err
	}
	if !needsEncode {
		progress.completeEncode(ctx, task)
		return nil
	}

	progress.startEncode(ctx, []renditionTask{task})
	utils.LogMain("🎬 [%s] Encoding %sp (%dx%d)...", slug, task.resolution, task.width, task.height)
	write := stepThrottle(1)
	logProgress := pctLogger(slug, "encode_"+task.resolution)
	err = transcoder.EncodeResolution(
		ctx, originalPath, task.outputPath, task.width, task.height,
		videoInfo.DurationF, videoInfo.VideoBitrate, srcShortSide,
		config.AppConfig.MediaLayout != "separated",
		func(percent int) {
			logProgress(percent)
			if write(float64(percent)) {
				progress.updateEncode(ctx, []renditionTask{task}, float64(percent))
			}
		},
	)
	if err != nil {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		return fmt.Errorf("encode %sp: %w", task.resolution, err)
	}
	progress.completeEncode(ctx, task)
	utils.LogMain("✅ [%s] Encoded %sp", slug, task.resolution)
	return nil
}

func ensureEncodedFanout(
	ctx context.Context,
	job *models.VideoProcess,
	slug, originalPath string,
	videoInfo *transcoder.VideoInfo,
	srcShortSide int,
	tasks []renditionTask,
	progress *adaptiveProgress,
	encoder string,
) (bool, error) {
	if len(tasks) == 0 {
		return false, nil
	}
	needsEncode := make([]renditionTask, 0, len(tasks))
	for _, task := range tasks {
		needs, err := prepareEncodeTask(slug, task, videoInfo)
		if err != nil {
			return false, err
		}
		if needs {
			needsEncode = append(needsEncode, task)
		} else {
			progress.completeEncode(ctx, task)
		}
	}
	if len(needsEncode) == 0 {
		return false, nil
	}
	if encoder == transcoder.EncoderCPU || len(needsEncode) == 1 {
		return false, ensureEncodedSequential(ctx, job, slug, originalPath, videoInfo, srcShortSide, needsEncode, progress)
	}

	progress.startEncode(ctx, needsEncode)
	loggers := make(map[string]func(int), len(needsEncode))
	for _, task := range needsEncode {
		loggers[task.resolution] = pctLogger(slug, "encode_"+task.resolution)
	}
	write := stepThrottle(1)
	outputs := make([]transcoder.Rendition, 0, len(needsEncode))
	for _, task := range needsEncode {
		outputs = append(outputs, transcoder.Rendition{
			Resolution: task.resolution,
			OutputPath: task.outputPath,
			Width:      task.width,
			Height:     task.height,
		})
	}

	err := transcoder.EncodeResolutions(
		ctx, originalPath, outputs, encoder,
		videoInfo.DurationF, videoInfo.VideoBitrate, srcShortSide,
		config.AppConfig.MediaLayout != "separated",
		func(percent int) {
			for _, logger := range loggers {
				logger(percent)
			}
			if write(float64(percent)) {
				progress.updateEncode(ctx, needsEncode, float64(percent))
			}
		},
	)
	if err == nil {
		for _, task := range needsEncode {
			progress.completeEncode(ctx, task)
		}
		return false, nil
	}
	if ctx.Err() != nil {
		return false, context.Cause(ctx)
	}

	utils.LogMain("⚠️  [%s] GPU fanout failed: %v — falling back to CPU sequential", slug, err)
	for _, task := range needsEncode {
		if validateErr := validRetryOutput(task, videoInfo); validateErr == nil {
			utils.LogMain("✅ [%s] Keeping completed %sp from failed fanout", slug, task.resolution)
			progress.completeEncode(ctx, task)
			continue
		}
		if removeErr := os.Remove(task.outputPath); removeErr != nil && !os.IsNotExist(removeErr) {
			return false, fmt.Errorf("remove failed fanout output %sp: %w", task.resolution, removeErr)
		}
	}
	transcoder.ForceCPU()
	return true, nil
}

func uploadRenditionsConcurrent(
	ctx context.Context,
	job *models.VideoProcess,
	fileID, slug string,
	videoInfo *transcoder.VideoInfo,
	tasks []renditionTask,
	targets *uploadTargets,
	progress *adaptiveProgress,
) error {
	if len(tasks) == 0 {
		return nil
	}
	errCh := make(chan error, len(tasks))
	var wg sync.WaitGroup
	for _, task := range tasks {
		task := task
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := uploadRendition(ctx, job, fileID, slug, videoInfo, task, targets, progress); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

func uploadRendition(
	ctx context.Context,
	job *models.VideoProcess,
	fileID, slug string,
	videoInfo *transcoder.VideoInfo,
	task renditionTask,
	targets *uploadTargets,
	progress *adaptiveProgress,
) error {
	progress.startUpload(ctx, task)
	uploadStep := "upload_" + task.resolution
	write := stepThrottle(1)
	logProgress := pctLogger64(slug, uploadStep)
	onProgress := func(done, total int64) {
		logProgress(done, total)
		if total > 0 {
			percent := float64(done) / float64(total) * 100
			if write(percent) {
				progress.updateUpload(ctx, task, percent)
			}
		}
	}

	size := transcoder.GetFileSize(task.outputPath)
	installedDirect := false
	if targets.permanent != nil {
		objectKey := fmt.Sprintf("%s/%s", fileID, task.fileName)
		utils.LogMain("📤 [%s] Uploading %s → permanent S3 %s", slug, task.fileName, targets.permanent.Name)
		if uploadErr := uploader.UploadToS3(ctx, targets.permanent, task.outputPath, objectKey, onProgress); uploadErr != nil {
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			utils.LogMain("⚠️  [%s] Permanent S3 upload failed: %v — falling back to Temp → Local", slug, uploadErr)
			// Multipart upload may have reported 100% before CompleteMultipartUpload
			// failed. Reset both the DB step and callback throttles for Temp retry.
			progress.startUpload(ctx, task)
			write = stepThrottle(1)
			logProgress = pctLogger64(slug, uploadStep)
		} else {
			if err := createPermanentVideoMedia(
				ctx, fileID, slug, task.resolution, task.fileName, objectKey, targets.permanent,
				size, task.width, task.height, videoInfo.DurationF,
			); err != nil {
				return fmt.Errorf("create permanent media %s: %w", task.fileName, err)
			}
			installedDirect = true
			if cacheInvalidationEnabled {
				invalidatePlaylistCaches(ctx, fileID, slug)
			}
		}
	}

	if !installedDirect {
		tempStorage, err := targets.resolveTemp(ctx)
		if err != nil {
			return fmt.Errorf("no S3 temp fallback for %s: %v: %w", task.fileName, err, queue.ErrJobRequeue)
		}
		objectKey := fmt.Sprintf("%s/%s_%s", time.Now().Format("2006-01-02"), fileID, task.fileName)
		utils.LogMain("📤 [%s] Uploading %s → S3 temp %s", slug, task.fileName, tempStorage.Name)
		if err := uploader.UploadToS3(ctx, tempStorage, task.outputPath, objectKey, onProgress); err != nil {
			return fmt.Errorf("upload %s to S3 temp: %w", task.fileName, err)
		}
		layout := config.AppConfig.MediaLayout
		metadata := &models.MediaMetadata{
			Size: size, Width: task.width, Height: task.height,
			Duration: videoInfo.DurationF, MediaLayout: &layout,
		}
		if err := createTranscodeIngest(
			ctx, fileID, tempStorage, task.fileName, objectKey,
			task.resolution, metadata, size,
		); err != nil {
			return fmt.Errorf("create ingest %s: %w", task.fileName, err)
		}
	}

	progress.completeUpload(ctx, task)
	if err := os.Remove(task.outputPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove uploaded output %s: %w", task.fileName, err)
	}
	utils.LogMain("🧹 [%s] Removed local %s after upload", slug, task.fileName)
	models.VideoProcessModel.UpdateByID(ctx, job.ID, bson.M{"$push": bson.M{"completed": task.resolution}})
	if installedDirect {
		utils.LogMain("✅ [%s] %sp ready on permanent S3", slug, task.resolution)
	} else {
		utils.LogMain("✅ [%s] %sp done (ingest created — worker-transfer will install)", slug, task.resolution)
	}
	return nil
}

func highestTaskResolution(tasks []renditionTask) int {
	highest := 0
	for _, task := range tasks {
		resolution, err := strconv.Atoi(task.resolution)
		if err == nil && resolution > highest {
			highest = resolution
		}
	}
	return highest
}
