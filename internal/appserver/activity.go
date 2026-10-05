package appserver

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

type activityEntry struct {
	offset   int64
	modified time.Time
	last     time.Time
	info     os.FileInfo
}

// ActivityTracker reads real user/assistant messages, ignoring tool output,
// session resumes and filesystem timestamps. Append-only logs are read once.
type ActivityTracker struct {
	mu      sync.Mutex
	entries map[string]activityEntry
}

func (t *ActivityTracker) LastDialogue(ctx context.Context, thread Thread) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	if thread.Path == "" {
		return dialogueFromTurns(thread), nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.entries == nil {
		t.entries = make(map[string]activityEntry)
	}
	f, err := os.Open(thread.Path)
	if err != nil {
		return time.Time{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return time.Time{}, err
	}
	entry := t.entries[thread.Path]
	if (entry.info != nil && !os.SameFile(info, entry.info)) || info.Size() < entry.offset || (info.Size() == entry.offset && !info.ModTime().Equal(entry.modified)) {
		entry = activityEntry{}
	}
	if info.Size() == entry.offset {
		return entry.last, nil
	}
	if _, err := f.Seek(entry.offset, io.SeekStart); err != nil {
		return time.Time{}, err
	}
	reader := bufio.NewReaderSize(f, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			return time.Time{}, err
		}
		line, readErr := reader.ReadBytes('\n')
		// A writer may not have completed the last JSONL record yet.
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return time.Time{}, readErr
		}
		entry.offset += int64(len(line))
		var record struct {
			Timestamp time.Time `json:"timestamp"`
			Type      string    `json:"type"`
			Payload   struct {
				Type string `json:"type"`
				Role string `json:"role"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			return time.Time{}, fmt.Errorf("读取会话消息时间: %w", err)
		}
		isDialogue := record.Type == "event_msg" && (record.Payload.Type == "user_message" || record.Payload.Type == "agent_message")
		isDialogue = isDialogue || record.Type == "response_item" && record.Payload.Type == "message" && (record.Payload.Role == "user" || record.Payload.Role == "assistant")
		if isDialogue && record.Timestamp.After(entry.last) {
			entry.last = record.Timestamp
		}
	}
	entry.modified = info.ModTime()
	entry.info = info
	t.entries[thread.Path] = entry
	return entry.last, nil
}

func dialogueFromTurns(thread Thread) time.Time {
	var last time.Time
	for _, raw := range thread.Turns {
		var turn struct {
			StartedAt   int64 `json:"startedAt"`
			CompletedAt int64 `json:"completedAt"`
			Items       []struct {
				Type string `json:"type"`
			} `json:"items"`
		}
		if json.Unmarshal(raw, &turn) != nil {
			continue
		}
		for _, item := range turn.Items {
			if item.Type != "userMessage" && item.Type != "agentMessage" {
				continue
			}
			seconds := turn.StartedAt
			if turn.CompletedAt > seconds {
				seconds = turn.CompletedAt
			}
			if seconds == 0 {
				seconds = thread.UpdatedAt
			}
			if seconds > 0 && time.Unix(seconds, 0).After(last) {
				last = time.Unix(seconds, 0)
			}
			break
		}
	}
	return last
}
