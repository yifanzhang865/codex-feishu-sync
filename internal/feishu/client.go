package feishu

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	channel "github.com/larksuite/channel-sdk-go"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	"github.com/zhangwei/codex-feishu-sync/internal/config"
)

type Inbound struct {
	EventID   string
	MessageID string
	ChatID    string
	SenderID  string
	Text      string
	CreatedAt time.Time
}

type CardAction struct {
	EventID  string
	ChatID   string
	SenderID string
	Value    map[string]any
}

type MessageHandler func(context.Context, Inbound) error
type CardActionHandler func(context.Context, CardAction) error

type Client struct {
	sdk       channel.Channel
	ownerID   string
	messageFn MessageHandler
	cardFn    CardActionHandler
}

func New(cfg config.Config, credentials config.Credentials) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if credentials.AppID == "" || credentials.AppSecret == "" {
		return nil, errors.New("飞书应用凭据不完整")
	}
	domain := "https://open.feishu.cn"
	if cfg.Region == "lark" {
		domain = "https://open.larksuite.com"
	}
	requireMention := false
	sdk, err := channel.New(
		credentials.AppID,
		credentials.AppSecret,
		channel.WithDomain(domain),
		channel.WithPolicyConfig(channel.PolicyConfig{
			RequireMention: &requireMention,
			DMMode:         "disabled",
		}),
	)
	if err != nil {
		return nil, err
	}
	client := &Client{sdk: sdk, ownerID: cfg.OwnerOpenID}
	sdk.OnMessage(func(ctx context.Context, message *channel.NormalizedMessage) error {
		if client.messageFn == nil {
			return nil
		}
		return client.messageFn(ctx, Inbound{
			EventID: message.EventID, MessageID: message.MessageID, ChatID: message.ChatID,
			SenderID: message.UserID, Text: message.Content,
			CreatedAt: time.UnixMilli(message.CreateTimeMs),
		})
	})
	sdk.OnCardAction(func(ctx context.Context, event *channel.CardActionEvent) error {
		if client.cardFn == nil {
			return nil
		}
		return client.cardFn(ctx, CardAction{
			EventID: event.EventID, ChatID: event.ChatID, SenderID: event.Operator.OpenID, Value: event.Action.Value,
		})
	})
	return client, nil
}

func (c *Client) OnMessage(handler MessageHandler) {
	c.messageFn = handler
}

func (c *Client) OnCardAction(handler CardActionHandler) {
	c.cardFn = handler
}

func (c *Client) Start(ctx context.Context) error {
	return c.sdk.Start(ctx)
}

func (c *Client) Stop(ctx context.Context) error {
	return c.sdk.Stop(ctx)
}

func (c *Client) Check(ctx context.Context) error {
	identity := c.sdk.GetBotIdentity(ctx)
	if identity == nil || identity.OpenID == "" {
		return errors.New("无法读取机器人身份，请检查机器人能力和通讯录权限")
	}
	return nil
}

func (c *Client) SendText(ctx context.Context, chatID, text string) error {
	if chatID == "" || strings.TrimSpace(text) == "" {
		return errors.New("chat_id 和消息内容不能为空")
	}
	_, err := c.sdk.Send(ctx, &channel.SendInput{ReceiveID: chatID, MsgType: "text", Text: text})
	return err
}

func (c *Client) SendCard(ctx context.Context, chatID, card string) error {
	if chatID == "" || card == "" {
		return errors.New("chat_id 和卡片内容不能为空")
	}
	_, err := c.sdk.Send(ctx, &channel.SendInput{ReceiveID: chatID, MsgType: "interactive", Card: card})
	return err
}

func (c *Client) StartMarkdownStream(ctx context.Context, chatID, title, initial string) (channel.StreamController, error) {
	if chatID == "" {
		return nil, errors.New("chat_id 不能为空")
	}
	return c.sdk.Stream(ctx, &channel.SendInput{ReceiveID: chatID, Title: title, Markdown: initial})
}

func (c *Client) CreateThreadChat(ctx context.Context, threadID, threadName, ownerID string) (string, error) {
	if threadID == "" || ownerID == "" {
		return "", errors.New("thread_id 和 owner_open_id 不能为空")
	}
	body := threadChatBody(threadID, threadName, ownerID)
	request := larkim.NewCreateChatReqBuilder().UserIdType("open_id").Body(body).Build()
	response, err := c.sdk.RawClient().Im.V1.Chat.Create(ctx, request)
	if err != nil {
		return "", fmt.Errorf("创建飞书群聊失败: %w", err)
	}
	if !response.Success() || response.Data == nil || response.Data.ChatId == nil || *response.Data.ChatId == "" {
		return "", fmt.Errorf("创建飞书群聊失败，错误码 %d: %s", response.Code, response.Msg)
	}
	return *response.Data.ChatId, nil
}

func threadChatBody(threadID, threadName, ownerID string) *larkim.CreateChatReqBody {
	name := "Codex - " + strings.TrimSpace(threadName)
	if strings.TrimSpace(threadName) == "" {
		name = "Codex - " + threadID[:min(len(threadID), 8)]
	}
	if len([]rune(name)) > 60 {
		name = string([]rune(name)[:60])
	}
	return larkim.NewCreateChatReqBodyBuilder().
		Name(name).
		Description("Codex thread: " + threadID).
		ChatMode("group").
		ChatType("private").
		UserIdList([]string{ownerID}).
		Build()
}

func (c *Client) OwnerID() string {
	return c.ownerID
}
