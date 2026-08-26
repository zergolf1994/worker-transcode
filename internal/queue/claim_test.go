package queue

import (
	"testing"
	"time"

	"worker-transcode/internal/core/enums"

	"go.mongodb.org/mongo-driver/bson"
)

func TestFailedTimelineFieldsClosesOnlyProcessingSteps(t *testing.T) {
	endedAt := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	timeline := bson.D{
		{Key: "download", Value: bson.D{{Key: "status", Value: enums.StepStatusCompleted}}},
		{Key: "encode_360", Value: bson.D{{Key: "status", Value: enums.StepStatusProcessing}, {Key: "percent", Value: 100.0}}},
		{Key: "upload_360", Value: bson.M{"status": enums.StepStatusPending}},
	}

	fields := failedTimelineFields(timeline, endedAt)
	if got := fields["timeline.encode_360.status"]; got != enums.StepStatusFailed {
		t.Fatalf("encode status = %v, want failed", got)
	}
	if got := fields["timeline.encode_360.endedAt"]; got != endedAt {
		t.Fatalf("encode endedAt = %v, want %v", got, endedAt)
	}
	if _, exists := fields["timeline.download.status"]; exists {
		t.Fatal("completed download must not be changed")
	}
	if _, exists := fields["timeline.upload_360.status"]; exists {
		t.Fatal("pending upload must not be changed")
	}
}

func TestFailedTimelineFieldsSupportsMapTimeline(t *testing.T) {
	fields := failedTimelineFields(bson.M{
		"encode_480": bson.M{"status": enums.StepStatusProcessing},
	}, time.Now())
	if fields["timeline.encode_480.status"] != enums.StepStatusFailed {
		t.Fatalf("fields = %#v, want encode_480 failed", fields)
	}
}
