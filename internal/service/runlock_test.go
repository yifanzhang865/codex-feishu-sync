//go:build linux || darwin || windows

package service

import (
	"errors"
	"testing"
)

func TestRunLockRejectsDuplicateAndReleasesOnClose(t *testing.T) {
	dir := t.TempDir()
	release, err := AcquireRunLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := AcquireRunLock(dir); !errors.Is(err, ErrAlreadyRunning) {
		if other != nil {
			other()
		}
		release()
		t.Fatalf("duplicate lock error = %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	release, err = AcquireRunLock(dir)
	if err != nil {
		t.Fatalf("released lock could not be acquired: %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}
