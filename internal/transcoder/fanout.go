package transcoder

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
)

// Rendition describes one output of a single-decode, multi-output encode.
type Rendition struct {
	Resolution string
	OutputPath string
	Width      int
	Height     int
}

// EncodeResolutions decodes inputPath once, splits its video frames, and
// encodes every rendition in the same FFmpeg process. The caller deliberately
// supplies the already-detected GPU encoder; CPU work must use the sequential
// EncodeResolution path so several libx264 encoders never compete at once.
func EncodeResolutions(
	ctx context.Context,
	inputPath string,
	renditions []Rendition,
	encoder string,
	totalDuration float64,
	srcBitrateKbps int64,
	srcShortSide int,
	includeAudio bool,
	onProgress func(percent int),
) error {
	if len(renditions) == 0 {
		return nil
	}
	if encoder == EncoderCPU {
		return fmt.Errorf("fanout requires a GPU encoder")
	}

	args := buildFanoutArgs(inputPath, renditions, encoder, srcBitrateKbps, srcShortSide, includeAudio)
	labels := make([]string, 0, len(renditions))
	for _, rendition := range renditions {
		labels = append(labels, rendition.Resolution+"p")
	}
	log.Printf("🎬 GPU fanout: decode once → %s", strings.Join(labels, ", "))

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	if err := runFFmpegWithProgress(cmd, totalDuration, onProgress); err != nil {
		return fmt.Errorf("fanout encode failed: %w", err)
	}

	for _, rendition := range renditions {
		info, err := os.Stat(rendition.OutputPath)
		if err != nil {
			return fmt.Errorf("%sp output not found: %w", rendition.Resolution, err)
		}
		if info.Size() == 0 {
			return fmt.Errorf("%sp output is empty", rendition.Resolution)
		}
		if err := ValidateEncodedOutput(rendition.OutputPath, totalDuration); err != nil {
			return fmt.Errorf("%sp output validation failed: %w", rendition.Resolution, err)
		}
		if !includeAudio {
			if err := ValidateVideoOnlyOutput(rendition.OutputPath); err != nil {
				return fmt.Errorf("%sp video-only validation failed: %w", rendition.Resolution, err)
			}
		}
		log.Printf("✅ Fanout encoded %sp (%.2f MB)", rendition.Resolution, float64(info.Size())/1024/1024)
	}
	return nil
}

func buildFanoutArgs(
	inputPath string,
	renditions []Rendition,
	encoder string,
	srcBitrateKbps int64,
	srcShortSide int,
	includeAudio bool,
) []string {
	args := []string{
		"-y",
		"-hide_banner",
		"-loglevel", "error",
		"-nostats",
		"-stats_period", "0.5",
		"-progress", "pipe:1",
	}

	switch encoder {
	case EncoderNVENC:
		args = append(args, "-hwaccel", "cuda")
	case EncoderAMF:
		if isLinux() {
			args = append(args, "-hwaccel", "vaapi")
		} else {
			args = append(args, "-hwaccel", "d3d11va")
		}
	case EncoderQSV:
		args = append(args, "-hwaccel", "qsv")
	}
	args = append(args, "-i", inputPath)

	inputLabels := make([]string, len(renditions))
	for i := range renditions {
		inputLabels[i] = fmt.Sprintf("[fan%d]", i)
	}
	filterParts := make([]string, 0, len(renditions)+1)
	if len(renditions) == 1 {
		filterParts = append(filterParts, fmt.Sprintf("[0:v:0]setpts=PTS-STARTPTS%s", inputLabels[0]))
	} else {
		filterParts = append(filterParts, fmt.Sprintf("[0:v:0]setpts=PTS-STARTPTS,split=%d%s", len(renditions), strings.Join(inputLabels, "")))
	}
	for i, rendition := range renditions {
		filterParts = append(filterParts, fmt.Sprintf(
			"%sscale=%d:%d:force_original_aspect_ratio=decrease,pad=ceil(iw/2)*2:ceil(ih/2)*2[out%d]",
			inputLabels[i], rendition.Width, rendition.Height, i,
		))
	}
	args = append(args, "-filter_complex", strings.Join(filterParts, ";"))

	for i, rendition := range renditions {
		shortSide := rendition.Height
		if rendition.Width < rendition.Height {
			shortSide = rendition.Width
		}
		args = append(args, "-map", fmt.Sprintf("[out%d]", i))
		if includeAudio {
			args = append(args, "-map", "0:a:0?")
		}
		args = append(args, getEncoderArgs(encoder, shortSide, srcBitrateKbps, srcShortSide)...)
		args = append(args, getHLSGOPArgs()...)
		args = append(args, "-pix_fmt", "yuv420p")
		if includeAudio {
			args = append(args, "-af", "asetpts=PTS-STARTPTS", "-c:a", "aac", "-b:a", getAudioBitrate(shortSide), "-ac", "2", "-ar", "48000")
		} else {
			args = append(args, "-an")
		}
		args = append(args, "-sn", "-dn", "-movflags", "+faststart", rendition.OutputPath)
	}
	return args
}
