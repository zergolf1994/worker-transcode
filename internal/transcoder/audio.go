package transcoder

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

type AudioStream struct {
	Index    int
	Codec    string
	Language string
	Title    string
	Default  bool
	Forced   bool
	Duration float64
}

type audioProbe struct {
	Streams []struct {
		Index     int    `json:"index"`
		CodecName string `json:"codec_name"`
		Tags      struct {
			Language string `json:"language"`
			Title    string `json:"title"`
		} `json:"tags"`
		Disposition struct {
			Default int `json:"default"`
			Forced  int `json:"forced"`
		} `json:"disposition"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func ProbeAudioStreams(inputPath string) ([]AudioStream, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-select_streams", "a",
		"-show_entries", "stream=index,codec_name:stream_tags=language,title:stream_disposition=default,forced:format=duration",
		"-of", "json", inputPath)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe audio streams: %w", err)
	}
	var raw audioProbe
	if err := json.Unmarshal(output, &raw); err != nil {
		return nil, fmt.Errorf("decode audio streams: %w", err)
	}
	duration, _ := strconv.ParseFloat(raw.Format.Duration, 64)
	streams := make([]AudioStream, 0, len(raw.Streams))
	hasDefault := false
	for _, stream := range raw.Streams {
		if stream.Disposition.Default != 0 {
			hasDefault = true
		}
		language := strings.ToLower(strings.TrimSpace(stream.Tags.Language))
		if language == "" {
			language = "und"
		}
		streams = append(streams, AudioStream{
			Index: stream.Index, Codec: stream.CodecName, Language: language,
			Title: stream.Tags.Title, Default: stream.Disposition.Default != 0,
			Forced: stream.Disposition.Forced != 0, Duration: duration,
		})
	}
	if !hasDefault && len(streams) > 0 {
		streams[0].Default = true
	}
	return streams, nil
}

func ExtractAudioStream(ctx context.Context, inputPath, outputPath string, stream AudioStream, onProgress func(int)) error {
	args := []string{
		"-y", "-hide_banner", "-loglevel", "error", "-nostats", "-stats_period", "0.5", "-progress", "pipe:1",
		"-i", inputPath, "-map", fmt.Sprintf("0:%d", stream.Index),
		"-map_metadata", "-1", "-vn", "-sn", "-dn",
		"-c:a", "aac", "-b:a", "192k", "-ac", "2", "-ar", "48000",
		"-movflags", "+faststart", outputPath,
	}
	if err := runFFmpegWithProgress(exec.CommandContext(ctx, "ffmpeg", args...), stream.Duration, onProgress); err != nil {
		return err
	}
	info, err := os.Stat(outputPath)
	if err != nil || info.Size() == 0 {
		return fmt.Errorf("audio output missing or empty")
	}
	return nil
}
