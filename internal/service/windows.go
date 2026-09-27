//go:build windows

package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const taskName = "CodexFeishuSync"

func Install(binaryPath string) error {
	if binaryPath == "" {
		return fmt.Errorf("binary path is required")
	}
	if _, err := command("schtasks.exe", "/Create", "/F", "/TN", taskName, "/SC", "ONLOGON", "/RL", "LIMITED", "/TR", fmt.Sprintf("\"%s\" run", binaryPath)); err != nil {
		return err
	}
	_, err := command("schtasks.exe", "/Run", "/TN", taskName)
	return err
}

func Uninstall() error {
	output, err := command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "(Get-ScheduledTask -TaskName 'CodexFeishuSync' -ErrorAction SilentlyContinue) -ne $null")
	if err != nil {
		return err
	}
	if !scheduledTaskExists(output) {
		return nil
	}
	active, err := Active()
	if err != nil {
		return err
	}
	if err := stopForUninstall(active, Stop); err != nil {
		return err
	}
	_, err = command("schtasks.exe", "/Delete", "/F", "/TN", taskName)
	return err
}

func Active() (bool, error) {
	output, err := command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "(Get-ScheduledTask -TaskName 'CodexFeishuSync' -ErrorAction SilentlyContinue).State")
	if err != nil {
		return false, err
	}
	return scheduledTaskRunning(output), nil
}

func Stop() error {
	if _, err := command("schtasks.exe", "/End", "/TN", taskName); err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		active, err := Active()
		if err != nil {
			return err
		}
		if !active {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("waiting for scheduled task to stop timed out")
}

func Start() error {
	_, err := command("schtasks.exe", "/Run", "/TN", taskName)
	return err
}

func RemoveInstalledBinary(path string) error {
	current, err := os.Executable()
	if err != nil || !strings.EqualFold(filepath.Clean(path), filepath.Clean(current)) {
		return removeFile(path)
	}
	script, err := os.CreateTemp("", "codex-feishu-uninstall-*.cmd")
	if err != nil {
		return err
	}
	scriptPath := script.Name()
	if _, err := script.WriteString(windowsDeleteScript(path, os.Getpid())); err != nil {
		script.Close()
		os.Remove(scriptPath)
		return err
	}
	if err := script.Close(); err != nil {
		os.Remove(scriptPath)
		return err
	}
	if _, err := command("cmd.exe", "/D", "/C", "start", "", "/B", scriptPath); err != nil {
		os.Remove(scriptPath)
		return err
	}
	return nil
}

func UserDataDir() (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		return "", fmt.Errorf("LOCALAPPDATA is not set")
	}
	return filepath.Join(base, "CodexFeishuSync"), nil
}
