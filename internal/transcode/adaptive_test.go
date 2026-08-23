package transcode

import (
	"math"
	"testing"

	"worker-transcode/internal/transcoder"
)

func pipelineTasks(resolutions ...string) []renditionTask {
	tasks := make([]renditionTask, 0, len(resolutions))
	for _, resolution := range resolutions {
		tasks = append(tasks, renditionTask{resolution: resolution})
	}
	return tasks
}

func TestAdaptiveOverallProgressUsesEveryRendition(t *testing.T) {
	progress := newAdaptiveProgress("process", pipelineTasks("360", "480", "720", "1080"))
	progress.encode["360"] = 100
	progress.upload["360"] = 100

	if got, want := progress.overallLocked(), 31.25; math.Abs(got-want) > 0.001 {
		t.Fatalf("360p-ready overall = %.3f, want %.3f", got, want)
	}

	progress.encode["480"] = 50
	progress.encode["720"] = 50
	progress.encode["1080"] = 50
	if got, want := progress.overallLocked(), 53.5625; math.Abs(got-want) > 0.001 {
		t.Fatalf("fanout overall = %.4f, want %.4f", got, want)
	}
}

func TestShouldOverlapUploadsKeepsSequentialAsRollback(t *testing.T) {
	if shouldOverlapUploads("sequential", true) {
		t.Fatal("explicit sequential mode enabled upload overlap")
	}
	if !shouldOverlapUploads("adaptive", true) {
		t.Fatal("adaptive mode did not enable upload overlap")
	}
	if shouldOverlapUploads("adaptive", false) {
		t.Fatal("disabled upload overlap was ignored")
	}
}

func TestChoosePipeline(t *testing.T) {
	tests := []struct {
		name        string
		mode        string
		encoder     string
		duration    float64
		resolutions []string
		want        string
	}{
		{
			name: "CPU is always sequential", mode: "fanout", encoder: transcoder.EncoderCPU,
			duration: 10 * 60, resolutions: []string{"360", "480", "720", "1080"}, want: "sequential",
		},
		{
			name: "explicit sequential wins", mode: "sequential", encoder: transcoder.EncoderNVENC,
			duration: 10 * 60, resolutions: []string{"360", "480", "720"}, want: "sequential",
		},
		{
			name: "explicit fanout ignores duration", mode: "fanout", encoder: transcoder.EncoderNVENC,
			duration: 120 * 60, resolutions: []string{"360", "480", "720"}, want: "fanout",
		},
		{
			name: "adaptive includes exact threshold", mode: "adaptive", encoder: transcoder.EncoderNVENC,
			duration: 30 * 60, resolutions: []string{"360", "480", "720", "1080"}, want: "fanout",
		},
		{
			name: "adaptive prioritizes 360 for long high input", mode: "adaptive", encoder: transcoder.EncoderNVENC,
			duration: 30*60 + 1, resolutions: []string{"360", "480", "720", "1080"}, want: "priority360",
		},
		{
			name: "adaptive fans out long 480 input", mode: "adaptive", encoder: transcoder.EncoderNVENC,
			duration: 90 * 60, resolutions: []string{"360", "480"}, want: "fanout",
		},
		{
			name: "adaptive skips priority when 360 already covered", mode: "adaptive", encoder: transcoder.EncoderNVENC,
			duration: 90 * 60, resolutions: []string{"480", "720", "1080"}, want: "fanout",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := choosePipeline(tt.mode, tt.encoder, tt.duration, pipelineTasks(tt.resolutions...), 30)
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
