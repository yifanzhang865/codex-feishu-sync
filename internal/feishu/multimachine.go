package feishu

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

type RoutedAction struct {
	Action   CardAction
	AtMillis int64
}
type HistoryBatch struct {
	Messages []Inbound
	Actions  []RoutedAction
	Cursor   int64
}
type relayPacket struct {
	Version   int        `json:"version"`
	Action    CardAction `json:"action"`
	Signature string     `json:"signature"`
}

func (c *Client) routingDescription() string {
	sum := sha256.Sum256([]byte(c.appID + ":" + c.ownerID))
	return "Codex Feishu Sync routing v1: " + hex.EncodeToString(sum[:16])
}

// All hosts discover the same bot-only private group. A deterministic request
// UUID prevents two first-time installations from creating separate mailboxes.
func (c *Client) EnsureRoutingChat(ctx context.Context) (string, error) {
	c.routingMu.Lock()
	defer c.routingMu.Unlock()
	if c.routingChatID != "" {
		if err := c.checkRoutingChat(ctx, c.routingChatID); err != nil {
			return "", err
		}
		return c.routingChatID, nil
	}
	description := c.routingDescription()
	token := ""
	for page := 0; page < 100; page++ {
		builder := larkim.NewListChatReqBuilder().PageSize(100).UserIdType("open_id")
		if token != "" {
			builder.PageToken(token)
		}
		response, err := c.sdk.RawClient().Im.V1.Chat.List(ctx, builder.Build())
		if err != nil {
			return "", err
		}
		if !response.Success() || response.Data == nil {
			return "", fmt.Errorf("发现多机路由群失败，需 im:chat:read 权限，错误码 %d: %s", response.Code, response.Msg)
		}
		for _, chat := range response.Data.Items {
			if chat != nil && chat.Description != nil && *chat.Description == description && chat.ChatId != nil {
				if err := c.checkRoutingChat(ctx, *chat.ChatId); err != nil {
					return "", err
				}
				c.routingChatID = *chat.ChatId
				return c.routingChatID, nil
			}
		}
		if response.Data.HasMore == nil || !*response.Data.HasMore {
			break
		}
		if response.Data.PageToken == nil || *response.Data.PageToken == "" || *response.Data.PageToken == token || page == 99 {
			return "", errors.New("路由群列表分页无效，未创建重复群")
		}
		token = *response.Data.PageToken
	}
	sum := sha256.Sum256([]byte(description))
	body := larkim.NewCreateChatReqBodyBuilder().Name("Codex Feishu Sync Routing").Description(description).ChatMode("group").ChatType("private").Build()
	response, err := c.sdk.RawClient().Im.V1.Chat.Create(ctx, larkim.NewCreateChatReqBuilder().UserIdType("open_id").Uuid(hex.EncodeToString(sum[:16])).Body(body).Build())
	if err != nil {
		return "", err
	}
	if !response.Success() || response.Data == nil || response.Data.ChatId == nil {
		return "", fmt.Errorf("创建机器人专用路由群失败，需 im:chat:create 权限，错误码 %d: %s", response.Code, response.Msg)
	}
	c.routingChatID = *response.Data.ChatId
	if err := c.checkRoutingChat(ctx, c.routingChatID); err != nil {
		c.routingChatID = ""
		return "", err
	}
	return c.routingChatID, nil
}

func (c *Client) checkRoutingChat(ctx context.Context, id string) error {
	r, err := c.sdk.RawClient().Im.V1.Chat.Get(ctx, larkim.NewGetChatReqBuilder().ChatId(id).UserIdType("open_id").Build())
	if err != nil {
		return err
	}
	if !r.Success() || r.Data == nil {
		return fmt.Errorf("无法读取路由群，错误码 %d: %s", r.Code, r.Msg)
	}
	d := r.Data
	if d.Description == nil || *d.Description != c.routingDescription() || d.ChatMode == nil || *d.ChatMode != "group" || d.ChatType == nil || *d.ChatType != "private" || (d.OwnerId != nil && *d.OwnerId != "") || (d.ChatStatus != nil && *d.ChatStatus != "normal") {
		return errors.New("路由群绑定描述、状态或机器人群主不匹配")
	}
	return nil
}

