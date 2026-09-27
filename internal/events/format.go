package events

import (
	"fmt"
	"strings"

	"github.com/zhangwei/codex-feishu-sync/internal/config"
)

type Kind string

const (
	UserMessage      Kind = "user_message"
	AssistantMessage Kind = "assistant_message"
	Status           Kind = "status"
	ToolOutput       Kind = "tool_output"
	ToolDetail       Kind = "tool_detail"
	PlanUpdate       Kind = "plan_update"
	Reasoning        Kind = "reasoning"
)

type Event struct {
	Kind Kind
	Text string
}

func Format(level config.SyncLevel, event Event) string {
	text := strings.TrimSpace(event.Text)
	if text == "" || event.Kind == Reasoning {
		return ""
	}
	switch event.Kind {
	case UserMessage:
		return "用户：" + text
	case AssistantMessage:
		return "Codex：" + text
	case Status:
		return "状态：" + text
	case PlanUpdate:
		return "计划已更新：\n" + text
	case ToolOutput:
		if level == config.WithTools || level == config.AllVisible {
			return "工具输出：" + text
		}
	case ToolDetail:
		if level == config.AllVisible {
			return "工具事件：" + text
		}
	}
	return ""
}

func TurnStatus(status string) string {
	switch status {
	case "completed":
		return "任务已完成。"
	case "interrupted":
		return "任务已中断。"
	case "failed":
		return fmt.Sprintf("任务失败：%s", status)
	default:
		return "任务状态：" + status
	}
}
