package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/zhangwei/codex-feishu-sync/internal/appserver"
	"github.com/zhangwei/codex-feishu-sync/internal/feishu"
	"github.com/zhangwei/codex-feishu-sync/internal/hooks"
	"github.com/zhangwei/codex-feishu-sync/internal/state"
)

func recentTurn(at time.Time, itemID string) json.RawMessage {
	value, _ := json.Marshal(map[string]any{"startedAt": at.Unix(), "status": "completed", "items": []map[string]any{{"id": itemID, "type": "agentMessage", "text": itemID}}})
	return value
}

func TestRecentWindowExcludesOldHooksAndRecreatesReactivatedSession(t *testing.T) {
	b, codex, chat := discoveryFixture(t)
	b.cfg.SessionActiveHours = 72
	old := time.Now().Add(-73 * time.Hour)
	thread := appserver.Thread{ID: "old-thread", SessionID: "old-session", CWD: "/old", Source: json.RawMessage(`"cli"`), UpdatedAt: time.Now().Unix(), Turns: []json.RawMessage{recentTurn(old, "old-reply")}}
	codex.byID[thread.ID] = thread
	codex.listed = []appserver.Thread{thread}
	b.discoverAllSessions(context.Background(), true)
	if err := b.registerSession(hooks.Registration{ThreadID: thread.ID, SessionID: thread.SessionID, CWD: thread.CWD}); err != nil {
		t.Fatal(err)
	}
	if len(chat.created) != 0 {
		t.Fatal("resume hook or updated metadata created an inactive group")
	}
	thread.Turns = append(thread.Turns, recentTurn(time.Now(), "reactivated-reply"))
	codex.byID[thread.ID] = thread
	codex.listed[0] = thread
	b.discoverAllSessions(context.Background(), false)
	if len(chat.created) != 1 || !b.isObserved(thread.ID) {
		t.Fatal("reactivated session was missed")
	}
	if codex.resumes != 0 || len(codex.inputs) != 0 {
		t.Fatal("recent discovery acquired a writer")
	}
}

func lifecycleFixture(t *testing.T) (*Bridge, *discoveryCodex, *observerFeishu, time.Time) {
	t.Helper()
	b, codex, chat := discoveryFixture(t)
	b.cfg.SessionActiveHours = 72
	b.cfg.GroupIdleHours = 72
	b.cfg.AutoDeleteInactiveGroups = true
	old := time.Now().Add(-80 * time.Hour).UTC().Truncate(time.Second)
	thread := codex.byID["thread-a"]
	thread.Turns = []json.RawMessage{recentTurn(old, "old")}
	codex.byID[thread.ID] = thread
	codex.listed = []appserver.Thread{thread}
	if err := b.store.BindManaged(thread.ID, "chat-a"); err != nil {
		t.Fatal(err)
	}
	if err := b.store.RecordActivity(thread.ID, old); err != nil {
		t.Fatal(err)
	}
	if err := b.store.InitializeHistory(thread.ID, []string{"codex-item:thread-a:old"}); err != nil {
		t.Fatal(err)
	}
	return b, codex, chat, old
}

func TestExpiredGroupRemovedOnceAndNewDialogueCreatesNewGroup(t *testing.T) {
	b, codex, chat, _ := lifecycleFixture(t)
	b.cleanupInactiveGroups(context.Background(), time.Now())
	b.cleanupInactiveGroups(context.Background(), time.Now())
	if len(chat.deleted) != 1 || len(b.store.Bindings()) != 0 || !b.store.HistoryInitialized("thread-a") || !b.store.HasEvent("codex-item:thread-a:old") {
		t.Fatal("cleanup duplicated deletion or lost local history")
	}
	b.discoverAllSessions(context.Background(), false)
	if len(chat.created) != 0 {
		t.Fatal("expired conversation immediately recreated its group")
	}
	thread := codex.byID["thread-a"]
	thread.Turns = append(thread.Turns, recentTurn(time.Now(), "new-reply"))
	codex.byID[thread.ID] = thread
	codex.listed[0] = thread
	b.discoverAllSessions(context.Background(), false)
	if len(chat.created) != 1 || !b.isObserved(thread.ID) {
		t.Fatal("new dialogue did not restore synchronization")
	}
}

func TestCleanupProtectsManualActiveBusyQueuedAndChangedGroups(t *testing.T) {
	for _, name := range []string{"manual", "recent-cli", "recent-feishu", "busy", "queued", "changed-group", "activity-during-check", "missing-log"} {
		t.Run(name, func(t *testing.T) {
			b, codex, chat, _ := lifecycleFixture(t)
			thread := codex.byID["thread-a"]
			switch name {
			case "manual":
				if err := b.store.Bind("thread-a", "chat-a"); err != nil {
					t.Fatal(err)
				}
			case "recent-cli":
				thread.Turns = append(thread.Turns, recentTurn(time.Now(), "recent"))
				codex.byID[thread.ID] = thread
			case "recent-feishu":
				chat.humanActivity = time.Now()
			case "busy":
				thread.Status = json.RawMessage(`{"type":"active"}`)
				codex.byID[thread.ID] = thread
			case "queued":
				if err := b.store.PrependQueue(thread.ID, []state.QueuedMessage{{Text: "keep"}}); err != nil {
					t.Fatal(err)
				}
			case "changed-group":
				chat.protectChat = true
			case "missing-log":
				thread.Path = "/missing/codex-rollout.jsonl"
				codex.byID[thread.ID] = thread
			case "activity-during-check":
				chat.onHistory = func() {
					thread.Turns = append(thread.Turns, recentTurn(time.Now(), "during-check"))
					codex.byID[thread.ID] = thread
				}
			}
			b.cleanupInactiveGroups(context.Background(), time.Now())
			if len(chat.deleted) != 0 || len(b.store.Bindings()) != 1 {
				t.Fatal("protected group was removed")
			}
		})
	}
}

func TestDeletionFailureAndCrashKeepBindingUntilRemoteStateConfirmed(t *testing.T) {
	b, _, chat, _ := lifecycleFixture(t)
	chat.deleteError = errors.New("network timeout")
	b.cleanupInactiveGroups(context.Background(), time.Now())
	if len(b.store.Bindings()) != 1 || !b.store.ManagedChats()["thread-a"].DeleteStarted {
		t.Fatal("failed deletion lost its recoverable binding")
	}
	chat.deleteError = nil
	chat.ownershipError = feishu.ErrChatDissolved
	b.cleanupInactiveGroups(context.Background(), time.Now())
	if len(b.store.Bindings()) != 0 || len(chat.deleted) != 0 {
		t.Fatal("already dissolved group was deleted twice or stayed bound")
	}
}
