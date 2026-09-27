package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/zhangwei/codex-feishu-sync/internal/appserver"
	"github.com/zhangwei/codex-feishu-sync/internal/config"
	"github.com/zhangwei/codex-feishu-sync/internal/events"
	"github.com/zhangwei/codex-feishu-sync/internal/state"
)

type storedTurn struct {
	Status json.RawMessage  `json:"status"`
	Items  []map[string]any `json:"items"`
}

type historyDisposition struct {
	baseline []string
	replay   [][]map[string]any
}

func (b *Bridge) syncHistoryOnResume(ctx context.Context, thread appserver.Thread, catchLatest bool) {
	initialized := b.store.HistoryInitialized(thread.ID)
	if initialized {
		for _, rawTurn := range thread.Turns {
			var turn storedTurn
			if json.Unmarshal(rawTurn, &turn) == nil && turnIsTerminal(turn.Status) {
				b.syncTurnItems(ctx, thread.ID, turn.Items, false)
			}
		}
		return
	}
	disposition := initialHistoryDisposition(thread, catchLatest)
	for _, items := range disposition.replay {
		b.syncTurnItems(ctx, thread.ID, items, false)
	}
	if err := b.store.InitializeHistory(thread.ID, disposition.baseline); err != nil {
		slog.Warn("初始化 Codex 会话历史去重状态失败", "thread_id", thread.ID, "error", err)
	}
}

func initialHistoryDisposition(thread appserver.Thread, catchLatest bool) historyDisposition {
	turns := make([]storedTurn, len(thread.Turns))
	for index, rawTurn := range thread.Turns {
		_ = json.Unmarshal(rawTurn, &turns[index])
	}
	disposition := historyDisposition{}
	for index, turn := range turns {
		if !turnIsTerminal(turn.Status) {
			continue
		}
		if catchLatest && index == len(turns)-1 {
			disposition.replay = append(disposition.replay, turn.Items)
			continue
		}
		for _, item := range turn.Items {
			if eventID := codexItemEventID(thread.ID, item); eventID != "" {
				disposition.baseline = append(disposition.baseline, eventID)
			}
		}
	}
	return disposition
}

func (b *Bridge) syncTurnItems(ctx context.Context, threadID string, items []map[string]any, streaming bool) (bool, bool) {
	hasAssistant := false
	assistantFullyDelivered := true
	if _, bound := b.store.ChatForThread(threadID); !bound {
		for _, item := range items {
			kind, _ := item["type"].(string)
			if kind == "agentMessage" {
				hasAssistant = true
			}
		}
		return hasAssistant, false
	}
	for _, item := range items {
		kind, _ := item["type"].(string)
		if kind == "agentMessage" {
			hasAssistant = true
		}
		if streaming && kind == "agentMessage" {
			b.markCodexItem(threadID, item)
			continue
		}
		if codexItemMarked(b.store, threadID, item) {
			continue
		}
		event, ok := itemEvent(item, b.cfg.SyncLevel)
		if !ok {
			b.markCodexItem(threadID, item)
			if kind == "agentMessage" {
				assistantFullyDelivered = false
			}
			continue
		}
		if err := b.sendEvent(ctx, threadID, event); err != nil {
			if kind == "agentMessage" {
				assistantFullyDelivered = false
			}
			continue
		}
		b.markCodexItem(threadID, item)
	}
	return hasAssistant, assistantFullyDelivered
}

func (b *Bridge) markCodexItem(threadID string, item map[string]any) {
	if _, err := markCodexItem(b.store, threadID, item); err != nil {
		slog.Warn("记录 Codex 条目去重状态失败", "thread_id", threadID, "error", err)
	}
}

func markCodexItem(store *state.Store, threadID string, item map[string]any) (bool, error) {
	eventID := codexItemEventID(threadID, item)
	if eventID == "" {
		return true, nil
	}
	return store.MarkEvent(eventID)
}

func codexItemMarked(store *state.Store, threadID string, item map[string]any) bool {
	eventID := codexItemEventID(threadID, item)
	return eventID != "" && store.HasEvent(eventID)
}

func codexItemEventID(threadID string, item map[string]any) string {
	itemID, _ := item["id"].(string)
	if itemID == "" {
		return ""
	}
	return "codex-item:" + threadID + ":" + itemID
}

func itemEvent(item map[string]any, level config.SyncLevel) (events.Event, bool) {
	kind, _ := item["type"].(string)
	text := strings.TrimSpace(itemText(item))
	if text == "" {
		return events.Event{}, false
	}
	switch kind {
	case "userMessage":
		return events.Event{Kind: events.UserMessage, Text: text}, true
	case "agentMessage":
		return events.Event{Kind: events.AssistantMessage, Text: text}, true
	case "plan":
		return events.Event{Kind: events.PlanUpdate, Text: text}, true
	case "reasoning", "reasoningSummary":
		return events.Event{}, false
	default:
		if level == config.AllVisible && kind != "commandExecution" {
			return events.Event{Kind: events.ToolDetail, Text: kind + ": " + text}, true
		}
		return events.Event{Kind: events.ToolOutput, Text: text}, true
	}
}

func turnIsTerminal(raw json.RawMessage) bool {
	switch strings.ToLower(statusValue(raw)) {
	case "completed", "interrupted", "failed":
		return true
	default:
		return false
	}
}
