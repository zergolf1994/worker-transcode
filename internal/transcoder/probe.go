package transcoder

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// VideoInfo contains video metadata from ffprobe
type VideoInfo struct {
	Width        int64
	Height       int64
	Duration     int64   // in seconds
	DurationF    float64 // duration in seconds (float)
	Codec        string
	VideoBitrate int64 // video bitrate in kbps
}

// ProbeVideoInfo extracts width, height, duration, and codec from a video file
func ProbeVideoInfo(filePath string) (*VideoInfo, error) {
	info := &VideoInfo{}

	// Get width and height
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height",
		"-of", "csv=s=x:p=0",
		filePath,
	)
	output, err := cmd.Output()
	if err == nil {
		parts := strings.Split(strings.TrimSpace(string(output)), "x")
		if len(parts) == 2 {
			w, _ := strconv.ParseInt(parts[0], 10, 64)
			h, _ := strconv.ParseInt(parts[1], 10, 64)
			info.Width = w
			info.Height = h
		}
	}

	// Get duration
	cmd = exec.Command("ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		filePath,
	)
	output, err = cmd.Output()
	if err == nil {
		dur, _ := strconv.ParseFloat(strings.TrimSpace(string(output)), 64)
		info.Duration = int64(dur)
		info.DurationF = dur
	}

	// Get codec
	cmd = exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=codec_name",
		"-of", "default=noprint_wrappers=1:nokey=1",
		filePath,
	)
	output, err = cmd.Output()
	if err == nil {
		info.Codec = strings.TrimSpace(string(output))
	}

	// Get video bitrate
	cmd = exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=bit_rate",
		"-of", "default=noprint_wrappers=1:nokey=1",
		filePath,
	)
	output, err = cmd.Output()
	if err == nil {
		brStr := strings.TrimSpace(string(output))
		if brStr != "" && brStr != "N/A" {
			br, _ := strconv.ParseInt(brStr, 10, 64)
			if br > 0 {
				info.VideoBitrate = br / 1000 // bps → kbps
			}
		}
	}

	// Fallback: estimate video bitrate from file size
	if info.VideoBitrate <= 0 && info.DurationF > 0 {
		fileInfo, statErr := os.Stat(filePath)
		if statErr == nil && fileInfo.Size() > 0 {
			totalBitrateKbps := float64(fileInfo.Size()) * 8 / info.DurationF / 1000
			info.VideoBitrate = int64(totalBitrateKbps * 0.85) // ~15% audio/overhead
		}
	}

	if info.Width == 0 || info.Height == 0 {
		return nil, fmt.Errorf("failed to probe video dimensions")
	}

	return info, nil
}

// Orientation constants
const (
	OrientationLandscape = "landscape"
	OrientationPortrait  = "portrait"
	OrientationSquare    = "square"
)

// DetectOrientation returns the video orientation based on width and height
func DetectOrientation(w, h int64) string {
	if w > h {
		return OrientationLandscape
	} else if h > w {
		return OrientationPortrait
	}
	return OrientationSquare
}

// ShortSide returns the shorter dimension of the video
func ShortSide(w, h int64) int64 {
	if w < h {
		return w
	}
	return h
}

// DetermineResolutions calculates which resolutions to encode based on short side.
// Uses ~95% tolerance (like YouTube) — e.g. 478px qualifies for 480p.
// Returns resolutions from lowest to highest (360 → 1080), max 1080p.
func DetermineResolutions(shortSide int64) []string {
	// threshold returns 95% of target (minimum pixels to qualify)
	threshold := func(target int64) int64 { return target * 95 / 100 }

	var resolutions []string

	// Always include 360p
	resolutions = append(resolutions, "360")

	if shortSide >= threshold(480) { // >= 456
		resolutions = append(resolutions, "480")
	}
	if shortSide >= threshold(720) { // >= 684
		resolutions = append(resolutions, "720")
	}
	if shortSide >= threshold(1080) { // >= 1026
		resolutions = append(resolutions, "1080")
	}

	return resolutions
}

// GetScaleDimensions returns the target width and height for a given resolution
// respecting the original aspect ratio and orientation
func GetScaleDimensions(srcW, srcH int64, targetShortSide int) (int, int) {
	if srcW > srcH {
		// Landscape: height = target, width = auto
		h := targetShortSide
		w := int(float64(srcW) / float64(srcH) * float64(h))
		return evenNum(w), evenNum(h)
	} else if srcH > srcW {
		// Portrait: width = target, height = auto
		w := targetShortSide
		h := int(float64(srcH) / float64(srcW) * float64(w))
		return evenNum(w), evenNum(h)
	}
	// Square
	return evenNum(targetShortSide), evenNum(targetShortSide)
}

// evenNum rounds up to the nearest even number (ffmpeg requires even dimensions)
func evenNum(n int) int {
	if n%2 != 0 {
		return n + 1
	}
	return n
}
