package transcoder

import (
	"strings"
	"testing"
)

func countArg(args []string, value string) int {
	count := 0
	for _, arg := range args {
		if arg == value {
			count++
		}
	}
	return count
}

func TestBuildFanoutArgsUsesOneInputAndOneOutputPerRendition(t *testing.T) {
	renditions := []Rendition{
		{Resolution: "360", OutputPath: "360.mp4", Width: 640, Height: 360},
		{Resolution: "480", OutputPath: "480.mp4", Width: 854, Height: 480},
		{Resolution: "720", OutputPath: "720.mp4", Width: 1280, Height: 720},
	}
	args := buildFanoutArgs("original.mp4", renditions, EncoderNVENC, 5000, 1080, false)

	if got := countArg(args, "-i"); got != 1 {
		t.Fatalf("input count = %d, want 1: %v", got, args)
	}
	if got := countArg(args, "h264_nvenc"); got != len(renditions) {
		t.Fatalf("NVENC output count = %d, want %d: %v", got, len(renditions), args)
	}
	if got := countArg(args, "-an"); got != len(renditions) {
		t.Fatalf("video-only output count = %d, want %d: %v", got, len(renditions), args)
	}
	for _, rendition := range renditions {
		if countArg(args, rendition.OutputPath) != 1 {
			t.Fatalf("missing output %q in %v", rendition.OutputPath, args)
		}
	}
	filterIndex := -1
	for index, arg := range args {
		if arg == "-filter_complex" {
			filterIndex = index
			break
		}
	}
	if filterIndex < 0 || filterIndex+1 >= len(args) {
		t.Fatalf("missing filter_complex in %v", args)
	}
	filter := args[filterIndex+1]
	if !strings.Contains(filter, "setpts=PTS-STARTPTS") {
		t.Fatalf("filter does not normalize input timestamps: %s", filter)
	}
	if !strings.Contains(filter, "split=3") {
		t.Fatalf("filter does not split three ways: %s", filter)
	}
	for _, dimensions := range []string{"scale=640:360", "scale=854:480", "scale=1280:720"} {
		if !strings.Contains(filter, dimensions) {
			t.Fatalf("filter missing %q: %s", dimensions, filter)
		}
	}
}

func TestBuildFanoutArgsMapsOptionalAudioPerOutput(t *testing.T) {
	renditions := []Rendition{
		{Resolution: "360", OutputPath: "360.mp4", Width: 640, Height: 360},
		{Resolution: "480", OutputPath: "480.mp4", Width: 854, Height: 480},
	}
	args := buildFanoutArgs("original.mp4", renditions, EncoderNVENC, 5000, 1080, true)

	if got := countArg(args, "0:a:0?"); got != len(renditions) {
		t.Fatalf("audio map count = %d, want %d: %v", got, len(renditions), args)
	}
	if got := countArg(args, "aac"); got != len(renditions) {
		t.Fatalf("AAC encoder count = %d, want %d: %v", got, len(renditions), args)
	}
	if got := countArg(args, "-an"); got != 0 {
		t.Fatalf("unexpected -an in muxed fanout args: %v", args)
	}
	if got := countArg(args, "asetpts=PTS-STARTPTS"); got != len(renditions) {
		t.Fatalf("audio timestamp filter count = %d, want %d: %v", got, len(renditions), args)
	}
}
