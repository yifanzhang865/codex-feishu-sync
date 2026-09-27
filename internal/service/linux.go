//go:build linux

package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const unitName = "codex-feishu-sync.service"

func Install(binaryPath string) error {
	if binaryPath == "" {
		return fmt.Errorf("binary path is required")
	}
	configHome, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	unitPath := filepath.Join(configHome, "systemd", "user", unitName)
	content := "[Unit]\nDescription=Codex Feishu Sync\nAfter=network-online.target\n\n[Service]\nType=simple\nExecStart=" + quoteSystemd(binaryPath) + " run\nRestart=on-failure\nRestartSec=3\n\n[Install]\nWantedBy=default.target\n"
	if err := writeAtomic(unitPath, []byte(content), 0600); err != nil {
		return err
	}
	if _, err := command("systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	_, err = command("systemctl", "--user", "enable", "--now", unitName)
	return err
}

func Uninstall() error {
	configHome, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	unitPath := filepath.Join(configHome, "systemd", "user", unitName)
	if _, err := os.Stat(unitPath); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err := command("systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if err := Stop(); err != nil {
		return err
	}
	if _, err := command("systemctl", "--user", "disable", unitName); err != nil {
		return err
	}
	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	_, err = command("systemctl", "--user", "daemon-reload")
	return err
}

func Active() (bool, error) {
	output, err := command("systemctl", "--user", "is-active", unitName)
	if err != nil {
		switch strings.TrimSpace(output) {
		case "inactive", "failed", "unknown":
			return false, nil
		case "activating", "deactivating", "reloading":
			return true, nil
		default:
			return false, err
		}
	}
	return strings.TrimSpace(output) == "active", nil
}

func Stop() error {
	_, err := command("systemctl", "--user", "stop", unitName)
	return err
}

func Start() error {
	_, err := command("systemctl", "--user", "start", unitName)
	return err
}

func RemoveInstalledBinary(path string) error {
	return removeFile(path)
}

func quoteSystemd(value string) string {
	return "\"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n").Replace(value) + "\""
}
