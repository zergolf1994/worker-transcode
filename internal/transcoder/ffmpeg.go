package transcoder

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// isLinux returns true if running on Linux
func isLinux() bool {
	return runtime.GOOS == "linux"
}

// GPU encoder types
const (
	EncoderCPU   = "libx264"
	EncoderNVENC = "h264_nvenc"
	EncoderAMF   = "h264_amf"
	EncoderQSV   = "h264_qsv"
)

var (
	detectedEncoder string
	gpuEnabled      bool
	encoderDetected bool
	encoderMu       sync.Mutex
)

// SetGPUEnabled sets whether GPU encoding is allowed
// Called from main loop when reading transcode_gpu_enabled setting
func SetGPUEnabled(enabled bool) {
	encoderMu.Lock()
	defer encoderMu.Unlock()

	if gpuEnabled != enabled {
		gpuEnabled = enabled
		encoderDetected = false // re-detect on next call
		if enabled {
			log.Printf("🎮 GPU encoding enabled — will auto-detect on next encode")
		} else {
			detectedEncoder = EncoderCPU
			encoderDetected = true
			log.Printf("💻 GPU encoding disabled — using CPU (libx264)")
		}
	}
}

// DetectEncoder detects the best available encoder
// If GPU is disabled, always returns CPU
func DetectEncoder() string {
	encoderMu.Lock()
	defer encoderMu.Unlock()

	if encoderDetected {
		return detectedEncoder
	}

	if !gpuEnabled {
		detectedEncoder = EncoderCPU
		encoderDetected = true
		log.Printf("💻 GPU disabled — using CPU (libx264)")
		return detectedEncoder
	}

	// Try NVIDIA first (most common)
	if testEncoder(EncoderNVENC) {
		detectedEncoder = EncoderNVENC
		encoderDetected = true
		log.Printf("🎮 GPU Detected: NVIDIA (h264_nvenc)")
		return detectedEncoder
	}

	// Try AMD
	if testEncoder(EncoderAMF) {
		detectedEncoder = EncoderAMF
		encoderDetected = true
		log.Printf("🎮 GPU Detected: AMD (h264_amf)")
		return detectedEncoder
	}

	// Try Intel QuickSync
	if testEncoder(EncoderQSV) {
		detectedEncoder = EncoderQSV
		encoderDetected = true
		log.Printf("🎮 GPU Detected: Intel QSV (h264_qsv)")
		return detectedEncoder
	}

	// Fallback to CPU
	detectedEncoder = EncoderCPU
	encoderDetected = true
	log.Printf("💻 No GPU encoder found — using CPU (libx264)")
	return detectedEncoder
}

// testEncoder tests if an encoder is available by running a quick encode
func testEncoder(encoder string) bool {
	var args []string

	switch encoder {
	case EncoderNVENC:
		// NVENC needs proper pixel format — use color source with nv12
		args = []string{
			"-hide_banner",
			"-loglevel", "error",
			"-f", "lavfi",
			"-i", "color=c=black:s=256x256:d=1:r=25",
			"-pix_fmt", "yuv420p",
			"-c:v", encoder,
			"-frames:v", "1",
			"-f", "null",
			"-",
		}
	default:
		args = []string{
			"-hide_banner",
			"-loglevel", "error",
			"-f", "lavfi",
			"-i", "color=c=black:s=256x256:d=1:r=25",
			"-c:v", encoder,
			"-frames:v", "1",
			"-f", "null",
			"-",
		}
	}

	cmd := exec.Command("ffmpeg", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("   ❌ Test %s failed: %v — %s", encoder, err, strings.TrimSpace(string(output)))
		return false
	}
	log.Printf("   ✅ Test %s passed", encoder)
	return true
}

// getBitrateForRes returns target bitrate, maxrate, bufsize for a resolution
// Dynamically caps based on original file's bitrate to prevent output inflation
func getBitrateForRes(shortSide int, srcBitrateKbps int64, srcShortSide int) (bitrate string, maxrate string, bufsize string) {
	// Default bitrates — balanced for streaming (Netflix H.264 level)
	var defBR, defMax, defBuf int
	switch {
	case shortSide >= 1080:
		defBR, defMax, defBuf = 3500, 5250, 7000
	case shortSide >= 720:
		defBR, defMax, defBuf = 2000, 3000, 4000
	case shortSide >= 480:
		defBR, defMax, defBuf = 1000, 1500, 2000
	default: // 360p
		defBR, defMax, defBuf = 600, 900, 1200
	}

	// Cap proportionally if original bitrate is known
	if srcBitrateKbps > 0 && srcShortSide > 0 {
		// Proportional bitrate based on pixel count ratio (shortSide²)
		// Apply 85% safety margin — since we re-encode audio (96-128k) and add container overhead,
		// using 100% of source video bitrate would make total output exceed original file size
		ratio := float64(shortSide) * float64(shortSide) / (float64(srcShortSide) * float64(srcShortSide))
		capBR := int(float64(srcBitrateKbps) * ratio * 0.85)

		// Minimum floor per resolution to avoid terrible quality
		minBR := 200
		switch {
		case shortSide >= 1080:
			minBR = 1000
		case shortSide >= 720:
			minBR = 600
		case shortSide >= 480:
			minBR = 350
		}
		if capBR < minBR {
			capBR = minBR
		}

		if capBR < defBR {
			log.Printf("📊 Bitrate cap %dp: %dk → %dk (src: %dkbps @ %dp, ratio: %.2f)",
				shortSide, defBR, capBR, srcBitrateKbps, srcShortSide, ratio)
			defBR = capBR
			defMax = capBR * 11 / 10 // 1.1x — strict cap for GPU encoders
			defBuf = capBR * 2       // 2x
		}
	}

	return fmt.Sprintf("%dk", defBR), fmt.Sprintf("%dk", defMax), fmt.Sprintf("%dk", defBuf)
}

