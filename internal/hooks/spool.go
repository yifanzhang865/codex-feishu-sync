package hooks

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type Registration struct {
	ThreadID  string `json:"thread_id,omitempty"`
	SessionID string `json:"session_id"`
	CWD       string `json:"cwd"`
	Source    string `json:"source,omitempty"`
}

func Record(configDir string, payload []byte) error {
	var input struct {
		EventName string `json:"hook_event_name"`
		ThreadID  string `json:"thread_id"`
		SessionID string `json:"session_id"`
		CWD       string `json:"cwd"`
		Source    string `json:"source"`
	}
	if err := json.Unmarshal(payload, &input); err != nil {
		return fmt.Errorf("decode hook input: %w", err)
	}
	if input.EventName != "SessionStart" {
		return nil
	}
	if input.SessionID == "" || input.CWD == "" {
		return errors.New("SessionStart input must include session_id and cwd")
	}
	dir := filepath.Join(configDir, "hook-events")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create hook queue: %w", err)
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return err
	}
	path := filepath.Join(dir, hex.EncodeToString(id)+".json")
	registration := Registration{
		ThreadID: input.ThreadID, SessionID: input.SessionID, CWD: input.CWD, Source: input.Source,
	}
	data, err := json.Marshal(registration)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write hook queue item: %w", err)
	}
	return nil
}

func Drain(configDir string, handle func(Registration) error) error {
	dir := filepath.Join(configDir, "hook-events")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var registration Registration
		if err := json.Unmarshal(data, &registration); err != nil {
			return fmt.Errorf("decode hook queue item %s: %w", entry.Name(), err)
		}
		if err := handle(registration); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
