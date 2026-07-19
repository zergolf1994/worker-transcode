package transcode

import (
	"context"
	"fmt"
	"log"
	"time"

	"worker-transcode/internal/core/enums"
	"worker-transcode/internal/db/models"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func newUUID() string { return uuid.New().String() }

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ─── Media / ingest lookups ──────────────────────────────────

// findOriginalMedia returns the original video media of the file.
func findOriginalMedia(ctx context.Context, fileID string) (*models.Media, error) {
	media, err := models.MediaModel.FindOne(ctx, bson.M{
		"fileId":     fileID,
		"type":       enums.MediaTypeVideo,
		"resolution": enums.ResolutionOriginal,
		"deletedAt":  bson.M{"$exists": false},
	})
	if err != nil {
		return nil, fmt.Errorf("original media not found for file %s", fileID)
	}
	return media, nil
}

// hasVideoMedia checks medias collection globally (any storage) for this resolution.
func hasVideoMedia(ctx context.Context, fileID, resolution string) bool {
	count, _ := models.MediaModel.CountDocuments(ctx, bson.M{
		"fileId":     fileID,
		"type":       enums.MediaTypeVideo,
		"resolution": resolution,
		"deletedAt":  bson.M{"$exists": false},
	})
	return count > 0
}

// hasPendingIngest — asset อัพขึ้น S3 แล้ว รอ worker-transfer ติดตั้ง
func hasPendingIngest(ctx context.Context, fileID, fileName string) bool {
	count, _ := models.IngestModel.CountDocuments(ctx, bson.M{
		"fileId":     fileID,
		"fileName":   fileName,
		"sourceType": enums.IngestSourceTypeProcessed,
		"deletedAt":  bson.M{"$exists": false},
	})
	return count > 0
}

// resolutionCovered — resolution นี้ไม่ต้อง encode แล้ว (media มี หรือ
// ingest ค้างรอ transfer อยู่)
func resolutionCovered(ctx context.Context, fileID, res string) (covered bool, reason string) {
	if hasVideoMedia(ctx, fileID, res) {
		return true, "media_exists"
	}
	if hasPendingIngest(ctx, fileID, enums.ResolutionToFileName[res]) {
		return true, "ingest_pending"
	}
	return false, ""
}

// createTranscodeIngest สร้าง ingest processed ให้ worker-transfer —
// key แบบมีวันที่เหมือน worker-download ({date}/{fileId}_{fileName})
// ingest.path เป็น source of truth ฝั่งอ่านทั้งหมด
func createTranscodeIngest(ctx context.Context, fileID string, s3Storage *models.Storage, fileName, objectKey string, size int64) error {
	if hasPendingIngest(ctx, fileID, fileName) {
		return nil
	}
	now := time.Now()
	mimeType := "video/mp4"
	storageID := s3Storage.ID
	key := objectKey
	ingest := models.Ingest{
		ID:         newUUID(),
		FileID:     &fileID,
		StorageID:  &storageID,
		FileName:   fileName,
		Status:     "completed",
		Size:       size,
		MimeType:   &mimeType,
		Path:       &key,
		SourceType: enums.IngestSourceTypeProcessed,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if _, err := models.IngestModel.Create(ctx, &ingest); err != nil {
		return err
	}
	log.Printf("✅ Created ingest: fileId=%s fileName=%s path=%s", fileID, fileName, key)
	return nil
}

// ─── Clone propagation ───────────────────────────────────────

// updateClonedFilesHighest กระจาย metadata.highest ไปไฟล์ clonedFrom
func updateClonedFilesHighest(ctx context.Context, sourceFileID string, highest int, slug string) {
	result, _ := models.FileModel.UpdateMany(ctx, bson.M{
		"clonedFrom":         sourceFileID,
		"type":               enums.FileTypeVideo,
		"metadata.trashedAt": bson.M{"$exists": false},
		"metadata.deletedAt": bson.M{"$exists": false},
	}, bson.M{"$set": bson.M{
		"metadata.highest": highest,
	}})
	if result != nil && result.ModifiedCount > 0 {
		log.Printf("📋 [%s] Updated metadata.highest=%d on %d cloned file(s)", slug, highest, result.ModifiedCount)
	}
}

// ─── Settings ────────────────────────────────────────────────

// gpuEnabledFromSetting อ่าน transcode_config.gpuEnabled — default true
// (auto-detect จะ fallback CPU เองถ้าไม่มี GPU)
func gpuEnabledFromSetting(ctx context.Context) bool {
	setting, err := models.SettingModel.FindOne(ctx, bson.M{"name": enums.SettingTranscodeConfig})
	if err != nil {
		return true
	}
	cfg, ok := setting.Value.(bson.M)
	if !ok {
		switch v := setting.Value.(type) {
		case map[string]interface{}:
			cfg = bson.M(v)
		case bson.D:
			// default registry decode document เป็น bson.D ไม่ใช่ bson.M
			cfg = bson.M{}
			for _, e := range v {
				cfg[e.Key] = e.Value
			}
		default:
			return true
		}
	}
	if enabled, ok := cfg["gpuEnabled"].(bool); ok {
		return enabled
	}
	return true
}

// ─── S3 temp storage (upload + logs) ─────────────────────────

// resolveS3TempStorage finds an S3 storage that accepts ["temp", "video"].
func resolveS3TempStorage(ctx context.Context) (*models.Storage, error) {
	filter := bson.M{
		"enable":  true,
		"status":  enums.StorageStatusOnline,
		"type":    enums.StorageTypeS3,
		"accepts": bson.M{"$all": []string{enums.StorageAcceptTemp, enums.StorageAcceptVideo}},
	}
	storage, err := models.StorageModel.FindOne(ctx, filter, options.FindOne().SetSort(bson.M{"capacity.percentage": 1}))
	if err != nil {
		return nil, fmt.Errorf("no S3 temp storage available")
	}
	return storage, nil
}
