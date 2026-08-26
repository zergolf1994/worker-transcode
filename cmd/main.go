package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"worker-transcode/internal/cache"
	"worker-transcode/internal/config"
	"worker-transcode/internal/core/utils"
	"worker-transcode/internal/dashboard"
	"worker-transcode/internal/db/database"
	"worker-transcode/internal/queue"
	"worker-transcode/internal/transcode"
	"worker-transcode/internal/transcoder"
)

// version ถูกฝังตอน build โดย GitHub Actions: -ldflags="-X main.version=v1.x.x"
var version = "dev"

func main() {
	config.Load()
	cache.Init(config.AppConfig.RedisURL)
	workerID := utils.GenerateWorkerID()
	releaseInstanceLock, err := utils.AcquireInstanceLock(workerID)
	if err != nil {
		log.Printf("❌ Worker %s refused to start: %v", workerID, err)
		os.Exit(1)
	}
	defer releaseInstanceLock()
	log.Printf("🚀 Starting Worker Transcode %s [Worker: %s]", version, workerID)

	// transcode ไม่ผูก storage — เครื่องไหนก็รันได้ (input ผ่าน HTTP จาก
	// storage-node, output อัพ S3 temp ให้ worker-transfer ติดตั้ง)

	// ffmpeg/ffprobe คือหัวใจของงานนี้ — ไม่มีก็ทำอะไรไม่ได้เลย
	if err := transcoder.CheckFFmpeg(); err != nil {
		log.Printf("❌ %v", err)
		time.Sleep(5 * time.Second) // กัน systemd restart-loop รัวๆ
		os.Exit(1)
	}
	if err := transcoder.CheckFFprobe(); err != nil {
		log.Printf("❌ %v", err)
		time.Sleep(5 * time.Second)
		os.Exit(1)
	}

	// ── MongoDB ───────────────────────────────────────────────
	if err := database.Connect(); err != nil {
		log.Printf("❌ Failed to connect to MongoDB: %v", err)
		time.Sleep(5 * time.Second) // ให้ log ถูก flush / กัน restart-loop รัวๆ
		os.Exit(1)
	}
	defer database.Disconnect()

	// ── Heartbeat ─────────────────────────────────────────────
	// ctx ยกเลิกเมื่อโดน SIGINT/SIGTERM → heartbeat mark ตัวเอง offline ก่อนจบ
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	utils.CleanOldLogs()
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				utils.CleanOldLogs()
			}
		}
	}()

	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		queue.StartHeartbeat(ctx, workerID)
	}()
	queue.ClaimGate = func(ctx context.Context) string {
		return queue.WorkerClaimGate(ctx, workerID)
	}

	// systemd may run several transcode instances on one host. Instance @1
	// alone owns :8886; it reads every sibling's active jobs from MongoDB.
	if dashboard.ShouldStart(workerID) {
		go dashboard.Start(ctx, config.AppConfig.DashboardPort, workerID, config.AppConfig.StoragePath)
	} else {
		log.Printf("📺 Dashboard is served by worker instance @1 on port %s", config.AppConfig.DashboardPort)
	}

	// ── Job loop (blocking จนโดน SIGINT/SIGTERM) ──────────────
	// shutdown ระหว่างทำงาน → loop จะ Release งานคืนคิวให้เอง
	queue.RunLoop(ctx, workerID, transcode.Run)

	log.Println("🛑 Shutting down...")

	// รอ heartbeat ปิดตัว (mark offline) ให้เสร็จก่อน disconnect DB
	select {
	case <-hbDone:
	case <-time.After(10 * time.Second):
		log.Println("⚠️ Heartbeat shutdown timed out")
	}
}