func (c *Client) encodeRelay(action CardAction) (string, error) {
	target, _ := action.Value["machine_id"].(string)
	request, _ := action.Value["request_id"].(string)
	decision, _ := action.Value["decision"].(string)
	if action.EventID == "" || action.ChatID == "" || action.SenderID != c.ownerID || target == "" || request == "" || (decision != "accept" && decision != "decline") {
		return "", errors.New("无效或未经授权的审批路由事件")
	}
	// Only approval identifiers and decisions are transmitted; arbitrary form
	// values, callback tokens and user prompts never enter the routing group.
	action.Value = map[string]any{"machine_id": target, "request_id": request, "decision": decision}
	payload, err := json.Marshal(action)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, c.relayKey)
	mac.Write(payload)
	data, err := json.Marshal(relayPacket{Version: 1, Action: action, Signature: hex.EncodeToString(mac.Sum(nil))})
	return string(data), err
}

func (c *Client) decodeRelay(text string) (CardAction, bool) {
	var p relayPacket
	if json.Unmarshal([]byte(text), &p) != nil || p.Version != 1 {
		return CardAction{}, false
	}
	payload, err := json.Marshal(p.Action)
	if err != nil {
		return CardAction{}, false
	}
	signature, err := hex.DecodeString(p.Signature)
	if err != nil {
		return CardAction{}, false
	}
	mac := hmac.New(sha256.New, c.relayKey)
	mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) || p.Action.SenderID != c.ownerID {
		return CardAction{}, false
	}
	target, _ := p.Action.Value["machine_id"].(string)
	if target != c.machineID {
		return CardAction{}, false
	}
	if _, err := c.encodeRelay(p.Action); err != nil {
		return CardAction{}, false
	}
	return p.Action, true
}

func (c *Client) RelayCardAction(ctx context.Context, action CardAction) error {
	if action.SenderID != c.ownerID {
		return nil
	}
	text, err := c.encodeRelay(action)
	if err != nil {
		return err
	}
	// EnsureRoutingChat has already populated this before the WS connection;
	// avoid several discovery calls inside Feishu's short callback deadline.
	c.routingMu.Lock()
	chatID := c.routingChatID
	c.routingMu.Unlock()
	if chatID == "" {
		chatID, err = c.EnsureRoutingChat(ctx)
		if err != nil {
			return err
		}
	}
	content, _ := json.Marshal(map[string]string{"text": text})
	sum := sha256.Sum256([]byte("codex-card:" + action.EventID))
	body := larkim.NewCreateMessageReqBodyBuilder().ReceiveId(chatID).MsgType("text").Content(string(content)).Uuid(hex.EncodeToString(sum[:16])).Build()
	r, err := c.sdk.RawClient().Im.V1.Message.Create(ctx, larkim.NewCreateMessageReqBuilder().ReceiveIdType("chat_id").Body(body).Build())
	if err != nil {
		return err
	}
	if !r.Success() {
		return fmt.Errorf("持久化审批路由事件失败，错误码 %d: %s", r.Code, r.Msg)
	}
	return nil
}

