//go:build linux || darwin

package service

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func AcquireRunLock(configDir string) (func() error, error) {
	dir := filepath.Join(configDir, "runtime")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, "service.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrAlreadyRunning
		}
		return nil, err
	}
	// Leave the lock file in place: unlinking a locked inode permits races.
	return file.Close, nil
}
