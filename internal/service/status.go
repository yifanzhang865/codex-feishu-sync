package service

import (
	"fmt"
	"strings"
)

func scheduledTaskRunning(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), "Running")
}

func launchServiceNotLoaded(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "could not find service") || strings.Contains(lower, "service not found")
}

func windowsDeleteScript(path string, pid int) string {
	path = strings.ReplaceAll(path, "%", "%%")
	return fmt.Sprintf("@echo off\r\n:wait\r\ntasklist /FI \"PID eq %d\" /NH | findstr /C:\"codex-feishu.exe\" >nul\r\nif not errorlevel 1 (\r\n timeout /T 1 /NOBREAK >nul\r\n goto wait\r\n)\r\ndel /F /Q \"%s\" >nul 2>&1\r\ndel /F /Q \"%%~f0\" >nul 2>&1\r\n", pid, path)
}