// ListInbound reads authoritative chat history, independent of which host
// received a load-balanced WebSocket event. The inclusive one-second overlap
// avoids losing messages with equal millisecond timestamps; IDs deduplicate.
func (c *Client) ListInbound(ctx context.Context, chatID string, since int64, mailbox bool) (HistoryBatch, error) {
	batch := HistoryBatch{Cursor: since}
	token := ""
	for page := 0; page < 100; page++ {
		builder := larkim.NewListMessageReqBuilder().ContainerIdType("chat").ContainerId(chatID).SortType("ByCreateTimeAsc").PageSize(50).StartTime(strconv.FormatInt(max(0, since/1000-1), 10))
		if token != "" {
			builder.PageToken(token)
		}
		r, err := c.sdk.RawClient().Im.V1.Message.List(ctx, builder.Build())
		if err != nil {
			return HistoryBatch{}, err
		}
		if !r.Success() || r.Data == nil {
			return HistoryBatch{}, fmt.Errorf("读取飞书消息失败，需消息历史读取权限，错误码 %d: %s", r.Code, r.Msg)
		}
		for _, m := range r.Data.Items {
			if m == nil || m.CreateTime == nil || m.MessageId == nil {
				return HistoryBatch{}, errors.New("飞书消息缺少时间或编号，未推进读取位置")
			}
			millis, err := strconv.ParseInt(*m.CreateTime, 10, 64)
			if err != nil || millis <= 0 {
				return HistoryBatch{}, errors.New("飞书消息时间无效，未推进读取位置")
			}
			batch.Cursor = max(batch.Cursor, millis)
			if millis < max(0, since-1000) || m.Sender == nil || m.Sender.SenderType == nil || m.Deleted != nil && *m.Deleted {
				continue
			}
			text := historyMessageText(m)
			if mailbox {
				if *m.Sender.SenderType != "app" {
					continue
				}
				if action, ok := c.decodeRelay(text); ok {
					batch.Actions = append(batch.Actions, RoutedAction{Action: action, AtMillis: millis})
				}
			} else if *m.Sender.SenderType == "user" && m.Sender.Id != nil && *m.Sender.Id == c.ownerID && strings.TrimSpace(text) != "" {
				if identity := c.sdk.GetBotIdentity(ctx); identity != nil {
					for _, mention := range m.Mentions {
						if mention != nil && mention.Id != nil && *mention.Id == identity.OpenID && mention.Key != nil {
							text = strings.ReplaceAll(text, *mention.Key, "")
						}
					}
				}
				text = strings.TrimSpace(text)
				if text == "" {
					continue
				}
				batch.Messages = append(batch.Messages, Inbound{EventID: "feishu-message:" + *m.MessageId, MessageID: *m.MessageId, ChatID: chatID, SenderID: *m.Sender.Id, Text: strings.TrimSpace(text), CreatedAt: time.UnixMilli(millis)})
			}
		}
		if r.Data.HasMore == nil || !*r.Data.HasMore {
			return batch, nil
		}
		if r.Data.PageToken == nil || *r.Data.PageToken == "" || *r.Data.PageToken == token {
			return HistoryBatch{}, errors.New("消息分页标记无效，未推进读取位置")
		}
		token = *r.Data.PageToken
	}
	return HistoryBatch{}, errors.New("消息历史超过 100 页，未推进读取位置")
}

func historyMessageText(m *larkim.Message) string {
	if m.Body == nil || m.Body.Content == nil || m.MsgType == nil {
		return ""
	}
	if *m.MsgType == "text" {
		var content struct {
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(*m.Body.Content), &content) == nil {
			return content.Text
		}
		return ""
	}
	if *m.MsgType == "post" {
		var body historyPost
		if json.Unmarshal([]byte(*m.Body.Content), &body) == nil && (body.Title != "" || body.Content != nil || body.ContentV2 != nil) {
			return body.text()
		}
		var languages map[string]historyPost
		if json.Unmarshal([]byte(*m.Body.Content), &languages) != nil {
			return ""
		}
		for _, lang := range []string{"zh_cn", "en_us", "ja_jp"} {
			if body, ok := languages[lang]; ok {
				return body.text()
			}
		}
	}
	return ""
}

type historyPost struct {
	Title     string             `json:"title"`
	Content   [][]map[string]any `json:"content"`
	ContentV2 [][]map[string]any `json:"content_v2"`
}

func (p historyPost) text() string {
	lines := []string{}
	if p.Title != "" {
		lines = append(lines, p.Title)
	}
	rows := p.Content
	if len(p.ContentV2) > 0 {
		rows = p.ContentV2
	}
	for _, row := range rows {
		var line strings.Builder
		for _, part := range row {
			if text, ok := part["text"].(string); ok {
				line.WriteString(text)
			}
		}
		lines = append(lines, line.String())
	}
	return strings.Join(lines, "\n")
}
