package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var ErrAlreadyRunning = errors.New("codex-feishu-sync 已在运行，请勿重复启动")

func execCommand(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".codex-feishu-sync-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func command(name string, args ...string) (string, error) {
	output, err := execCommand(name, args...)
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message != "" {
			return message, fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), message)
		}
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output)), nil
}

func stopForUninstall(active bool, stop func() error) error {
	if !active {
		return nil
	}
	return stop()
}

func scheduledTaskExists(output string) bool {
	return strings.EqualFold(strings.TrimSpace(output), "true")
}
