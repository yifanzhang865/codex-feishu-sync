package bridge

import (
	"context"
	"log/slog"
	"time"

	"github.com/zhangwei/codex-feishu-sync/internal/appserver"
)

func syncableMainThread(thread appserver.Thread) bool {
	return thread.ID != "" && thread.ParentID == "" && !thread.Ephemeral && supportedSource(thread.SourceKind())
}

// Discovery runs separately so creating groups cannot stall existing replies.
func (b *Bridge) observeAllSessions(ctx context.Context) {
	if b.cfg.AutoDeleteInactiveGroups {
		workerCtx, cancel := context.WithCancel(ctx)
		ctx = workerCtx
		cleanupDone := make(chan struct{})
		go func() {
			defer close(cleanupDone)
			b.cleanupInactiveGroups(ctx, time.Now())
			ticker := time.NewTicker(5 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case now := <-ticker.C:
					b.cleanupInactiveGroups(ctx, now)
				}
			}
		}()
		defer func() { cancel(); <-cleanupDone }()
	}
	b.discoverAllSessions(ctx, true)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.discoverAllSessions(ctx, false)
		}
	}
}

func (b *Bridge) discoverAllSessions(ctx context.Context, initial bool) {
	listCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	threads, err := b.codex.ListThreads(listCtx)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("发现本机 Codex 会话失败", "error", err)
		}
		return
	}
	if initial && b.cfg.AutoCreateGroup {
		// Baseline old history before group creation. Replies completed while
		// later groups are being created remain unmarked and will be delivered.
		for _, thread := range threads {
			if !b.eligibleSession(ctx, thread) || b.store.HistoryInitialized(thread.ID) {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			lock := b.threadLock(thread.ID)
			lock.Lock()
			if !b.store.HistoryInitialized(thread.ID) {
				readCtx, readCancel := context.WithTimeout(ctx, 10*time.Second)
				stored, readErr := b.codex.ReadThread(readCtx, thread.ID)
				readCancel()
				if readErr == nil {
					b.syncHistoryOnResume(ctx, stored, false)
				} else if ctx.Err() == nil {
					slog.Warn("建立会话历史基线失败", "thread_id", thread.ID, "error", readErr)
				}
			}
			lock.Unlock()
		}
	}
	var observed int
	for _, thread := range threads {
		if ctx.Err() != nil {
			return
		}
		if !b.eligibleSession(ctx, thread) {
			b.setObserved(thread.ID, false)
			continue
		}
		if _, bound := b.store.ChatForThread(thread.ID); bound && (b.isObserved(thread.ID) || b.isControlled(thread.ID)) {
			if b.cfg.AutoDeleteInactiveGroups {
				b.recordDialogueActivity(ctx, thread)
			}
			observed++
			continue
		}
		observeCtx, observeCancel := context.WithTimeout(ctx, 30*time.Second)
		err := b.ensureThreadWithHistory(observeCtx, thread, !initial)
		observeCancel()
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("纳入全会话只读同步失败", "thread_id", thread.ID, "error", err)
			}
			continue
		}
		if b.isObserved(thread.ID) {
			observed++
		}
	}
	if initial {
		slog.Info("全会话初次发现完成", "observed_threads", observed, "read_only", b.cfg.ReadOnly)
	}
}
