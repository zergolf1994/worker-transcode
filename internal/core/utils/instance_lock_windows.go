//go:build windows

package utils

import (
	"fmt"
	"syscall"
	"unsafe"
)

var createMutexW = syscall.NewLazyDLL("kernel32.dll").NewProc("CreateMutexW")

func acquirePlatformInstanceLock(key string) (func(), error) {
	name, err := syscall.UTF16PtrFromString(`Local\worker-transcode-` + key)
	if err != nil {
		return nil, fmt.Errorf("encode instance lock name: %w", err)
	}
	handle, _, callErr := createMutexW.Call(0, 1, uintptr(unsafe.Pointer(name)))
	if handle == 0 {
		return nil, fmt.Errorf("create instance lock: %w", callErr)
	}
	if errno, ok := callErr.(syscall.Errno); ok && errno == syscall.ERROR_ALREADY_EXISTS {
		_ = syscall.CloseHandle(syscall.Handle(handle))
		return nil, fmt.Errorf("another process is already using this WORKER_ID")
	}
	return func() { _ = syscall.CloseHandle(syscall.Handle(handle)) }, nil
}
