//go:build windows

package dashboard

import "runtime"

// Windows is used for development only; production metrics are collected from
// Linux /proc. GPU metrics still work when nvidia-smi is in PATH.
type platformSampler struct{}

func newPlatformSampler(_ string) platformSampler { return platformSampler{} }
func (s *platformSampler) sample() (CPUInfo, MemoryInfo, DiskInfo) {
	return CPUInfo{Cores: runtime.NumCPU()}, MemoryInfo{}, DiskInfo{}
}
