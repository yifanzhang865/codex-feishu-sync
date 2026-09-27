package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/zhangwei/codex-feishu-sync/internal/feishu"
)

func (b *Bridge) onServerRequest(ctx context.Context, requestID json.RawMessage, method string, raw json.RawMessage) (any, error) {
	var params map[string]any
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, err
	}
	threadID, _ := params["threadId"].(string)
	chatID := b.chatFor(threadID)
	if threadID == "" || chatID == "" {
		return nil, errors.New("审批或问题请求没有已绑定的飞书群")
	}
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		return b.waitForApproval(ctx, requestID, method, params, chatID)
	case "item/permissions/requestApproval":
		return b.waitForApproval(ctx, requestID, method, params, chatID)
	case "item/tool/requestUserInput":
		return b.waitForAnswer(ctx, threadID, params, chatID)
	default:
		return nil, fmt.Errorf("暂不支持 Codex 请求类型 %q", method)
	}
}

func (b *Bridge) waitForApproval(ctx context.Context, requestID json.RawMessage, method string, params map[string]any, chatID string) (any, error) {
	key := string(requestID)
	result := make(chan string, 1)
	pending := pendingApproval{chatID: chatID, result: result}
	b.mu.Lock()
	b.pendingApprovals[key] = pending
	b.pendingByChat[chatID] = key
	b.mu.Unlock()
	defer b.removeApproval(key, chatID)

	summary := approvalSummary(method, params)
	card, err := approvalCard(summary, key)
	if err != nil {
		return nil, err
	}
	if err := b.feishu.SendCard(ctx, chatID, card); err != nil {
		return nil, err
	}
	var decision string
	select {
	case decision = <-result:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if method == "item/permissions/requestApproval" {
		permissions := params["permissions"]
		if decision != "accept" {
			permissions = map[string]any{}
		}
		return map[string]any{"permissions": permissions, "scope": "turn"}, nil
	}
	return map[string]string{"decision": decision}, nil
}

func (b *Bridge) waitForAnswer(ctx context.Context, threadID string, params map[string]any, chatID string) (any, error) {
	questions := make([]question, 0)
	if rawQuestions, ok := params["questions"].([]any); ok {
		for _, raw := range rawQuestions {
			encoded, _ := json.Marshal(raw)
			var item question
			if json.Unmarshal(encoded, &item) == nil {
				questions = append(questions, item)
			}
		}
	}
	answers := make(map[string]map[string][]string, len(questions))
	visible := make([]question, 0, len(questions))
	for _, item := range questions {
		if item.ID == "" {
			continue
		}
		if item.IsSecret {
			answers[item.ID] = map[string][]string{"answers": {}}
			continue
		}
		visible = append(visible, item)
	}
	if len(visible) == 0 {
		if len(answers) == 0 {
			return nil, errors.New("Codex 问题请求不包含可回答的问题")
		}
		_ = b.feishu.SendText(ctx, chatID, "此问题要求敏感答案，已留空处理；请在本机 Codex 中重新回答。")
		return map[string]any{"answers": answers}, nil
	}
	var prompt strings.Builder
	prompt.WriteString("Codex 需要你回答：\n")
	for index, item := range visible {
		fmt.Fprintf(&prompt, "%d. %s", index+1, item.Question)
		if item.Header != "" {
			fmt.Fprintf(&prompt, "（%s）", item.Header)
		}
		prompt.WriteString("\n")
	}
	if len(visible) == 1 {
		prompt.WriteString("直接回复答案。")
	} else {
		prompt.WriteString("按问题顺序逐行回复。")
	}
	result := make(chan string, 1)
	pending := &pendingQuestion{chatID: chatID, questions: visible, result: result}
	b.mu.Lock()
	if b.pendingQuestions[threadID] != nil {
		b.mu.Unlock()
		return nil, errors.New("此 Codex 会话已有待回答问题")
	}
	b.pendingQuestions[threadID] = pending
	b.mu.Unlock()
	defer b.removeQuestion(threadID, pending)
	if err := b.feishu.SendText(ctx, chatID, prompt.String()); err != nil {
		return nil, err
	}
	var text string
	select {
	case text = <-result:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	lines := splitAnswers(text, len(visible))
	for index, item := range visible {
		answers[item.ID] = map[string][]string{"answers": lines[index : index+1]}
	}
	return map[string]any{"answers": answers}, nil
}

func (b *Bridge) onCardAction(ctx context.Context, action feishu.CardAction) error {
	if action.SenderID != b.cfg.OwnerOpenID {
		return nil
	}
	accepted, err := b.store.MarkEvent(action.EventID)
	if err != nil || !accepted {
		return err
	}
	requestID, _ := action.Value["request_id"].(string)
	decision, _ := action.Value["decision"].(string)
	if decision != "accept" && decision != "decline" {
		return nil
	}
	b.mu.Lock()
	pending, ok := b.pendingApprovals[requestID]
	b.mu.Unlock()
	if !ok || pending.chatID != action.ChatID {
		return nil
	}
	select {
	case pending.result <- decision:
	default:
	}
	return nil
}

func (b *Bridge) handleApprovalText(ctx context.Context, message feishu.Inbound) (bool, error) {
	requestID, ok := b.pendingApprovalForChat(message.ChatID)
	if !ok {
		return false, nil
	}
	accepted, err := b.store.MarkEvent(message.EventID)
	if err != nil || !accepted {
		return true, err
	}
	text := strings.ToLower(strings.TrimSpace(message.Text))
	decision := ""
	switch text {
	case "/approve", "approve", "通过", "批准":
		decision = "accept"
	case "/deny", "deny", "拒绝", "不通过":
		decision = "decline"
	}
	if decision == "" {
		return true, b.feishu.SendText(ctx, message.ChatID, "当前消息用于等待审批，请点击卡片按钮，或回复 /approve、/deny。")
	}
	b.mu.Lock()
	pending, exists := b.pendingApprovals[requestID]
	b.mu.Unlock()
	if exists {
		select {
		case pending.result <- decision:
		default:
		}
	}
	return true, nil
}

func (b *Bridge) pendingQuestionForChat(chatID string) *pendingQuestion {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, pending := range b.pendingQuestions {
		if pending.chatID == chatID {
			return pending
		}
	}
	return nil
}

func (b *Bridge) pendingApprovalForChat(chatID string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	requestID, ok := b.pendingByChat[chatID]
	return requestID, ok
}

func (b *Bridge) removeApproval(requestID, chatID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.pendingApprovals, requestID)
	if b.pendingByChat[chatID] == requestID {
		delete(b.pendingByChat, chatID)
	}
}

func (b *Bridge) removeQuestion(threadID string, target *pendingQuestion) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pendingQuestions[threadID] == target {
		delete(b.pendingQuestions, threadID)
	}
}