// getAudioBitrate returns audio bitrate per resolution
func getAudioBitrate(shortSide int) string {
	switch {
	case shortSide >= 1080:
		return "128k"
	case shortSide >= 360:
		return "96k"
	default:
		return "64k"
	}
}

// getCRF returns CRF/CQ value based on resolution
// Standard streaming quality — lower = better quality, larger file
func getCRF(shortSide int) int {
	if shortSide >= 1080 {
		return 24
	}
	if shortSide >= 720 {
		return 25
	}
	if shortSide >= 480 {
		return 26
	}
	return 27
}

// getEncoderArgs returns ffmpeg encoder arguments based on detected encoder
// Constrained CRF/CQ mode — quality-first with strict bitrate cap to prevent inflation
// srcBitrateKbps/srcShortSide enable dynamic bitrate capping
func getEncoderArgs(encoder string, shortSide int, srcBitrateKbps int64, srcShortSide int) []string {
	crf := getCRF(shortSide)
	bitrate, maxrate, bufsize := getBitrateForRes(shortSide, srcBitrateKbps, srcShortSide)

	switch encoder {
	case EncoderNVENC:
		// Pure bitrate VBR — NVENC ignores maxrate when CQ is set, so no CQ here
		return []string{
			"-c:v", "h264_nvenc",
			"-preset", "p5",
			"-tune", "hq",
			"-rc", "vbr",
			"-b:v", bitrate,
			"-maxrate", maxrate,
			"-bufsize", bufsize,
			"-profile:v", "high",
			"-spatial-aq", "1",
			"-b_ref_mode", "middle",
		}
	case EncoderAMF:
		// VBR with bitrate cap (AMF CQP doesn't support maxrate)
		return []string{
			"-c:v", "h264_amf",
			"-quality", "quality",
			"-rc", "vbr_peak",
			"-b:v", bitrate,
			"-maxrate", maxrate,
			"-bufsize", bufsize,
			"-profile:v", "high",
		}
	case EncoderQSV:
		// Constrained quality — global_quality for quality, maxrate as cap
		return []string{
			"-c:v", "h264_qsv",
			"-preset", "slow",
			"-global_quality", strconv.Itoa(crf),
			"-b:v", bitrate,
			"-maxrate", maxrate,
			"-bufsize", bufsize,
			"-look_ahead", "1",
			"-profile:v", "high",
		}
	default: // CPU — constrained CRF (CRF for quality, maxrate as ceiling)
		return []string{
			"-c:v", "libx264",
			"-preset", "medium",
			"-crf", strconv.Itoa(crf),
			"-maxrate", maxrate,
			"-bufsize", bufsize,
			"-profile:v", "high",
			"-threads", "0",
			"-x264-params", "keyint=60:min-keyint=30:scenecut=40",
		}
	}
}

