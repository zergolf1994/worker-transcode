package transcode

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"worker-transcode/internal/cache"
	"worker-transcode/internal/config"
	"worker-transcode/internal/core/enums"
	"worker-transcode/internal/core/utils"
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

// createPermanentVideoMedia records a rendition uploaded directly to durable
// S3. path is persisted so cleanup can resolve the exact physical object and
// every clone shares the same path.
func createPermanentVideoMedia(
	ctx context.Context,
	fileID, slug, resolution, fileName, objectKey string,
	storage *models.Storage,
	size int64,
	width, height int,
	duration float64,
) error {
	existingCount, err := models.MediaModel.CountDocuments(ctx, bson.M{
		"fileId":     fileID,
		"type":       enums.MediaTypeVideo,
		"resolution": resolution,
		"deletedAt":  bson.M{"$exists": false},
	})
	if err != nil {
		return fmt.Errorf("check existing media: %w", err)
	}
	if existingCount > 0 {
		return nil
	}

	now := time.Now()
	mimeType := "video/mp4"
	mediaLayout := config.AppConfig.MediaLayout
	media := models.Media{
		ID:         newUUID(),
		Type:       enums.MediaTypeVideo,
		FileName:   &fileName,
		MimeType:   &mimeType,
		Resolution: &resolution,
		StorageID:  &storage.ID,
		Slug:       utils.RandomString(11, true),
		Path:       &objectKey,
		FileID:     &fileID,
		Metadata: &models.MediaMetadata{
			Size:        size,
			Width:       width,
			Height:      height,
			Duration:    duration,
			MediaLayout: &mediaLayout,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	if _, err := models.MediaModel.Create(ctx, &media); err != nil {
		return err
	}
	cloneMediaToClonedFiles(ctx, fileID, media, slug)
	return nil
}

func cloneMediaToClonedFiles(ctx context.Context, sourceFileID string, media models.Media, slug string) {
	cursor, err := models.FileModel.FindRaw(ctx, bson.M{
		"clonedFrom":         sourceFileID,
		"type":               enums.FileTypeVideo,
		"metadata.trashedAt": bson.M{"$exists": false},
		"metadata.deletedAt": bson.M{"$exists": false},
	})
	if err != nil {
		return
	}
	defer cursor.Close(ctx)

	for cursor.Next(ctx) {
		var clonedFile models.File
		if err := cursor.Decode(&clonedFile); err != nil {
			continue
		}

		filter := bson.M{
			"fileId":    clonedFile.ID,
			"type":      media.Type,
			"deletedAt": bson.M{"$exists": false},
		}
		if media.Resolution != nil {
			filter["resolution"] = *media.Resolution
		}
		if media.FileName != nil {
			filter["fileName"] = *media.FileName
		}
		if count, _ := models.MediaModel.CountDocuments(ctx, filter); count > 0 {
			continue
		}

		now := time.Now()
		clonedFrom := sourceFileID
		clonedMedia := media
		clonedMedia.ID = newUUID()
		clonedMedia.Slug = utils.RandomString(11, true)
		clonedMedia.FileID = &clonedFile.ID
		clonedMedia.ClonedFrom = &clonedFrom
		clonedMedia.Prewarm = nil
		clonedMedia.DeletedAt = nil
		clonedMedia.CreatedAt = now
		clonedMedia.UpdatedAt = now

		if _, err := models.MediaModel.Create(ctx, &clonedMedia); err != nil {
			log.Printf("⚠️  [%s] Failed to clone media to %s: %v", slug, clonedFile.ID, err)
			continue
		}
		log.Printf("📋 [%s] Cloned media → file %s", slug, clonedFile.ID)
	}
}

// Cache invalidation is intentionally disabled while content-node controls
// playlist freshness through CDN TTLs. Set this to true to restore Redis and
// Cloudflare invalidation without rebuilding the old call sites.
const cacheInvalidationEnabled = false

// invalidatePlaylistCaches is called once per transcode job after direct S3
// media creation. Redis must be cleared before Cloudflare so the first MISS
// cannot repopulate the CDN with a stale playlist.
func invalidatePlaylistCaches(ctx context.Context, fileID, slug string) {
	slugs := collectSlugs(ctx, fileID, slug)
	utils.LogMain("🧹 [%s] Invalidating playlist cache for %d file(s)...", slug, len(slugs))
	cache.Del(ctx, redisKeysFor(slugs)...)
	if err := purgePlaylistCache(ctx, slug, slugs); err != nil {
		utils.LogMain("⚠️  [%s] Playlist cache purge failed (ignored): %v", slug, err)
		return
	}
	utils.LogMain("✅ [%s] Playlist cache invalidation finished", slug)
}

func collectSlugs(ctx context.Context, fileID, slug string) []string {
	slugs := make([]string, 0, 1)
	if slug != "" {
		slugs = append(slugs, slug)
	}
	cursor, err := models.FileModel.FindRaw(ctx, bson.M{
		"clonedFrom":         fileID,
		"type":               enums.FileTypeVideo,
		"metadata.trashedAt": bson.M{"$exists": false},
		"metadata.deletedAt": bson.M{"$exists": false},
	}, options.Find().SetProjection(bson.M{"slug": 1}))
	if err != nil {
		return slugs
	}
	defer cursor.Close(ctx)
	for cursor.Next(ctx) {
		var clonedFile models.File
		if err := cursor.Decode(&clonedFile); err == nil && clonedFile.Slug != "" {
			slugs = append(slugs, clonedFile.Slug)
		}
	}
	return slugs
}

func redisKeysFor(slugs []string) []string {
	keys := make([]string, 0, len(slugs)*3)
	for _, slug := range slugs {
		keys = append(keys,
			"playlist_master:"+slug,
			"playlist_json:"+slug,
			"embed_resolve:"+slug,
		)
	}
	return keys
}

func resolveCfProfile(ctx context.Context, purpose string) utils.CloudflareConfig {
	binding, err := models.SettingModel.FindOne(ctx, bson.M{"name": enums.SettingDomainBindings})
	if err != nil {
		return utils.CloudflareConfig{}
	}
	bindingMap, ok := asBsonM(binding.Value)
	if !ok {
		return utils.CloudflareConfig{}
	}
	profileID, _ := bindingMap[purpose].(string)
	if profileID == "" {
		return utils.CloudflareConfig{}
	}

	setting, err := models.SettingModel.FindOne(ctx, bson.M{"name": enums.SettingDomainProfiles})
	if err != nil {
		return utils.CloudflareConfig{}
	}
	profiles, ok := setting.Value.(bson.A)
	if !ok {
		return utils.CloudflareConfig{}
	}
	for _, profile := range profiles {
		profileMap, ok := asBsonM(profile)
		if !ok || profileMap["_id"] != profileID {
			continue
		}
		zoneID, _ := profileMap["zone_id"].(string)
		apiToken, _ := profileMap["api_token"].(string)
		return utils.CloudflareConfig{ZoneID: zoneID, APIToken: apiToken}
	}
	return utils.CloudflareConfig{}
}

func asBsonM(value interface{}) (bson.M, bool) {
	switch typed := value.(type) {
	case bson.M:
		return typed, true
	case map[string]interface{}:
		return bson.M(typed), true
	case bson.D:
		out := bson.M{}
		for _, entry := range typed {
			out[entry.Key] = entry.Value
		}
		return out, true
	default:
		return nil, false
	}
}

// purgePlaylistCache purges only /<slug>/playlist.m3u8. Missing profile means
// Cloudflare invalidation is intentionally disabled.
func purgePlaylistCache(ctx context.Context, slug string, slugs []string) error {
	domainSetting, err := models.SettingModel.FindOne(ctx, bson.M{"name": enums.SettingDomainPlaylist})
	if err != nil {
		return fmt.Errorf("domain_playlist is unavailable: %w", err)
	}
	domain := domainSetting.GetString("")
	if domain == "" {
		return fmt.Errorf("domain_playlist is empty")
	}
	if !strings.HasPrefix(domain, "http://") && !strings.HasPrefix(domain, "https://") {
		domain = "https://" + domain
	}
	domain = strings.TrimRight(domain, "/")

	cfConfig := resolveCfProfile(ctx, "playlist")
	if cfConfig.ZoneID == "" || cfConfig.APIToken == "" {
		utils.LogMain("⚠️  [%s] Cloudflare purge skipped: playlist profile is not configured", slug)
		return nil
	}
	urls := make([]string, 0, len(slugs))
	for _, fileSlug := range slugs {
		urls = append(urls, fmt.Sprintf("%s/%s/playlist.m3u8", domain, fileSlug))
	}
	if len(urls) == 0 {
		return nil
	}
	utils.LogMain("☁️  [%s] Purging %d playlist URL(s) from Cloudflare cache...", slug, len(urls))
	return utils.PurgeCloudflareCache(ctx, cfConfig, urls)
}

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

// resolveS3VideoStorage finds directly playable durable S3 storage. originUrl
// is required because storage-node/nginx-vod reads the uploaded MP4 through it.
func resolveS3VideoStorage(ctx context.Context) (*models.Storage, error) {
	filter := bson.M{
		"enable":    true,
		"status":    enums.StorageStatusOnline,
		"type":      enums.StorageTypeS3,
		"originUrl": bson.M{"$type": "string", "$ne": ""},
		"accepts":   bson.M{"$all": []string{enums.StorageAcceptStorage, enums.StorageAcceptVideo}},
		"$or": []bson.M{
			{"drainState": "idle"},
			{"drainState": bson.M{"$exists": false}},
		},
	}
	storage, err := models.StorageModel.FindOne(ctx, filter, options.FindOne().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("no S3 video storage available")
	}
	if storage.OriginURL == nil || strings.TrimSpace(*storage.OriginURL) == "" {
		return nil, fmt.Errorf("S3 video storage has no origin URL")
	}
	return storage, nil
}

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
