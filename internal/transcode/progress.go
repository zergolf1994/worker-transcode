package transcode

import (
	"context"
	"fmt"
	"time"

	"worker-transcode/internal/core/enums"
	"worker-transcode/internal/core/utils"
	"worker-transcode/internal/db/models"

	"go.mongodb.org/mongo-driver/bson"
)

// ─── Progress writes ─────────────────────────────────────────
// transcode เป็นงานยาว (encode เป็นสิบนาที–ชั่วโมง) — เขียน % ลง DB
// แบบ throttle (ทุก ~1%) ให้หน้า admin และ realtime monitor เห็นความคืบหน้า
// ตรงกับ milestone ใน process log โดยจำกัดสูงสุดประมาณ 100 writes ต่อ step
// steps เป็น dynamic ตาม resolution: download, encode_{res}, upload_{res}

func startStep(ctx context.Context, processID, step string) {
	now := time.Now()
	models.VideoProcessModel.UpdateByID(ctx, processID, bson.M{"$set": bson.M{
		fmt.Sprintf("timeline.%s.status", step):    enums.StepStatusProcessing,
		fmt.Sprintf("timeline.%s.percent", step):   0,
		fmt.Sprintf("timeline.%s.startedAt", step): now,
	}})
}

func completeStep(ctx context.Context, processID, step string) {
	now := time.Now()
	models.VideoProcessModel.UpdateByID(ctx, processID, bson.M{"$set": bson.M{
		fmt.Sprintf("timeline.%s.status", step):  enums.StepStatusCompleted,
		fmt.Sprintf("timeline.%s.percent", step): 100,
		fmt.Sprintf("timeline.%s.endedAt", step): now,
	}})
}

// updateStepAndOverall writes both progress values atomically. The realtime
// monitor uses both fields, and one update per 1% avoids doubling Mongo writes.
func updateStepAndOverall(ctx context.Context, processID, step string, stepPercent, overallPercent float64) {
	if overallPercent > 100 {
		overallPercent = 100
	}
	models.VideoProcessModel.UpdateByID(ctx, processID, bson.M{"$set": bson.M{
		fmt.Sprintf("timeline.%s.status", step):  enums.StepStatusProcessing,
		fmt.Sprintf("timeline.%s.percent", step): stepPercent,
		"overallPercent":                         overallPercent,
	}})
}

func updateOverallPercent(ctx context.Context, processID string, percent float64) {
	if percent > 100 {
		percent = 100
	}
	models.VideoProcessModel.UpdateByID(ctx, processID, bson.M{"$set": bson.M{
		"overallPercent": percent,
	}})
}

// stepThrottle คืนตัวเช็คว่า % นี้ควรเขียน DB ไหม (ทุก stepPct%)
func stepThrottle(stepPct float64) func(pct float64) bool {
	last := -stepPct
	return func(pct float64) bool {
		if pct-last >= stepPct || pct >= 100 {
			last = pct
			return true
		}
		return false
	}
}

// pctLogger64 returns a bytes-progress callback that logs every ~10%.
func pctLogger64(slug, step string) func(done, total int64) {
	lastPct := -10.0
	return func(done, total int64) {
		if total <= 0 {
			return
		}
		pct := float64(done) / float64(total) * 100
		if pct-lastPct >= 10 || pct >= 100 {
			utils.LogProgress("📊 [%s] %s: %.1f%% (%.2f / %.2f MB)", slug, step, pct,
				float64(done)/1024/1024, float64(total)/1024/1024)
			lastPct = pct
		}
	}
}

// pctLogger returns an encode-progress callback that logs every 1%.
func pctLogger(slug, step string) func(percent int) {
	nextMilestone := 0
	return func(percent int) {
		if percent > 100 {
			percent = 100
		}
		for nextMilestone <= percent && nextMilestone <= 100 {
			utils.LogProgress("📊 [%s] %s: %d%%", slug, step, nextMilestone)
			nextMilestone++
		}
	}
}
