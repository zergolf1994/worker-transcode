package transcoder

import (
	"os/exec"
	"path/filepath"
	"testing"
)

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
