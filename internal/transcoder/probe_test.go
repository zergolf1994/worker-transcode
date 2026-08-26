package transcoder

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseVideoProbePrefersPrimaryVideoDuration(t *testing.T) {
	info, err := parseVideoProbe([]byte(`{
		"streams": [{
			"width": 1280,
			"height": 720,
			"codec_name": "h264",
			"start_time": "12.500000",
			"duration": "5783.340000",
			"bit_rate": "2000000"
		}],
		"format": {"duration": "5891.920000"}
	}`))
	if err != nil {
		t.Fatalf("parseVideoProbe: %v", err)
	}
	if info.DurationF != 5783.34 {
		t.Fatalf("video duration = %.2f, want 5783.34", info.DurationF)
	}
	if info.FormatDuration != 5891.92 {
		t.Fatalf("format duration = %.2f, want 5891.92", info.FormatDuration)
	}
	if info.StartTime != 12.5 {
		t.Fatalf("start time = %.2f, want 12.5", info.StartTime)
	}
}

func TestParseVideoProbeFallsBackToFormatDuration(t *testing.T) {
	info, err := parseVideoProbe([]byte(`{
		"streams": [{"width": 640, "height": 360, "duration": "N/A"}],
		"format": {"duration": "120.250000"}
	}`))
	if err != nil {
		t.Fatalf("parseVideoProbe: %v", err)
	}
	if info.DurationF != 120.25 {
		t.Fatalf("duration = %.2f, want format fallback 120.25", info.DurationF)
	}
}

func TestParseVideoProbeRejectsMissingVideoStream(t *testing.T) {
	_, err := parseVideoProbe([]byte(`{"streams": [], "format": {"duration": "10"}}`))
	if err == nil || !strings.Contains(err.Error(), "no readable primary video stream") {
		t.Fatalf("error = %v, want missing video stream diagnostic", err)
	}
}

func TestValidateDurationErrorDescribesDirection(t *testing.T) {
	tests := []struct {
		got, expected float64
		want          string
	}{
		{got: 90, expected: 100, want: "too short"},
		{got: 110, expected: 100, want: "too long"},
	}
	for _, tt := range tests {
		err := validateDuration(tt.got, tt.expected)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Fatalf("error = %v for %.0f/%.0f, want %q", err, tt.got, tt.expected, tt.want)
		}
	}
}

func TestProbeVideoInfoUsesVideoDurationWhenAudioIsLonger(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is not installed")
	}

	output := filepath.Join(t.TempDir(), "long-audio.mp4")
	args := []string{
		"-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=black:s=320x180:r=25:d=1",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3",
		"-map", "0:v:0", "-map", "1:a:0",
		"-c:v", "libx264", "-c:a", "aac", output,
	}
	if data, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("create long-audio fixture: %v: %s", err, data)
	}

	info, err := ProbeVideoInfo(output)
	if err != nil {
		t.Fatalf("ProbeVideoInfo: %v", err)
	}
	if info.DurationF < 0.9 || info.DurationF > 1.2 {
		t.Fatalf("video duration = %.3f, want about 1 second", info.DurationF)
	}
	if info.FormatDuration < 2.9 {
		t.Fatalf("format duration = %.3f, want about 3 seconds", info.FormatDuration)
	}
}

func TestEncodeResolutionRebasesPositiveVideoTimestamp(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is not installed")
	}

	dir := t.TempDir()
	input := filepath.Join(dir, "positive-start.mp4")
	output := filepath.Join(dir, "normalized.mp4")
	args := []string{
		"-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=black:s=320x180:r=25:d=1",
		"-vf", "setpts=PTS+5/TB", "-c:v", "libx264", input,
	}
	if data, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("create positive-start fixture: %v: %s", err, data)
	}

	source, err := ProbeVideoInfo(input)
	if err != nil {
		t.Fatalf("probe source: %v", err)
	}
	if source.StartTime < 4.9 {
		t.Fatalf("fixture start time = %.3f, want about 5 seconds", source.StartTime)
	}

	SetGPUEnabled(false)
	if err := EncodeResolution(
		context.Background(), input, output, 320, 180,
		source.DurationF, 500, 180, false, nil,
	); err != nil {
		t.Fatalf("EncodeResolution: %v", err)
	}

	normalized, err := ProbeVideoInfo(output)
	if err != nil {
		t.Fatalf("probe normalized output: %v", err)
	}
	if normalized.StartTime > 0.1 {
		t.Fatalf("normalized start time = %.3f, want near zero", normalized.StartTime)
	}
	if normalized.DurationF < 0.9 || normalized.DurationF > 1.2 {
		t.Fatalf("normalized duration = %.3f, want about 1 second", normalized.DurationF)
	}
}

func TestValidateVideoOnlyOutput(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is not installed")
	}

	dir := t.TempDir()
	videoOnly := filepath.Join(dir, "video-only.mp4")
	muxed := filepath.Join(dir, "muxed.mp4")

	createFixture(t, videoOnly, false)
	createFixture(t, muxed, true)
	if err := ValidateVideoOnlyOutput(videoOnly); err != nil {
		t.Fatalf("video-only rejected: %v", err)
	}
	if err := ValidateVideoOnlyOutput(muxed); err == nil {
		t.Fatal("muxed output accepted as video-only")
	}
}

func createFixture(t *testing.T, output string, withAudio bool) {
	t.Helper()
	args := []string{"-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=black:s=320x180:r=25:d=1"}
	if withAudio {
		args = append(args, "-f", "lavfi", "-i", "sine=frequency=440:duration=1",
			"-map", "0:v:0", "-map", "1:a:0", "-c:a", "aac")
	}
	args = append(args, "-c:v", "libx264", "-shortest", output)
	if data, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("create fixture: %v: %s", err, data)
	}
}
