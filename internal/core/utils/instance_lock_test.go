package utils

import "testing"

func TestInstanceLockRejectsDuplicateWorkerID(t *testing.T) {
	workerID := "transcode_instance-lock-test@1"
	release, err := AcquireInstanceLock(workerID)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}

	if releaseDuplicate, duplicateErr := AcquireInstanceLock(workerID); duplicateErr == nil {
		releaseDuplicate()
		release()
		t.Fatal("duplicate WORKER_ID lock unexpectedly succeeded")
	}
	release()

	releaseAgain, err := AcquireInstanceLock(workerID)
	if err != nil {
		t.Fatalf("lock was not released: %v", err)
	}
	releaseAgain()
}