// EncodeResolution encodes a video file to a specific resolution
// Auto-detects GPU encoder, fallback to CPU
// srcBitrateKbps/srcShortSide: original file's video bitrate and short side for dynamic capping
func EncodeResolution(ctx context.Context, inputPath, outputPath string, targetW, targetH int, totalDuration float64, srcBitrateKbps int64, srcShortSide int, onProgress func(percent int)) error {
	encoder := DetectEncoder()
	scaleFilter := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,pad=ceil(iw/2)*2:ceil(ih/2)*2", targetW, targetH)

	// Determine short side for per-resolution settings
	shortSide := targetH
	if targetW < targetH {
		shortSide = targetW
	}
	audioBitrate := getAudioBitrate(shortSide)

	// Build ffmpeg command
	args := []string{
		"-y",
		"-hide_banner",
		"-loglevel", "error",
		"-nostats",
		"-stats_period", "0.5",
		"-progress", "pipe:1",
	}

	// Use GPU hwaccel for decode — reduces CPU usage significantly
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

	// Add encoder-specific args (includes dynamic bitrate capping)
	args = append(args, getEncoderArgs(encoder, shortSide, srcBitrateKbps, srcShortSide)...)

	// Always use CPU-based scale filter (compatible with all encoders)
	args = append(args, "-vf", scaleFilter)
	args = append(args, "-pix_fmt", "yuv420p")

	args = append(args,
		"-c:a", "aac",
		"-b:a", audioBitrate,
		"-ac", "2",
		"-ar", "48000",
		"-movflags", "+faststart",
		outputPath,
	)

	bitrate, _, _ := getBitrateForRes(shortSide, srcBitrateKbps, srcShortSide)
	log.Printf("🔧 Encoder: %s | CRF/CQ: %d | Bitrate: %s | Audio: %s | SrcBR: %dkbps", encoder, getCRF(shortSide), bitrate, audioBitrate, srcBitrateKbps)

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)

	err := runFFmpegWithProgress(cmd, totalDuration, onProgress)
	if err != nil {
		// If GPU encoder fails, retry with CPU
		if encoder != EncoderCPU {
			log.Printf("⚠️  GPU encode failed, retrying with CPU (libx264)...")
			detectedEncoder = EncoderCPU // force CPU for subsequent encodes

			cpuArgs := []string{
				"-y",
				"-hide_banner",
				"-loglevel", "error",
				"-nostats",
				"-stats_period", "0.5",
				"-progress", "pipe:1",
				"-i", inputPath,
			}
			cpuArgs = append(cpuArgs, getEncoderArgs(EncoderCPU, shortSide, srcBitrateKbps, srcShortSide)...)
			cpuArgs = append(cpuArgs,
				"-vf", scaleFilter,
				"-c:a", "aac",
				"-b:a", audioBitrate,
				"-ac", "2",
				"-ar", "48000",
				"-movflags", "+faststart",
				"-pix_fmt", "yuv420p",
				outputPath,
			)

			cmd2 := exec.CommandContext(ctx, "ffmpeg", cpuArgs...)
			err = runFFmpegWithProgress(cmd2, totalDuration, onProgress)
			if err != nil {
				return fmt.Errorf("encode failed (CPU fallback): %w", err)
			}
		} else {
			return fmt.Errorf("encode failed: %w", err)
		}
	}

	// Verify output
	info, err := os.Stat(outputPath)
	if err != nil {
		return fmt.Errorf("output file not found: %w", err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("output file is empty")
	}

	log.Printf("✅ Encoded %s (%.2f MB) [%s]", outputPath, float64(info.Size())/1024/1024, encoder)
	return nil
}

// CheckFFmpeg verifies ffmpeg is available
func CheckFFmpeg() error {
	cmd := exec.Command("ffmpeg", "-version")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg not found: %w", err)
	}
	return nil
}

// CheckFFprobe verifies ffprobe is available
func CheckFFprobe() error {
	cmd := exec.Command("ffprobe", "-version")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffprobe not found: %w", err)
	}
	return nil
}

// runFFmpegWithProgress consumes FFmpeg's machine-readable -progress output.
// stderr is reserved for bounded error diagnostics.
func runFFmpegWithProgress(cmd *exec.Cmd, totalDuration float64, onProgress func(percent int)) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to pipe ffmpeg progress: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("failed to pipe stderr: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start ffmpeg: %w", err)
	}

	var lastLines []string
	const maxLastLines = 10

	progressDone := make(chan error, 1)
	go func() {
		lastPercent := -1
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 16*1024), 1024*1024)
		for scanner.Scan() {
			key, value, ok := strings.Cut(scanner.Text(), "=")
			if !ok || (key != "out_time_us" && key != "out_time_ms") {
				continue
			}
			elapsedMicros, parseErr := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if parseErr != nil || elapsedMicros < 0 || totalDuration <= 0 {
				continue
			}
			percent := int(float64(elapsedMicros) / (totalDuration * 1_000_000) * 100)
			if percent > 99 {
				percent = 99
			}
			if percent < 0 || percent == lastPercent {
				continue
			}
			lastPercent = percent
			if onProgress != nil {
				onProgress(percent)
			}
		}
		progressDone <- scanner.Err()
	}()

	stderrDone := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 16*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			lastLines = append(lastLines, line)
			if len(lastLines) > maxLastLines {
				lastLines = lastLines[1:]
			}
		}
		stderrDone <- scanner.Err()
	}()

	progressErr := <-progressDone
	stderrErr := <-stderrDone
	waitErr := cmd.Wait()

	if waitErr != nil {
		var stderrMsg string
		if len(lastLines) > 0 {
			log.Printf("⚠️  ffmpeg stderr (last %d lines):", len(lastLines))
			for _, line := range lastLines {
				log.Printf("   %s", line)
			}
			stderrMsg = strings.Join(lastLines, "\n")
		}
		return fmt.Errorf("%w: %s", waitErr, stderrMsg)
	}
	if progressErr != nil {
		return fmt.Errorf("read ffmpeg progress: %w", progressErr)
	}
	if stderrErr != nil {
		return fmt.Errorf("read ffmpeg stderr: %w", stderrErr)
	}
	if onProgress != nil {
		onProgress(100)
	}
	return nil
}

// GetFileSize returns the file size in bytes
func GetFileSize(filePath string) int64 {
	info, err := os.Stat(filePath)
	if err != nil {
		return 0
	}
	return info.Size()
}
