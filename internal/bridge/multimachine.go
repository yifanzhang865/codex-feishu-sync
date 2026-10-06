package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/zhangwei/codex-feishu-sync/internal/feishu"
	"github.com/zhangwei/codex-feishu-sync/internal/state"
)

type machineTransport interface {
	EnsureRoutingChat(context.Context) (string, error)
	ListInbound(context.Context, string, int64, bool) (feishu.HistoryBatch, error)
}

func (b *Bridge) runMachinePolling(ctx context.Context, transport machineTransport, mailbox string) {
	seconds := b.cfg.MessagePollSeconds
	if seconds == 0 {
		seconds = 5
	}
	ticker := time.NewTicker(time.Duration(seconds) * time.Second)
	defer ticker.Stop()
	for {
		b.pollMachineMessages(ctx, transport, mailbox)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (b *Bridge) pollMachineMessages(ctx context.Context, transport machineTransport, mailbox string) {
	b.recoverDeliveries(ctx)
	b.drainDeliveries(ctx)
	chats := map[string]bool{}
	for _, chatID := range b.store.Bindings() {
		chats[chatID] = false
	}
	if !b.cfg.ReadOnly {
		chats[mailbox] = true
	}
	for chatID, isMailbox := range chats {
		if ctx.Err() != nil {
			return
		}
		since, ok := b.store.FeishuCursor(chatID)
		if !ok {
			if err := b.store.SetFeishuCursor(chatID, time.Now().UnixMilli()); err != nil {
				slog.Error("保存消息读取基线失败", "error", err)
				continue
			}
			since, _ = b.store.FeishuCursor(chatID)
		}
		pollCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		batch, err := transport.ListInbound(pollCtx, chatID, since, isMailbox)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("读取跨机器消息失败", "chat_id", chatID, "error", err)
			}
			continue
		}
		failed := false
		for _, message := range batch.Messages {
			data, _ := json.Marshal(message)
			if err := b.store.QueueDelivery(state.Delivery{ID: message.EventID, Kind: "message", ChatID: chatID, Payload: data}); err != nil {
				slog.Error("持久化群指令失败", "error", err)
				failed = true
				break
			}
		}
		if !failed {
			for _, action := range batch.Actions {
				data, _ := json.Marshal(action.Action)
				if err := b.store.QueueDelivery(state.Delivery{ID: action.Action.EventID, Kind: "card", ChatID: action.Action.ChatID, Payload: data}); err != nil {
					slog.Error("持久化审批事件失败", "error", err)
					failed = true
					break
				}
			}
		}
		if !failed {
			if err := b.store.SetFeishuCursor(chatID, batch.Cursor); err != nil {
				slog.Error("保存消息读取位置失败", "error", err)
			}
		}
		b.drainDeliveries(ctx)
	}
}

func (b *Bridge) recoverDeliveries(ctx context.Context) {
	for _, d := range b.store.Deliveries() {
		if d.Status != "processing" && d.Status != "uncertain" {
			continue
		}
		// A crash after StartTurn but before recording its result is ambiguous.
		// Do not replay an instruction that could already be executing.
		if err := b.store.SetDeliveryStatus(d.ID, "uncertain"); err != nil {
			continue
		}
		if err := b.feishu.SendText(ctx, d.ChatID, "服务曾在处理此消息时中断，执行结果待确认。请核对本机 Codex 后重新发送需要继续的指令；旧审批请重新发起。"); err != nil {
			continue
		}
		if err := b.store.FinishDelivery(d.ID); err != nil {
			slog.Error("保存待确认消息状态失败", "error", err)
		}
	}
}

func (b *Bridge) drainDeliveries(ctx context.Context) {
	failedChats := make(map[string]bool)
	for _, d := range b.store.Deliveries() {
		if ctx.Err() != nil {
			return
		}
		if d.Status != "pending" || failedChats[d.ChatID] {
			continue
		}
		if err := b.store.SetDeliveryStatus(d.ID, "processing"); err != nil {
			slog.Error("记录消息处理状态失败", "error", err)
			return
		}
		var err error
		switch d.Kind {
		case "message":
			var m feishu.Inbound
			if err = json.Unmarshal(d.Payload, &m); err == nil {
				err = b.onFeishuMessage(ctx, m)
			}
		case "card":
			var a feishu.CardAction
			if err = json.Unmarshal(d.Payload, &a); err == nil {
				err = b.onCardAction(ctx, a)
			}
		default:
			err = errors.New("unknown delivery kind")
		}
		if err != nil {
			failedChats[d.ChatID] = true
			_ = b.store.SetDeliveryStatus(d.ID, "pending")
			slog.Warn("处理持久化消息失败，稍后重试", "error", err)
			continue
		}
		if err := b.store.FinishDelivery(d.ID); err != nil {
			slog.Error("保存消息处理结果失败", "error", err)
			return
		}
	}
}
