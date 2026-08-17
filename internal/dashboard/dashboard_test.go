package dashboard

import (
	"testing"

	"go.mongodb.org/mongo-driver/bson"
)

func TestShouldStart(t *testing.T) {
	tests := []struct {
		workerID string
		want     bool
	}{
		{"transcode_host@1", true},
		{"transcode_host@2", false},
		{"transcode_host@12", false},
		{"transcode_manual", true},
	}
	for _, test := range tests {
		if got := ShouldStart(test.workerID); got != test.want {
			t.Errorf("ShouldStart(%q) = %v, want %v", test.workerID, got, test.want)
		}
	}
}

func TestTimelineSteps(t *testing.T) {
	timeline := bson.D{
		{Key: "upload_360", Value: bson.D{{Key: "status", Value: "pending"}, {Key: "percent", Value: int32(0)}}},
		{Key: "download", Value: bson.D{{Key: "status", Value: "completed"}, {Key: "percent", Value: float64(97)}}},
		{Key: "encode_360", Value: bson.D{{Key: "status", Value: "processing"}, {Key: "percent", Value: int32(55)}}},
	}
	steps := timelineSteps(timeline)
	if len(steps) != 3 {
		t.Fatalf("got %d steps, want 3", len(steps))
	}
	if steps[0].Key != "download" || steps[0].Percent != 100 {
		t.Fatalf("first step = %#v, want completed download", steps[0])
	}
	if steps[1].Key != "encode_360" || steps[1].Percent != 55 {
		t.Fatalf("second step = %#v, want encode_360 at 55%%", steps[1])
	}
}

func TestParseCodecUtilization(t *testing.T) {
	output := `# gpu   sm   mem   enc   dec   jpg   ofa
# Idx    %     %     %     %     %     %
    0   12     4    67     3     -     -
    1    8     2     -     -     -     -`
	got := parseCodecUtilization(output)
	if got[0].encoder != 67 || got[0].decoder != 3 {
		t.Fatalf("GPU 0 utilization = %#v", got[0])
	}
	if got[1].encoder != 0 || got[1].decoder != 0 {
		t.Fatalf("GPU 1 unsupported values must be zero: %#v", got[1])
	}
}
