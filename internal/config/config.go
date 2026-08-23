package config

import (
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

// AppConfig holds the application configuration loaded from environment variables.
var AppConfig Config

// Config represents the application configuration.
type Config struct {
	// DashboardPort is opened by worker instance @1 only. All instances write
	// progress to MongoDB, so that single dashboard can display the whole host.
	DashboardPort string
	MongoURI      string

	// transcode ไม่ผูก storage — สองตัวนี้ใช้แค่วัด disk ของเครื่อง
	// (heartbeat system info + disk gate ของ job loop) ว่าง = ใช้ working dir
	StorageId   string
	StoragePath string

	// Optional: invalidate content-node/player-node caches after direct media
	// creation. RADIS_URL remains supported for legacy deployments.
	RedisURL string

	// Number of multipart S3 parts uploaded in parallel.
	S3UploadConcurrency int
	MediaLayout         string

	// TranscodePipelineMode controls how renditions are scheduled:
	// adaptive (default), fanout, or sequential. Adaptive uses GPU fanout for
	// short inputs, prioritizes 360p for long high-resolution inputs, and
	// always keeps CPU encoding sequential.
	TranscodePipelineMode  string
	FanoutMaxMinutes       int
	TranscodeUploadOverlap bool
	MaxParallelUploads     int
}

// Load reads configuration from environment variables (and .env file).
func Load() {
	// Load .env file if present (ignore error if not found)
	godotenv.Load()

	AppConfig = Config{
		DashboardPort:          getEnv("DASHBOARD_PORT", getEnv("PORT", "8886")),
		MongoURI:               getEnv("DATABASE_URL", "mongodb://localhost:27017"),
		StorageId:              getEnv("STORAGE_ID", ""),
		StoragePath:            getEnv("STORAGE_PATH", ""),
		RedisURL:               getEnv("REDIS_URL", getEnv("RADIS_URL", "")),
		S3UploadConcurrency:    getIntEnv("S3_UPLOAD_CONCURRENCY", 2, 1, 8),
		MediaLayout:            getMediaLayoutEnv(),
		TranscodePipelineMode:  getTranscodePipelineModeEnv(),
		FanoutMaxMinutes:       getIntEnv("TRANSCODE_FANOUT_MAX_MINUTES", 30, 1, 1440),
		TranscodeUploadOverlap: getBoolEnv("TRANSCODE_UPLOAD_OVERLAP", true),
		MaxParallelUploads:     getIntEnv("TRANSCODE_MAX_PARALLEL_UPLOADS", 2, 1, 4),
	}
}

func getTranscodePipelineModeEnv() string {
	switch getEnv("TRANSCODE_PIPELINE_MODE", "adaptive") {
	case "sequential":
		return "sequential"
	case "fanout":
		return "fanout"
	default:
		return "adaptive"
	}
}

func getMediaLayoutEnv() string {
	switch getEnv("MEDIA_LAYOUT", "muxed") {
	case "separated":
		return "separated"
	default:
		return "muxed"
	}
}

func getIntEnv(key string, fallback, minValue, maxValue int) int {
	value, err := strconv.Atoi(os.Getenv(key))
	if err != nil {
		return fallback
	}
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func getBoolEnv(key string, fallback bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}