func approvalSummary(method string, params map[string]any) string {
	var builder strings.Builder
	if method == "item/commandExecution/requestApproval" {
		builder.WriteString("需要批准执行命令")
		if value, _ := params["command"].(string); value != "" {
			builder.WriteString("\n命令：\n")
			builder.WriteString(truncate(value, 3000))
		}
	} else if method == "item/fileChange/requestApproval" {
		builder.WriteString("需要批准文件变更")
	} else {
		builder.WriteString("需要批准权限变更")
	}
	if reason, _ := params["reason"].(string); reason != "" {
		builder.WriteString("\n原因：")
		builder.WriteString(truncate(reason, 1000))
	}
	if cwd, _ := params["cwd"].(string); cwd != "" {
		builder.WriteString("\n工作目录：")
		builder.WriteString(cwd)
	}
	return builder.String()
}

func approvalCard(summary, requestID string) (string, error) {
	button := func(label, style, decision string) map[string]any {
		return map[string]any{
			"tag":  "button",
			"text": map[string]any{"tag": "plain_text", "content": label},
			"type": style,
			"behaviors": []any{map[string]any{
				"type":  "callback",
				"value": map[string]string{"request_id": requestID, "decision": decision},
			}},
		}
	}
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{
			"title":    map[string]any{"tag": "plain_text", "content": "Codex 需要人工审批"},
			"template": "orange",
		},
		"body": map[string]any{"elements": []any{
			map[string]any{"tag": "markdown", "content": summary},
			map[string]any{"tag": "column_set", "columns": []any{
				map[string]any{"tag": "column", "width": "auto", "vertical_align": "center", "elements": []any{
					button("通过", "primary", "accept"),
				}},
				map[string]any{"tag": "column", "width": "auto", "vertical_align": "center", "elements": []any{
					button("拒绝", "danger", "decline"),
				}},
			}},
		}},
	}
	data, err := json.Marshal(card)
	return string(data), err
}

func splitAnswers(text string, count int) []string {
	if count <= 1 {
		return []string{text}
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	answers := make([]string, count)
	for index := 0; index < count; index++ {
		if index < len(lines) {
			answers[index] = strings.TrimSpace(strings.TrimPrefix(lines[index], fmt.Sprintf("%d.", index+1)))
		}
	}
	return answers
}
