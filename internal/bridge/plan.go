package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/zhangwei/codex-feishu-sync/internal/events"
)

type planStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

type planNotification struct {
	TurnID      string     `json:"turnId"`
	Explanation string     `json:"explanation"`
	Plan        []planStep `json:"plan"`
}

func (b *Bridge) handlePlanUpdated(ctx context.Context, threadID string, params json.RawMessage) {
	var notification planNotification
	if json.Unmarshal(params, &notification) != nil {
		return
	}
	text := formatPlan(notification.Explanation, notification.Plan)
	if text == "" {
		return
	}
	threadLock := b.threadLock(threadID)
	threadLock.Lock()
	defer threadLock.Unlock()
	if _, bound := b.store.ChatForThread(threadID); !bound {
		return
	}
	eventID := planEventID(threadID, notification)
	if b.store.HasEvent(eventID) {
		return
	}
	if err := b.sendEvent(ctx, threadID, events.Event{Kind: events.PlanUpdate, Text: text}); err != nil {
		return
	}
	if _, err := b.store.MarkEvent(eventID); err != nil {
		slog.Warn("记录计划通知去重状态失败", "thread_id", threadID, "error", err)
	}
}

func planEventID(threadID string, notification planNotification) string {
	payload, _ := json.Marshal(notification)
	digest := sha256.Sum256(payload)
	return fmt.Sprintf("codex-plan:%s:%x", threadID, digest[:])
}

func formatPlan(explanation string, steps []planStep) string {
	lines := make([]string, 0, len(steps)+1)
	if explanation = strings.TrimSpace(explanation); explanation != "" {
		lines = append(lines, "说明："+explanation)
	}
	stepNo := 0
	for _, step := range steps {
		text := strings.TrimSpace(step.Step)
		if text == "" {
			continue
		}
		stepNo++
		status := map[string]string{
			"pending":    "待处理",
			"inProgress": "进行中",
			"completed":  "已完成",
		}[step.Status]
		if status == "" {
			status = "状态：" + step.Status
		}
		lines = append(lines, fmt.Sprintf("%d. [%s] %s", stepNo, status, text))
	}
	return strings.Join(lines, "\n")
}
