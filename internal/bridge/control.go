package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/zhangwei/codex-feishu-sync/internal/appserver"
	"github.com/zhangwei/codex-feishu-sync/internal/control"
)

func (b *Bridge) onLocalCLICall(ctx context.Context, owner, method string, raw json.RawMessage) (json.RawMessage, error) {
	var params struct {
		ThreadID string `json:"threadId"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, err
	}
	if params.ThreadID != "" && control.IsWrite(method) {
		lock := b.threadLock(params.ThreadID)
		lock.Lock()
		defer lock.Unlock()
	}
	result, err := b.localControl.Execute(ctx, owner, method, raw)
	if err != nil {
		return nil, err
	}
	if method == "thread/start" || method == "thread/resume" || method == "thread/fork" {
		var response struct {
			Thread appserver.Thread `json:"thread"`
		}
		if err := json.Unmarshal(result, &response); err != nil {
			return nil, err
		}
		thread := response.Thread
		if thread.ID == "" {
			return nil, fmt.Errorf("%s 未返回 thread ID", method)
		}
		lock := b.threadLock(thread.ID)
		lock.Lock()
		defer lock.Unlock()
		b.localControl.Manage(thread)
		b.rememberThread(thread)
		b.setControlled(thread.ID, true)
		if _, bound := b.store.ChatForThread(thread.ID); !bound && b.cfg.AutoCreateGroup && !thread.Ephemeral {
			if err := b.createBinding(ctx, thread); err != nil {
				// An unavailable Feishu API should not prevent local CLI work.
				slog.Warn("创建 CLI 会话飞书群失败，将由会话发现重试", "thread_id", thread.ID, "error", err)
			}
		}
		b.syncHistoryOnResume(ctx, thread, false)
	}
	return result, nil
}

// Caller holds the bridge thread lock. Old queued commands must not regain
// control after the new sender's turn completes.
func (b *Bridge) onControlTakeover(ctx context.Context, threadID, owner string) error {
	queued, err := b.store.Dequeue(threadID)
	if err != nil {
		return err
	}
	message := fmt.Sprintf("控制权已交给 %s；另一端继续只读同步回复，提交新指令可再次接管。", control.OwnerLabel(owner))
	if len(queued) != 0 {
		message += fmt.Sprintf("\n已取消旧端尚未执行的 %d 条排队指令。", len(queued))
	}
	if chat := b.chatFor(threadID); chat != "" {
		if err := b.feishu.SendText(ctx, chat, message); err != nil {
			slog.Warn("发送控制权交接通知失败", "thread_id", threadID, "error", err)
		}
	}
	return nil
}
