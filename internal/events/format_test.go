package events

import (
	"strings"
	"testing"

	"github.com/zhangwei/codex-feishu-sync/internal/config"
)

func TestReasoningIsNeverForwarded(t *testing.T) {
	event := Event{Kind: Reasoning, Text: "private reasoning"}
	for _, level := range []config.SyncLevel{config.ConversationStatus, config.WithTools, config.AllVisible} {
		if got := Format(level, event); got != "" {
			t.Fatalf("Format(%q, reasoning) = %q", level, got)
		}
	}
}

func TestSyncLevelsControlToolOutput(t *testing.T) {
	event := Event{Kind: ToolOutput, Text: "go test: ok"}
	if got := Format(config.ConversationStatus, event); got != "" {
		t.Fatalf("conversation_status included tool output: %q", got)
	}
	if got := Format(config.WithTools, event); !strings.Contains(got, "go test: ok") {
		t.Fatalf("with_tools omitted tool output: %q", got)
	}
	if got := Format(config.AllVisible, event); !strings.Contains(got, "go test: ok") {
		t.Fatalf("all_visible omitted tool output: %q", got)
	}
}

func TestConversationIncludesUserAssistantAndStatus(t *testing.T) {
	for _, event := range []Event{
		{Kind: UserMessage, Text: "request"},
		{Kind: AssistantMessage, Text: "answer"},
		{Kind: Status, Text: "waiting for approval"},
	} {
		if got := Format(config.ConversationStatus, event); got == "" {
			t.Fatalf("conversation_status omitted %q", event.Kind)
		}
	}
}

func TestConversationIncludesPlanUpdates(t *testing.T) {
	got := Format(config.ConversationStatus, Event{Kind: PlanUpdate, Text: "1. [进行中] 修改模块"})
	if got != "计划已更新：\n1. [进行中] 修改模块" {
		t.Fatalf("Format(plan update) = %q", got)
	}
}
