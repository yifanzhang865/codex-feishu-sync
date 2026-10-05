package appserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func dialogueLine(at time.Time, role string) []byte {
	value, _ := json.Marshal(map[string]any{"timestamp": at, "type": "response_item", "payload": map[string]any{"type": "message", "role": role}})
	return append(value, '\n')
}

func TestDialogueActivityIgnoresToolsAndReadsPartialAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	old := time.Now().Add(-80 * time.Hour).UTC().Truncate(time.Second)
	recent := old.Add(79 * time.Hour)
	initial := append(dialogueLine(old, "user"), []byte(`{"timestamp":"2099-01-01T00:00:00Z","type":"response_item","payload":{"type":"function_call_output"}}`+"\n")...)
	if err := os.WriteFile(path, initial, 0600); err != nil {
		t.Fatal(err)
	}
	tracker := &ActivityTracker{}
	thread := Thread{ID: "thread", Path: path, UpdatedAt: time.Now().Unix()}
	at, err := tracker.LastDialogue(context.Background(), thread)
	if err != nil || !at.Equal(old) {
		t.Fatalf("tool output renewed activity: %v, %v", at, err)
	}
	line := dialogueLine(recent, "assistant")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(line[:len(line)/2]); err != nil {
		t.Fatal(err)
	}
	at, err = tracker.LastDialogue(context.Background(), thread)
	if err != nil || !at.Equal(old) {
		t.Fatalf("partial record changed activity: %v, %v", at, err)
	}
	if _, err := f.Write(line[len(line)/2:]); err != nil {
		t.Fatal(err)
	}
	at, err = tracker.LastDialogue(context.Background(), thread)
	if err != nil || !at.Equal(recent) {
		t.Fatalf("completed append was missed: %v, %v", at, err)
	}
	if err := os.WriteFile(path, dialogueLine(old, "user"), 0600); err != nil {
		t.Fatal(err)
	}
	at, err = tracker.LastDialogue(context.Background(), thread)
	if err != nil || !at.Equal(old) {
		t.Fatalf("truncated log retained stale activity: %v, %v", at, err)
	}
}

func TestUnreadableOrCorruptLogCannotPretendToBeInactive(t *testing.T) {
	tracker := &ActivityTracker{}
	if _, err := tracker.LastDialogue(context.Background(), Thread{Path: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("missing log did not fail closed")
	}
	path := filepath.Join(t.TempDir(), "corrupt.jsonl")
	if err := os.WriteFile(path, []byte("invalid json\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := tracker.LastDialogue(context.Background(), Thread{Path: path}); err == nil {
		t.Fatal("corrupt log did not fail closed")
	}
}
