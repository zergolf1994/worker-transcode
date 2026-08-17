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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Keep the CSV query to fields supported across old GeForce and new data
	// center cards. Asking for encoder/decoder utilization here makes the whole
	// command fail on some driver/GPU combinations, so those are read via dmon.
	cmd := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=index,name,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw", "--format=csv,noheader,nounits")
	out, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(out))
		if message == "" {
			message = err.Error()
		}
		return []GPUInfo{}, "nvidia-smi: " + message
	}
	reader := csv.NewReader(strings.NewReader(string(out)))
	records, err := reader.ReadAll()
	if err != nil {
		return []GPUInfo{}, "invalid nvidia-smi output"
	}
	utilization := sampleCodecUtilization()
	gpus := make([]GPUInfo, 0, len(records))
	for _, row := range records {
		if len(row) < 7 {
			continue
		}
		index := int(number(row[0]))
		codec := utilization[index]
		gpus = append(gpus, GPUInfo{
			Name: strings.TrimSpace(row[1]), GPUPercent: number(row[2]), EncoderPercent: codec.encoder,
			DecoderPercent: codec.decoder, MemoryUsed: int64(number(row[3])) * 1024 * 1024,
			MemoryTotal: int64(number(row[4])) * 1024 * 1024, Temperature: number(row[5]), PowerWatts: number(row[6]),
		})
	}
	if len(gpus) == 0 {
		return []GPUInfo{}, "nvidia-smi returned no GPU rows"
	}
	return gpus, ""
}

type codecUtilization struct {
	encoder float64
	decoder float64
}

// sampleCodecUtilization uses the legacy enc/dec columns documented for
// GeForce cards. Unsupported engines are reported as '-' and become zero.
func sampleCodecUtilization() map[int]codecUtilization {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi", "dmon", "-s", "u", "-c", "1").CombinedOutput()
	if err != nil {
		return map[int]codecUtilization{}
	}
	return parseCodecUtilization(string(out))
}

func parseCodecUtilization(output string) map[int]codecUtilization {
	result := make(map[int]codecUtilization)
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		// dmon -s u: gpu, sm, mem, enc, dec, [jpg, ofa]
		if len(fields) < 5 {
			continue
		}
		index, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		result[index] = codecUtilization{encoder: number(fields[3]), decoder: number(fields[4])}
	}
	return result
}

func number(value string) float64 {
	value = strings.TrimSpace(value)
	if value == "[N/A]" || value == "N/A" {
		return 0
	}
	v, _ := strconv.ParseFloat(value, 64)
	return v
}
