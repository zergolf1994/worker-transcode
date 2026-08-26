package transcoder

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// ValidateEncodedOutput verifies that a retry candidate is a finalized video,
// not merely a non-empty MP4 that FFmpeg was still writing when the worker
// stopped. A small duration tolerance covers normal container rounding.
func ValidateEncodedOutput(filePath string, expectedDuration float64) error {
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("stat output: %w", err)
	}
	if fileInfo.Size() <= 0 {
		return fmt.Errorf("output is empty")
	}

	info, err := ProbeVideoInfo(filePath)
	if err != nil {
		return fmt.Errorf("ffprobe output: %w", err)
	}
	if info.DurationF <= 0 {
		return fmt.Errorf("output has no readable duration")
	}
	if err := validateDuration(info.DurationF, expectedDuration); err != nil {
		return err
	}
	return nil
}

func validateDuration(actual, expected float64) error {
	if expected <= 0 {
		return nil
	}
	tolerance := math.Max(2, expected*0.01)
	delta := actual - expected
	if math.Abs(delta) <= tolerance {
		return nil
	}
	direction := "too long"
	if delta < 0 {
		direction = "too short"
	}
	return fmt.Errorf("duration %s: got %.2fs, expected %.2fs (tolerance %.2fs)", direction, actual, expected, tolerance)
}

// ValidateVideoOnlyOutput rejects retry artifacts created previously in muxed
// mode. Without this check, switching MEDIA_LAYOUT could reuse an MP4 that
// still contains an embedded audio stream.
func ValidateVideoOnlyOutput(filePath string) error {
	cmd := exec.Command("ffprobe", "-v", "error", "-select_streams", "a",
		"-show_entries", "stream=index", "-of", "csv=p=0", filePath)
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("ffprobe audio streams: %w", err)
	}
	if strings.TrimSpace(string(output)) != "" {
		return fmt.Errorf("encoded output still contains audio")
	}
	return nil
}

// VideoInfo contains video metadata from ffprobe
type VideoInfo struct {
	Width          int64
	Height         int64
	Duration       int64   // in seconds
	DurationF      float64 // primary video-stream duration in seconds (format fallback)
	FormatDuration float64 // whole-container duration; may include longer audio
	StartTime      float64 // primary video-stream start timestamp
	Codec          string
	VideoBitrate   int64 // video bitrate in kbps
}

type videoProbeResult struct {
	Streams []struct {
		Width     int64  `json:"width"`
		Height    int64  `json:"height"`
		Duration  string `json:"duration"`
		StartTime string `json:"start_time"`
		CodecName string `json:"codec_name"`
		BitRate   string `json:"bit_rate"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func parseProbeFloat(value string) float64 {
	value = strings.TrimSpace(value)
	if value == "" || value == "N/A" {
		return 0
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0
	}
	return parsed
}

func parseVideoProbe(output []byte) (*VideoInfo, error) {
	var probe videoProbeResult
	if err := json.Unmarshal(output, &probe); err != nil {
		return nil, fmt.Errorf("decode ffprobe JSON: %w", err)
	}
	if len(probe.Streams) == 0 {
		return nil, fmt.Errorf("no readable primary video stream")
	}

	stream := probe.Streams[0]
	if stream.Width <= 0 || stream.Height <= 0 {
		return nil, fmt.Errorf("invalid primary video dimensions: %dx%d", stream.Width, stream.Height)
	}

	formatDuration := parseProbeFloat(probe.Format.Duration)
	videoDuration := parseProbeFloat(stream.Duration)
	if videoDuration <= 0 {
		videoDuration = formatDuration
	}
	info := &VideoInfo{
		Width:          stream.Width,
		Height:         stream.Height,
		Duration:       int64(videoDuration),
		DurationF:      videoDuration,
		FormatDuration: formatDuration,
		StartTime:      parseProbeFloat(stream.StartTime),
		Codec:          strings.TrimSpace(stream.CodecName),
	}
	if bitrate := parseProbeFloat(stream.BitRate); bitrate > 0 {
		info.VideoBitrate = int64(bitrate) / 1000
	}
	return info, nil
}

// ProbeVideoInfo extracts width, height, duration, and codec from a video file
func ProbeVideoInfo(filePath string) (*VideoInfo, error) {
	// One JSON probe keeps all values from the same stream selection and also
	// preserves stderr. Previously four independent probes discarded the real
	// decoder/demuxer error and compared video-only outputs with format.duration
	// (which can be longer because of audio).
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height,duration,start_time,codec_name,bit_rate:format=duration",
		"-of", "json",
		filePath,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			detail = "no diagnostic output"
		}
		return nil, fmt.Errorf("ffprobe video stream: %w: %s", err, detail)
	}
	info, err := parseVideoProbe(output)
	if err != nil {
		return nil, err
	}

	// Fallback: estimate video bitrate from file size
	if info.VideoBitrate <= 0 && info.DurationF > 0 {
		fileInfo, statErr := os.Stat(filePath)
		if statErr == nil && fileInfo.Size() > 0 {
			totalBitrateKbps := float64(fileInfo.Size()) * 8 / info.DurationF / 1000
			info.VideoBitrate = int64(totalBitrateKbps * 0.85) // ~15% audio/overhead
		}
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
