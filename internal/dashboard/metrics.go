package dashboard

import (
	"context"
	"encoding/csv"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type SystemMetrics struct {
	CPU      CPUInfo    `json:"cpu"`
	Memory   MemoryInfo `json:"memory"`
	Disk     DiskInfo   `json:"disk"`
	GPUs     []GPUInfo  `json:"gpus"`
	GPUError string     `json:"gpuError,omitempty"`
}

type CPUInfo struct {
	Percent float64 `json:"percent"`
	Cores   int     `json:"cores"`
}

type MemoryInfo struct {
	Used    int64   `json:"used"`
	Total   int64   `json:"total"`
	Percent float64 `json:"percent"`
}

type DiskInfo struct {
	Used       int64   `json:"used"`
	Total      int64   `json:"total"`
	Percent    float64 `json:"percent"`
	ReadBytes  float64 `json:"readBytesPerSecond"`
	WriteBytes float64 `json:"writeBytesPerSecond"`
}

type GPUInfo struct {
	Name           string  `json:"name"`
	GPUPercent     float64 `json:"gpuPercent"`
	EncoderPercent float64 `json:"encoderPercent"`
	DecoderPercent float64 `json:"decoderPercent"`
	MemoryUsed     int64   `json:"memoryUsed"`
	MemoryTotal    int64   `json:"memoryTotal"`
	Temperature    float64 `json:"temperature"`
	PowerWatts     float64 `json:"powerWatts"`
}

type metricSampler struct {
	platform platformSampler
}

func newMetricSampler(storagePath string) *metricSampler {
	return &metricSampler{platform: newPlatformSampler(storagePath)}
}

func (s *metricSampler) sample() SystemMetrics {
	cpu, memory, disk := s.platform.sample()
	gpus, gpuErr := sampleGPUs()
	return SystemMetrics{CPU: cpu, Memory: memory, Disk: disk, GPUs: gpus, GPUError: gpuErr}
}

func sampleGPUs() ([]GPUInfo, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=name,utilization.gpu,utilization.encoder,utilization.decoder,memory.used,memory.total,temperature.gpu,power.draw", "--format=csv,noheader,nounits")
	out, err := cmd.Output()
	if err != nil {
		return []GPUInfo{}, "nvidia-smi unavailable"
	}
	reader := csv.NewReader(strings.NewReader(string(out)))
	records, err := reader.ReadAll()
	if err != nil {
		return []GPUInfo{}, "invalid nvidia-smi output"
	}
	gpus := make([]GPUInfo, 0, len(records))
	for _, row := range records {
		if len(row) < 8 {
			continue
		}
		gpus = append(gpus, GPUInfo{
			Name: strings.TrimSpace(row[0]), GPUPercent: number(row[1]), EncoderPercent: number(row[2]),
			DecoderPercent: number(row[3]), MemoryUsed: int64(number(row[4])) * 1024 * 1024,
			MemoryTotal: int64(number(row[5])) * 1024 * 1024, Temperature: number(row[6]), PowerWatts: number(row[7]),
		})
	}
	return gpus, ""
}

func number(value string) float64 {
	value = strings.TrimSpace(value)
	if value == "[N/A]" || value == "N/A" {
		return 0
	}
	v, _ := strconv.ParseFloat(value, 64)
	return v
}
