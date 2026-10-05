package feishu

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

var ErrPermissionDenied = errors.New("飞书权限不足，请开通 im:chat:read 和 im:chat:delete 并发布应用版本")
var ErrChatDissolved = errors.New("飞书群已解散")

func lifecycleAPIError(code int, message string) error {
	if code == 99991672 {
		return fmt.Errorf("%w: %s", ErrPermissionDenied, message)
	}
	if code == 232009 {
		return ErrChatDissolved
	}
	return fmt.Errorf("飞书群管理错误 %d: %s", code, message)
}

func (c *Client) ThreadChatCanBeDeleted(ctx context.Context, chatID, threadID string) (bool, error) {
	response, err := c.sdk.RawClient().Im.V1.Chat.Get(ctx, larkim.NewGetChatReqBuilder().ChatId(chatID).UserIdType("open_id").Build())
	if err != nil {
		return false, err
	}
	if !response.Success() {
		return false, lifecycleAPIError(response.Code, response.Msg)
	}
	if response.Data == nil {
		return false, errors.New("飞书未返回群信息")
	}
	return matchesManagedThreadChat(response.Data, threadID), nil
}

func matchesManagedThreadChat(chat *larkim.GetChatRespData, threadID string) bool {
	// Feishu omits owner_id when a bot owns the group. Never dismiss a
	// group transferred to a human, or whose binding description changed.
	return chat != nil && threadID != "" && chat.Description != nil && *chat.Description == "Codex thread: "+threadID &&
		chat.ChatMode != nil && *chat.ChatMode == "group" && chat.ChatType != nil && *chat.ChatType == "private" &&
		(chat.OwnerId == nil || *chat.OwnerId == "")
}

func (c *Client) LastHumanMessage(ctx context.Context, chatID string, since time.Time) (time.Time, error) {
	pageToken := ""
	for page := 0; page < 100; page++ {
		builder := larkim.NewListMessageReqBuilder().ContainerIdType("chat").ContainerId(chatID).
			StartTime(strconv.FormatInt(since.Unix(), 10)).SortType("ByCreateTimeDesc").PageSize(50)
		if pageToken != "" {
			builder.PageToken(pageToken)
		}
		response, err := c.sdk.RawClient().Im.V1.Message.List(ctx, builder.Build())
		if err != nil {
			return time.Time{}, err
		}
		if !response.Success() {
			return time.Time{}, lifecycleAPIError(response.Code, response.Msg)
		}
		if response.Data == nil {
			return time.Time{}, errors.New("飞书未返回群消息历史")
		}
		for _, message := range response.Data.Items {
			at, err := humanMessageTime(message)
			if err != nil {
				return time.Time{}, err
			}
			if !at.IsZero() {
				return at, nil
			}
		}
		if response.Data.HasMore == nil || !*response.Data.HasMore {
			return time.Time{}, nil
		}
		if response.Data.PageToken == nil || *response.Data.PageToken == "" || *response.Data.PageToken == pageToken {
			return time.Time{}, errors.New("飞书消息历史分页标记无效，暂缓回收")
		}
		pageToken = *response.Data.PageToken
	}
	return time.Time{}, errors.New("飞书消息历史超过 100 页，暂缓回收")
}

func humanMessageTime(message *larkim.Message) (time.Time, error) {
	if message == nil {
		return time.Time{}, errors.New("飞书返回空消息，暂缓回收")
	}
	if message.MsgType != nil && *message.MsgType == "system" {
		return time.Time{}, nil
	}
	if message.Sender == nil || message.Sender.SenderType == nil {
		return time.Time{}, errors.New("飞书消息缺少发送者类型，暂缓回收")
	}
	if *message.Sender.SenderType == "app" {
		return time.Time{}, nil
	}
	if message.CreateTime == nil {
		return time.Time{}, errors.New("飞书用户消息缺少时间，暂缓回收")
	}
	milliseconds, err := strconv.ParseInt(*message.CreateTime, 10, 64)
	if err != nil || milliseconds <= 0 {
		return time.Time{}, errors.New("飞书用户消息时间无效，暂缓回收")
	}
	// Anonymous/unknown senders are treated as human for conservative cleanup.
	return time.UnixMilli(milliseconds), nil
}

func (c *Client) DeleteThreadChat(ctx context.Context, chatID string) error {
	response, err := c.sdk.RawClient().Im.V1.Chat.Delete(ctx, larkim.NewDeleteChatReqBuilder().ChatId(chatID).Build())
	if err != nil {
		return err
	}
	if !response.Success() && response.Code != 232009 {
		return lifecycleAPIError(response.Code, response.Msg)
	}
	return nil
}
