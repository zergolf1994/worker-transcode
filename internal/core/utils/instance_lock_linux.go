//go:build !windows

package utils

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func acquirePlatformInstanceLock(key string) (func(), error) {
	path := filepath.Join(os.TempDir(), "worker-transcode-"+key+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open instance lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("another process is already using this WORKER_ID")
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}
