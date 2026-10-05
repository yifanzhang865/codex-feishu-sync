package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/zhangwei/codex-feishu-sync/internal/appserver"
	"github.com/zhangwei/codex-feishu-sync/internal/feishu"
)

func (b *Bridge) dialogueActivity(ctx context.Context, thread appserver.Thread) (time.Time, error) {
	b.mu.Lock()
	if b.activityTracker == nil {
		b.activityTracker = &appserver.ActivityTracker{}
	}
	tracker := b.activityTracker
	b.mu.Unlock()
	return tracker.LastDialogue(ctx, thread)
}

func (b *Bridge) eligibleSession(ctx context.Context, thread appserver.Thread) bool {
	if !syncableMainThread(thread) {
		return false
	}
	if b.cfg.SessionActiveHours == 0 {
		return true
	}
	activity, err := b.dialogueActivity(ctx, thread)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("读取会话活动时间失败，保留现有群", "thread_id", thread.ID, "error", err)
		}
		return false
	}
	if thread.IsBusy() && !activity.IsZero() {
		return true
	}
	return !activity.IsZero() && !activity.Before(time.Now().Add(-time.Duration(b.cfg.SessionActiveHours)*time.Hour))
}

func (b *Bridge) recordDialogueActivity(ctx context.Context, thread appserver.Thread) {
	activity, err := b.dialogueActivity(ctx, thread)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("读取会话活动时间失败，暂缓清理", "thread_id", thread.ID, "error", err)
		}
		return
	}
	if err := b.store.RecordActivity(thread.ID, activity); err != nil {
		slog.Warn("保存会话活动时间失败", "thread_id", thread.ID, "error", err)
	}
}

func (b *Bridge) cleanupInactiveGroups(ctx context.Context, now time.Time) {
	if !b.cfg.AutoDeleteInactiveGroups {
		return
	}
	cutoff := now.Add(-time.Duration(b.cfg.GroupIdleHours) * time.Hour)
	for threadID, chat := range b.store.ManagedChats() {
		if ctx.Err() != nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		err := b.cleanupInactiveGroup(cleanupCtx, threadID, chat.ChatID, cutoff)
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Warn("回收过期飞书群失败，保留绑定并稍后重试", "thread_id", threadID, "error", err)
		}
		if errors.Is(err, feishu.ErrPermissionDenied) {
			return
		}
	}
}

func (b *Bridge) cleanupInactiveGroup(ctx context.Context, threadID, chatID string, cutoff time.Time) error {
	lock := b.threadLock(threadID)
	lock.Lock()
	defer lock.Unlock()
	managed, ok := b.store.ManagedChats()[threadID]
	if !ok || managed.ChatID != chatID {
		return nil
	}
	if managed.DeletePending {
		return b.finishDeletedChat(threadID, chatID)
	}
	if managed.DeleteStarted {
		_, err := b.feishu.ThreadChatCanBeDeleted(ctx, chatID, threadID)
		if errors.Is(err, feishu.ErrChatDissolved) {
			return b.confirmDeletedChat(threadID, chatID)
		}
		if err != nil {
			return err
		}
		if err := b.store.CancelChatDeletion(threadID, chatID); err != nil {
			return err
		}
	}
	if !managed.LastActivity.IsZero() && !managed.LastActivity.Before(cutoff) {
		return nil
	}
	for _, queued := range b.store.QueuedThreads() {
		if queued == threadID {
			return nil
		}
	}
	thread, err := b.codex.ReadThread(ctx, threadID)
	if err != nil {
		return err
	}
	if thread.IsBusy() {
		return nil
	}
	activity, err := b.dialogueActivity(ctx, thread)
	if err != nil {
		return err
	}
	if activity.IsZero() {
		return nil
	}
	if err := b.store.RecordActivity(threadID, activity); err != nil {
		return err
	}
	if !activity.Before(cutoff) {
		return nil
	}
	allowed, err := b.feishu.ThreadChatCanBeDeleted(ctx, chatID, threadID)
	if errors.Is(err, feishu.ErrChatDissolved) {
		return b.confirmDeletedChat(threadID, chatID)
	}
	if err != nil {
		return err
	}
	if !allowed {
		return nil
	}
	humanActivity, err := b.feishu.LastHumanMessage(ctx, chatID, cutoff)
	if err != nil {
		return err
	}
	if err := b.store.RecordActivity(threadID, humanActivity); err != nil {
		return err
	}
	if !humanActivity.Before(cutoff) {
		return nil
	}
	// Recheck CLI activity after network requests; a conversation may have
	// resumed while its Feishu group history was being read.
	thread, err = b.codex.ReadThread(ctx, threadID)
	if err != nil {
		return err
	}
	if thread.IsBusy() {
		return nil
	}
	activity, err = b.dialogueActivity(ctx, thread)
	if err != nil {
		return err
	}
	if activity.IsZero() || !activity.Before(cutoff) {
		return nil
	}
	if err := b.store.BeginChatDeletion(threadID, chatID); err != nil {
		return err
	}
	if err := b.feishu.DeleteThreadChat(ctx, chatID); err != nil {
		return fmt.Errorf("解散群：%w", err)
	}
	return b.confirmDeletedChat(threadID, chatID)
}

func (b *Bridge) confirmDeletedChat(threadID, chatID string) error {
	if err := b.store.MarkChatDeleted(threadID, chatID); err != nil {
		return err
	}
	return b.finishDeletedChat(threadID, chatID)
}

func (b *Bridge) finishDeletedChat(threadID, chatID string) error {
	if err := b.store.UnbindDeletedChat(threadID, chatID); err != nil {
		return err
	}
	b.setObserved(threadID, false)
	b.setControlled(threadID, false)
	slog.Info("已解散三天未对话的飞书群", "thread_id", threadID, "chat_id", chatID)
	return nil
}
