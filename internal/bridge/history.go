package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/zhangwei/codex-feishu-sync/internal/appserver"
	"github.com/zhangwei/codex-feishu-sync/internal/config"
	"github.com/zhangwei/codex-feishu-sync/internal/events"
	"github.com/zhangwei/codex-feishu-sync/internal/state"
)

type storedTurn struct {
	ID     string           `json:"id"`
	Error  json.RawMessage  `json:"error"`
	Status json.RawMessage  `json:"status"`
	Items  []map[string]any `json:"items"`
}

type historyDisposition struct {
	baseline []string
	replay   []storedTurn
}

func (b *Bridge) syncHistoryOnResume(ctx context.Context, thread appserver.Thread, catchLatest bool) {
	initialized := b.store.HistoryInitialized(thread.ID)
	if initialized {
		marker := "codex-turn-status-baseline:" + thread.ID
		if !b.store.HasEvent(marker) {
			ids := []string{marker}
			for _, raw := range thread.Turns {
				var turn storedTurn
				if json.Unmarshal(raw, &turn) == nil && turnIsTerminal(turn.Status) && b.turnItemsMarked(thread.ID, turn.Items) {
					ids = append(ids, turnEventID(thread.ID, turn))
				}
			}
			if err := b.store.MarkEvents(ids); err != nil {
				slog.Warn("建立轮次状态基线失败", "error", err)
				return
			}
		}
		for _, rawTurn := range thread.Turns {
			var turn storedTurn
			if json.Unmarshal(rawTurn, &turn) == nil && turnIsTerminal(turn.Status) {
				b.syncStoredTurn(ctx, thread.ID, turn)
			}
		}
		return
	}
	disposition := initialHistoryDisposition(thread, catchLatest)
	for _, turn := range disposition.replay {
		b.syncStoredTurn(ctx, thread.ID, turn)
	}
	disposition.baseline = append(disposition.baseline, "codex-turn-status-baseline:"+thread.ID)
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
			disposition.replay = append(disposition.replay, turn)
			continue
		}
		disposition.baseline = append(disposition.baseline, turnEventID(thread.ID, turn))
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

func turnEventID(threadID string, turn storedTurn) string {
	id := turn.ID
	if id == "" {
		data, _ := json.Marshal(turn)
		id = fmt.Sprintf("legacy-%x", sha256.Sum256(data))
	}
	return "codex-turn:" + threadID + ":" + id
}

func (b *Bridge) turnItemsMarked(threadID string, items []map[string]any) bool {
	for _, item := range items {
		if id := codexItemEventID(threadID, item); id != "" && !b.store.HasEvent(id) {
			return false
		}
	}
	return true
}

func (b *Bridge) syncStoredTurn(ctx context.Context, threadID string, turn storedTurn) {
	_, assistantDelivered := b.syncTurnItems(ctx, threadID, turn.Items, false)
	if assistantDelivered && b.turnItemsMarked(threadID, turn.Items) {
		b.sendTurnStatus(ctx, threadID, turn, "")
	}
}

func (b *Bridge) sendTurnStatus(ctx context.Context, threadID string, turn storedTurn, message string) {
	id := turnEventID(threadID, turn)
	if b.store.HasEvent(id) {
		return
	}
	if _, bound := b.store.ChatForThread(threadID); !bound {
		return
	}
	if message == "" {
		message = events.TurnStatus(strings.ToLower(statusValue(turn.Status)))
		if detail := itemTextFromRaw(turn.Error); detail != "" {
			message = "任务失败：" + truncate(detail, 1000)
		}
	}
	if err := b.sendEvent(ctx, threadID, events.Event{Kind: events.Status, Text: message}); err != nil {
		return
	}
	if _, err := b.store.MarkEvent(id); err != nil {
		slog.Warn("保存轮次状态去重记录失败", "thread_id", threadID, "error", err)
	}
}
