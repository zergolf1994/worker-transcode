package transcode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"worker-transcode/internal/core/enums"
	"worker-transcode/internal/core/utils"
	"worker-transcode/internal/db/models"
	"worker-transcode/internal/queue"
	"worker-transcode/internal/transcoder"
	"worker-transcode/internal/uploader"

	"go.mongodb.org/mongo-driver/bson"
)

// ensureSeparatedAudio backfills audio medias for legacy muxed originals.
// New separated originals have no audio streams and therefore only need their
// file metadata normalized. Tracks are keyed by sourceIndex so retries are
// idempotent and cannot create duplicate audio records.
func ensureSeparatedAudio(
	ctx context.Context,
	job *models.VideoProcess,
	fileID, slug, originalPath, workDir string,
) (int, error) {
	streams, err := transcoder.ProbeAudioStreams(originalPath)
	if err != nil {
		return 0, err
	}

	missing := make([]transcoder.AudioStream, 0, len(streams))
	for position, stream := range streams {
		fileName := fmt.Sprintf("audio_%d.m4a", position+1)
		if !hasAudioMedia(ctx, fileID, stream.Index) && !hasPendingIngest(ctx, fileID, fileName) {
			missing = append(missing, stream)
		}
	}

	var storage *models.Storage
	if len(missing) > 0 {
		storage, _ = resolveS3VideoStorage(ctx)
	}

	for position, stream := range streams {
		if hasAudioMedia(ctx, fileID, stream.Index) || hasPendingIngest(ctx, fileID, fmt.Sprintf("audio_%d.m4a", position+1)) {
			continue
		}
		trackNumber := position + 1
		fileName := fmt.Sprintf("audio_%d.m4a", trackNumber)
		outputPath := filepath.Join(workDir, fileName)
		extractStep := fmt.Sprintf("extract_audio_%d", trackNumber)
		uploadStep := fmt.Sprintf("upload_audio_%d", trackNumber)

		startStep(ctx, job.ID, extractStep)
		utils.LogMain("🎧 [%s] Extracting audio track %d (stream=%d codec=%s language=%s)...",
			slug, trackNumber, stream.Index, stream.Codec, stream.Language)
		logExtract := pctLogger(slug, extractStep)
		writeExtract := stepThrottle(1)
		if err := transcoder.ExtractAudioStream(ctx, originalPath, outputPath, stream, func(percent int) {
			logExtract(percent)
			if writeExtract(float64(percent)) {
				updateStepAndOverall(ctx, job.ID, extractStep, float64(percent), 10)
			}
		}); err != nil {
			return 0, fmt.Errorf("extract %s: %w", fileName, err)
		}
		completeStep(ctx, job.ID, extractStep)

		objectKey := fmt.Sprintf("%s/%s", fileID, fileName)
		startStep(ctx, job.ID, uploadStep)
		logUpload := pctLogger64(slug, uploadStep)
		writeUpload := stepThrottle(1)
		upload := func(target *models.Storage, key string) error {
			return uploader.UploadToS3(ctx, target, outputPath, key, func(done, total int64) {
				logUpload(done, total)
				if total > 0 {
					percent := float64(done) / float64(total) * 100
					if writeUpload(percent) {
						updateStepAndOverall(ctx, job.ID, uploadStep, percent, 10)
					}
				}
			})
		}

		info, err := os.Stat(outputPath)
		if err != nil {
			return 0, fmt.Errorf("stat %s: %w", fileName, err)
		}
		installedDirect := false
		if storage != nil {
			utils.LogMain("📤 [%s] Uploading %s → permanent S3 %s", slug, fileName, storage.Name)
			if uploadErr := upload(storage, objectKey); uploadErr == nil {
				if err := createPermanentAudioMedia(ctx, fileID, slug, fileName, objectKey, storage, info.Size(), stream); err != nil {
					return 0, fmt.Errorf("create audio media %s: %w", fileName, err)
				}
				installedDirect = true
			} else {
				utils.LogMain("⚠️  [%s] Permanent audio upload failed: %v — falling back to Temp → Local", slug, uploadErr)
			}
		}
		if !installedDirect {
			tempStorage, tempErr := resolveS3TempStorage(ctx)
			if tempErr != nil {
				return 0, fmt.Errorf("no Temp fallback for %s: %v: %w", fileName, tempErr, queue.ErrJobRequeue)
			}
			objectKey = fmt.Sprintf("%s/%s_%s", time.Now().Format("2006-01-02"), fileID, fileName)
			utils.LogMain("📤 [%s] Uploading %s → Temp %s", slug, fileName, tempStorage.Name)
			if err := upload(tempStorage, objectKey); err != nil {
				return 0, fmt.Errorf("upload audio fallback %s: %w", fileName, err)
			}
			if err := createAudioIngest(ctx, fileID, fileName, objectKey, tempStorage, info.Size(), stream); err != nil {
				return 0, fmt.Errorf("create audio ingest %s: %w", fileName, err)
			}
		}
		completeStep(ctx, job.ID, uploadStep)
		if err := os.Remove(outputPath); err != nil && !os.IsNotExist(err) {
			return 0, fmt.Errorf("remove uploaded audio %s: %w", fileName, err)
		}
		utils.LogMain("✅ [%s] Audio track %d uploaded", slug, trackNumber)
	}
	return len(streams), nil
}

