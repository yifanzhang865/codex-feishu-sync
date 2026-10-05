package bridge

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

func isActiveWriterError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already has an active writer")
}

func (b *Bridge) isObserved(threadID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.observed[threadID]
}

func (b *Bridge) setObserved(threadID string, observed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.observed == nil {
		b.observed = make(map[string]bool)
	}
	if observed {
		b.observed[threadID] = true
		delete(b.controlled, threadID)
	} else {
		delete(b.observed, threadID)
		delete(b.lastTakeover, threadID)
	}
}

func (b *Bridge) isControlled(threadID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.controlled[threadID]
}

func (b *Bridge) setControlled(threadID string, controlled bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if controlled {
		if b.controlled == nil {
			b.controlled = make(map[string]bool)
		}
		b.controlled[threadID] = true
		delete(b.observed, threadID)
		delete(b.lastTakeover, threadID)
	} else {
		delete(b.controlled, threadID)
	}
}

// pollObserved forwards completed persisted turns without loading a second
// writer. The existing item IDs provide replay and restart deduplication.
func (b *Bridge) pollObserved(ctx context.Context) {
	b.mu.Lock()
	threadIDs := make([]string, 0, len(b.observed))
	for threadID := range b.observed {
		threadIDs = append(threadIDs, threadID)
	}
	b.mu.Unlock()
	queued := make(map[string]bool)
	for _, threadID := range b.store.QueuedThreads() {
		queued[threadID] = true
	}
	for _, threadID := range threadIDs {
		if ctx.Err() != nil {
			return
		}
		func() {
			lock := b.threadLock(threadID)
			lock.Lock()
			defer lock.Unlock()
			if !b.isObserved(threadID) {
				return
			}
			readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			thread, err := b.codex.ReadThread(readCtx, threadID)
			if err != nil {
				slog.Warn("读取 CLI 会话回复失败", "thread_id", threadID, "error", err)
				return
			}
			b.rememberThread(thread)
			if b.cfg.SessionActiveHours > 0 || b.cfg.AutoDeleteInactiveGroups {
				b.recordDialogueActivity(readCtx, thread)
			}
			b.syncHistoryOnResume(readCtx, thread, false)
			// A killed CLI can leave an in-progress marker in its rollout.
			// Resume is the authoritative writer check, even for such a marker.
			if !b.cfg.ReadOnly && queued[threadID] {
				b.mu.Lock()
				if b.lastTakeover == nil {
					b.lastTakeover = make(map[string]time.Time)
				}
				due := time.Since(b.lastTakeover[threadID]) >= 10*time.Second
				if due {
					b.lastTakeover[threadID] = time.Now()
				}
				b.mu.Unlock()
				if due {
					if err := b.takeOverObserved(readCtx, threadID); err != nil && !isActiveWriterError(err) {
						slog.Warn("接管已释放的 Codex 会话失败", "thread_id", threadID, "error", err)
					}
				}
			}
		}()
	}
}

// Caller holds the thread lock. Do not acquire another writer unless an owner
// has explicitly sent a queued instruction from Feishu.
func (b *Bridge) takeOverObserved(ctx context.Context, threadID string) error {
	if b.cfg.ReadOnly {
		return errReadOnly
	}
	thread, err := b.codex.ResumeThread(ctx, threadID)
	if err != nil {
		return err
	}
	b.setControlled(threadID, true)
	b.rememberThread(thread)
	b.syncHistoryOnResume(ctx, thread, false)
	slog.Info("CLI 已释放写入权限，飞书可继续控制会话", "thread_id", threadID)
	return nil
}
