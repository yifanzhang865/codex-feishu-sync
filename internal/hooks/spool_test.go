package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSessionStartHookSpoolsRegistrationWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	payload, err := json.Marshal(map[string]string{
		"hook_event_name": "SessionStart",
		"session_id":      "session-a",
		"cwd":             "/work/project",
		"source":          "startup",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := Record(dir, payload); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "hook-events"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("spooled event count = %d, want 1", len(entries))
	}
	var received Registration
	if err := Drain(dir, func(reg Registration) error {
		received = reg
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if received.SessionID != "session-a" || received.CWD != "/work/project" {
		t.Fatalf("registration = %#v", received)
	}
	entries, err = os.ReadDir(filepath.Join(dir, "hook-events"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("events remain after successful drain: %d", len(entries))
	}
}
