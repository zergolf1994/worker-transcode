package utils

import (
	"crypto/sha256"
	"encoding/hex"
)

// AcquireInstanceLock prevents two OS processes from running with the same
// WORKER_ID. Different slots (@1, @2, ...) receive different locks.
func AcquireInstanceLock(workerID string) (release func(), err error) {
	sum := sha256.Sum256([]byte(workerID))
	return acquirePlatformInstanceLock(hex.EncodeToString(sum[:8]))
}
