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
	for _, stream := range streams {
		if !hasAudioMedia(ctx, fileID, stream.Index) {
			missing = append(missing, stream)
		}
	}

	var storage *models.Storage
	if len(missing) > 0 {
		storage, err = resolveS3VideoStorage(ctx)
		if err != nil {
			// Audio is a permanent media asset. Sending it through the old
			// processed-video ingest path would make worker-transfer interpret it
			// as MP4 video, so wait for durable S3 instead.
			return 0, fmt.Errorf("no permanent S3 for %d missing audio track(s): %v: %w", len(missing), err, queue.ErrJobRequeue)
		}
	}

	for position, stream := range streams {
		if hasAudioMedia(ctx, fileID, stream.Index) {
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
		utils.LogMain("📤 [%s] Uploading %s → permanent S3 %s", slug, fileName, storage.Name)
		logUpload := pctLogger64(slug, uploadStep)
		writeUpload := stepThrottle(1)
		if err := uploader.UploadToS3(ctx, storage, outputPath, objectKey, func(done, total int64) {
			logUpload(done, total)
			if total > 0 {
				percent := float64(done) / float64(total) * 100
				if writeUpload(percent) {
					updateStepAndOverall(ctx, job.ID, uploadStep, percent, 10)
				}
			}
		}); err != nil {
			return 0, fmt.Errorf("upload %s: %w", fileName, err)
		}

		info, err := os.Stat(outputPath)
		if err != nil {
			return 0, fmt.Errorf("stat %s: %w", fileName, err)
		}
		if err := createPermanentAudioMedia(ctx, fileID, slug, fileName, objectKey, storage, info.Size(), stream); err != nil {
			return 0, fmt.Errorf("create audio media %s: %w", fileName, err)
		}
		completeStep(ctx, job.ID, uploadStep)
		if err := os.Remove(outputPath); err != nil && !os.IsNotExist(err) {
			return 0, fmt.Errorf("remove uploaded audio %s: %w", fileName, err)
		}
		utils.LogMain("✅ [%s] Audio track %d ready on permanent S3", slug, trackNumber)
	}

	audioCount, err := models.MediaModel.CountDocuments(ctx, bson.M{
		"fileId": fileID, "type": enums.MediaTypeAudio,
		"deletedAt": bson.M{"$exists": false},
	})
	if err != nil {
		return 0, fmt.Errorf("count audio medias: %w", err)
	}
	return int(audioCount), nil
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
