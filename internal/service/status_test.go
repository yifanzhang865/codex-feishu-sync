package service

import (
	"errors"
	"strings"
	"testing"
)

func TestScheduledTaskStateIsActiveOnlyWhenRunning(t *testing.T) {
	for _, test := range []struct {
		state string
		want  bool
	}{
		{state: "Running\n", want: true},
		{state: "Ready", want: false},
		{state: "Disabled", want: false},
	} {
		if got := scheduledTaskRunning(test.state); got != test.want {
			t.Errorf("scheduledTaskRunning(%q) = %v, want %v", test.state, got, test.want)
		}
	}
}

func TestWindowsDeleteScriptWaitsForTargetProcessAndRemovesExecutable(t *testing.T) {
	script := windowsDeleteScript(`C:\Program Files\Codex\codex-feishu.exe`, 42)
	for _, expected := range []string{
		`tasklist /FI "PID eq 42"`,
		`del /F /Q "C:\Program Files\Codex\codex-feishu.exe"`,
		`%~f0`,
	} {
		if !strings.Contains(script, expected) {
			t.Errorf("windowsDeleteScript() omitted %q", expected)
		}
	}
}

func TestUninstallStopFailurePreventsServiceRemoval(t *testing.T) {
	stopErr := errors.New("service could not stop")
	if err := stopForUninstall(true, func() error { return stopErr }); !errors.Is(err, stopErr) {
		t.Fatalf("stopForUninstall() error = %v, want stop error", err)
	}
	called := false
	if err := stopForUninstall(false, func() error {
		called = true
		return nil
	}); err != nil || called {
		t.Fatalf("inactive service stop = %v, called %v", err, called)
	}
}

func TestPlatformStatusParsersRecognizeInactiveServices(t *testing.T) {
	if !launchServiceNotLoaded("Could not find service in domain") {
		t.Fatal("launchctl missing-service output was not recognized")
	}
	if launchServiceNotLoaded("operation not permitted") {
		t.Fatal("launchctl permission error was treated as a missing service")
	}
	if scheduledTaskExists("False\n") || !scheduledTaskExists("True\n") {
		t.Fatal("scheduled task existence parser returned an unexpected result")
	}
}