func createAudioIngest(ctx context.Context, fileID, fileName, objectKey string, storage *models.Storage, size int64, stream transcoder.AudioStream) error {
	if hasPendingIngest(ctx, fileID, fileName) {
		return nil
	}
	mimeType, mediaType, installTarget := "audio/mp4", enums.MediaTypeAudio, "local"
	sourceCodec, codec, language, title, layout := stream.Codec, "aac", stream.Language, stream.Title, "separated"
	channels, sampleRate, bitrate := 2, 48000, int64(192000)
	metadata := &models.MediaMetadata{Size: size, Duration: stream.Duration, SourceIndex: &stream.Index,
		SourceCodec: &sourceCodec, Codec: &codec, Language: &language, Title: &title,
		IsDefault: &stream.Default, IsForced: &stream.Forced, Channels: &channels,
		SampleRate: &sampleRate, Bitrate: &bitrate, MediaLayout: &layout}
	now := time.Now()
	ingest := models.Ingest{ID: newUUID(), FileID: &fileID, StorageID: &storage.ID, FileName: fileName,
		Status: "completed", Size: size, MimeType: &mimeType, Path: &objectKey,
		SourceType: enums.IngestSourceTypeProcessed, MediaType: &mediaType,
		MediaMetadata: metadata, InstallTarget: &installTarget, CreatedAt: now, UpdatedAt: now}
	_, err := models.IngestModel.Create(ctx, &ingest)
	return err
}

func hasAudioMedia(ctx context.Context, fileID string, sourceIndex int) bool {
	count, err := models.MediaModel.CountDocuments(ctx, bson.M{
		"fileId": fileID, "type": enums.MediaTypeAudio,
		"metadata.sourceIndex": sourceIndex,
		"deletedAt":            bson.M{"$exists": false},
	})
	return err == nil && count > 0
}

// rejectLegacyMuxedRenditions prevents a single file from exposing a mixed
// set where some resolutions contain embedded audio and newer ones do not.
// Fresh files and retries from this implementation are safe because every
// generated rendition records metadata.mediaLayout=separated.
func rejectLegacyMuxedRenditions(ctx context.Context, fileID string) error {
	count, err := models.MediaModel.CountDocuments(ctx, bson.M{
		"fileId":               fileID,
		"type":                 enums.MediaTypeVideo,
		"resolution":           bson.M{"$in": enums.TranscodedResolutions},
		"metadata.mediaLayout": bson.M{"$ne": "separated"},
		"deletedAt":            bson.M{"$exists": false},
	})
	if err != nil {
		return fmt.Errorf("check existing rendition layout: %w", err)
	}
	if count > 0 {
		return fmt.Errorf("file has %d legacy muxed rendition(s); explicit media migration is required before separated transcode", count)
	}
	return nil
}

func createPermanentAudioMedia(
	ctx context.Context,
	fileID, slug, fileName, objectKey string,
	storage *models.Storage,
	size int64,
	stream transcoder.AudioStream,
) error {
	if hasAudioMedia(ctx, fileID, stream.Index) {
		return nil
	}

	now := time.Now()
	mimeType := "audio/mp4"
	sourceCodec := stream.Codec
	codec := "aac"
	language := stream.Language
	title := stream.Title
	channels, sampleRate := 2, 48000
	bitrate := int64(192000)
	mediaLayout := "separated"
	media := models.Media{
		ID: newUUID(), Type: enums.MediaTypeAudio,
		FileName: &fileName, MimeType: &mimeType, StorageID: &storage.ID,
		Slug: utils.RandomString(11, true), Path: &objectKey, FileID: &fileID,
		Metadata: &models.MediaMetadata{
			Size: size, Duration: stream.Duration, SourceIndex: &stream.Index,
			SourceCodec: &sourceCodec, Codec: &codec, Language: &language,
			Title: &title, IsDefault: &stream.Default, IsForced: &stream.Forced,
			Channels: &channels, SampleRate: &sampleRate, Bitrate: &bitrate,
			MediaLayout: &mediaLayout,
		},
		CreatedAt: now, UpdatedAt: now,
	}
	if _, err := models.MediaModel.Create(ctx, &media); err != nil {
		return err
	}
	cloneMediaToClonedFiles(ctx, fileID, media, slug)
	return nil
}

func setSeparatedFileMetadata(ctx context.Context, fileID string, audioTrackCount int) {
	update := bson.M{"$set": bson.M{
		"metadata.mediaLayout":     "separated",
		"metadata.audioTrackCount": audioTrackCount,
	}}
	models.FileModel.UpdateByID(ctx, fileID, update)
	models.FileModel.UpdateMany(ctx, bson.M{
		"clonedFrom":         fileID,
		"type":               enums.FileTypeVideo,
		"metadata.trashedAt": bson.M{"$exists": false},
		"metadata.deletedAt": bson.M{"$exists": false},
	}, update)
}
