package appserver

import (
	"encoding/json"
	"strings"

	"github.com/zhangwei/codex-feishu-sync/internal/hooks"
)

type Thread struct {
	ID        string            `json:"id"`
	SessionID string            `json:"sessionId"`
	ParentID  string            `json:"parentThreadId"`
	Ephemeral bool              `json:"ephemeral"`
	CWD       string            `json:"cwd"`
	Name      string            `json:"name"`
	Preview   string            `json:"preview"`
	Source    json.RawMessage   `json:"source"`
	Status    json.RawMessage   `json:"status"`
	Turns     []json.RawMessage `json:"turns"`
	Path      string            `json:"path"`
	UpdatedAt int64             `json:"updatedAt"`
}

func MatchRegistration(threads []Thread, registration hooks.Registration) (Thread, bool) {
	if registration.ThreadID != "" {
		for _, thread := range threads {
			if thread.ID == registration.ThreadID {
				return thread, true
			}
		}
	}
	matches := make([]Thread, 0, 2)
	for _, thread := range threads {
		if thread.ParentID != "" || thread.Ephemeral {
			continue
		}
		if registration.SessionID != "" && thread.SessionID != registration.SessionID {
			continue
		}
		if registration.CWD != "" && thread.CWD != registration.CWD {
			continue
		}
		matches = append(matches, thread)
	}
	if len(matches) != 1 {
		return Thread{}, false
	}
	return matches[0], true
}

func (t Thread) SourceKind() string {
	return stringField(t.Source, "kind", "type", "value")
}

func (t Thread) IsBusy() bool {
	status := stringField(t.Status, "type", "status", "kind", "value")
	switch strings.ToLower(status) {
	case "active", "inprogress", "in_progress", "running", "working":
		return true
	case "idle":
		// After resume, the live runtime is authoritative over a stale
		// in-progress marker left by a terminated CLI in persisted history.
		return false
	default:
		// A separately running CLI is not loaded in this App Server. Its
		// persisted active turn still tells us whether it is busy.
		if len(t.Turns) == 0 {
			return false
		}
		var turn struct {
			Status json.RawMessage `json:"status"`
		}
		return json.Unmarshal(t.Turns[len(t.Turns)-1], &turn) == nil && statusIsActive(turn.Status)
	}
}

func (t Thread) ActiveTurnID() string {
	if id := stringField(t.Status, "activeTurnId", "turnId"); id != "" {
		return id
	}
	for i := len(t.Turns) - 1; i >= 0; i-- {
		var turn struct {
			ID     string          `json:"id"`
			Status json.RawMessage `json:"status"`
		}
		if json.Unmarshal(t.Turns[i], &turn) != nil || !statusIsActive(turn.Status) {
			continue
		}
		return turn.ID
	}
	return ""
}

func statusIsActive(raw json.RawMessage) bool {
	status := stringField(raw, "type", "status", "kind", "value")
	switch strings.ToLower(status) {
	case "active", "inprogress", "in_progress", "running", "working":
		return true
	default:
		return false
	}
}

func stringField(raw json.RawMessage, keys ...string) string {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return ""
	}
	for _, key := range keys {
		if item, ok := object[key]; ok {
			if json.Unmarshal(item, &value) == nil {
				return value
			}
		}
	}
	return ""
}
