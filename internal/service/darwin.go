//go:build darwin

package service

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const launchLabel = "com.codex.feishu-sync"

func Install(binaryPath string) error {
	if binaryPath == "" {
		return fmt.Errorf("binary path is required")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	configHome, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	logDir := filepath.Join(configHome, "codex-feishu-sync", "logs")
	if err := os.MkdirAll(logDir, 0700); err != nil {
		return err
	}
	data := []byte(plist(binaryPath, filepath.Join(logDir, "service.log"), filepath.Join(logDir, "service.error.log")))
	path := filepath.Join(home, "Library", "LaunchAgents", launchLabel+".plist")
	if err := writeAtomic(path, data, 0600); err != nil {
		return err
	}
	uid := strconv.Itoa(os.Getuid())
	_, _ = command("launchctl", "bootout", "gui/"+uid+"/"+launchLabel)
	_, err = command("launchctl", "bootstrap", "gui/"+uid, path)
	return err
}

func Uninstall() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	active, err := Active()
	if err != nil {
		return err
	}
	if err := stopForUninstall(active, Stop); err != nil {
		return err
	}
	path := filepath.Join(home, "Library", "LaunchAgents", launchLabel+".plist")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func Active() (bool, error) {
	uid := strconv.Itoa(os.Getuid())
	output, err := command("launchctl", "print", "gui/"+uid+"/"+launchLabel)
	if err != nil {
		if launchServiceNotLoaded(output) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func Stop() error {
	uid := strconv.Itoa(os.Getuid())
	_, err := command("launchctl", "bootout", "gui/"+uid+"/"+launchLabel)
	return err
}

func Start() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	uid := strconv.Itoa(os.Getuid())
	path := filepath.Join(home, "Library", "LaunchAgents", launchLabel+".plist")
	_, err = command("launchctl", "bootstrap", "gui/"+uid, path)
	return err
}

func RemoveInstalledBinary(path string) error {
	return removeFile(path)
}

func plist(binaryPath, stdoutPath, stderrPath string) string {
	values := []string{launchLabel, binaryPath, "run", stdoutPath, stderrPath}
	for index, value := range values {
		var escaped strings.Builder
		_ = xml.EscapeText(&escaped, []byte(value))
		values[index] = escaped.String()
	}
	return xml.Header +
		"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n" +
		"<plist version=\"1.0\"><dict>" +
		"<key>Label</key><string>" + values[0] + "</string>" +
		"<key>ProgramArguments</key><array><string>" + values[1] + "</string><string>" + values[2] + "</string></array>" +
		"<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>" +
		"<key>StandardOutPath</key><string>" + values[3] + "</string>" +
		"<key>StandardErrorPath</key><string>" + values[4] + "</string>" +
		"</dict></plist>\n"
}
