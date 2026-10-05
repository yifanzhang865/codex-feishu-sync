package bridge

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhangwei/codex-feishu-sync/internal/appserver"
	"github.com/zhangwei/codex-feishu-sync/internal/feishu"
	"github.com/zhangwei/codex-feishu-sync/internal/router"
	"github.com/zhangwei/codex-feishu-sync/internal/state"
)

func controlFixture(t *testing.T) (*Bridge, *discoveryCodex, *observerFeishu) {
	t.Helper()
	b, codex, chat := discoveryFixture(t)
	b.cfg.ReadOnly = false
	b.cfg.AutoDeleteInactiveGroups = true
	b.cfg.GroupIdleHours = 72
	return b, codex, chat
}

func TestControlDiscoveryAndRecoveryDoNotAcquireAnyWriter(t *testing.T) {
	b, codex, chat := controlFixture(t)
	codex.writer = false
	thread := appserver.Thread{ID: "thread-b", Source: json.RawMessage(`"cli"`)}
	codex.listed = append(codex.listed, thread)
	codex.byID[thread.ID] = thread
	if err := b.recoverThreads(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.discoverAllSessions(context.Background(), true)
	b.discoverAllSessions(context.Background(), false)
	if codex.resumes != 0 || len(codex.inputs) != 0 || len(chat.created) != 1 || !b.isObserved("thread-a") || !b.isObserved("thread-b") {
		t.Fatal("startup or discovery acquired writers without a Feishu instruction")
	}
}

func TestFeishuInstructionResumesOnlyItsThreadAndDiscoveryKeepsControl(t *testing.T) {
	b, codex, _ := controlFixture(t)
	codex.writer = false
	other := appserver.Thread{ID: "thread-b", Source: json.RawMessage(`"cli"`)}
	codex.listed = append(codex.listed, other)
	codex.byID[other.ID] = other
	b.discoverAllSessions(context.Background(), true)
	message := feishu.Inbound{EventID: "control-event", ChatID: "chat-a", SenderID: "owner-a", Text: "continue the task"}
	if err := b.onFeishuMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	b.discoverAllSessions(context.Background(), false)
	b.pollObserved(context.Background())
	if err := b.onFeishuMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if codex.resumes != 1 || len(codex.inputs) != 1 || codex.inputs[0] != message.Text || !b.isControlled("thread-a") || b.isObserved("thread-a") {
		t.Fatal("instruction was duplicated or discovery returned a controlled thread to observation")
	}
	if len(codex.resumeThreads) != 1 || codex.resumeThreads[0] != "thread-a" || codex.inputThreads[0] != "thread-a" || !b.isObserved(other.ID) || b.isControlled(other.ID) {
		t.Fatal("Feishu instruction acquired control of another conversation")
	}
}

func TestFeishuInstructionWaitsForCLIExitAndSurvivesRestart(t *testing.T) {
	b, codex, chat := controlFixture(t)
	b.discoverAllSessions(context.Background(), true)
	message := feishu.Inbound{EventID: "queued-control-event", ChatID: "chat-a", SenderID: "owner-a", Text: "continue after CLI exits"}
	if err := b.onFeishuMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if len(codex.inputs) != 0 || len(b.store.QueuedThreads()) != 1 || !strings.Contains(chat.messages[len(chat.messages)-1], "排队") {
		t.Fatal("CLI-owned instruction was not safely queued")
	}
	if err := b.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(filepath.Join(b.configDir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	b.store = store
	b.router = router.New(store, b.cfg.OwnerOpenID)
	b.observed = nil
	b.controlled = nil
	codex.writer = false
	if err := b.recoverThreads(context.Background()); err != nil {
		t.Fatal(err)
	}
	b.pollObserved(context.Background())
	b.flushIdleQueues(context.Background())
	if len(codex.inputs) != 1 || codex.inputs[0] != message.Text || len(b.store.QueuedThreads()) != 0 || !b.isControlled("thread-a") {
		t.Fatal("queued instruction did not resume after CLI exit and bridge restart")
	}
}

func TestAbandonedActiveTurnDoesNotBlockQueuedTakeover(t *testing.T) {
	b, codex, _ := controlFixture(t)
	thread := codex.byID["thread-a"]
	thread.Turns = []json.RawMessage{json.RawMessage(`{"id":"abandoned","status":"inProgress","items":[]}`)}
	codex.byID[thread.ID] = thread
	codex.thread = thread
	b.discoverAllSessions(context.Background(), true)
	message := feishu.Inbound{EventID: "abandoned-event", ChatID: "chat-a", SenderID: "owner-a", Text: "continue"}
	if err := b.onFeishuMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	b.pollObserved(context.Background())
	if len(codex.inputs) != 0 || len(b.store.QueuedThreads()) != 1 {
		t.Fatal("live CLI writer was bypassed")
	}
	codex.writer = false
	b.lastTakeover[thread.ID] = time.Time{}
	b.pollObserved(context.Background())
	b.flushIdleQueues(context.Background())
	if len(codex.inputs) != 1 || len(b.store.QueuedThreads()) != 0 {
		t.Fatal("abandoned persisted turn blocked a released writer")
	}
}

func TestOtherFeishuUsersCannotAcquireWriter(t *testing.T) {
	b, codex, _ := controlFixture(t)
	b.discoverAllSessions(context.Background(), true)
	if err := b.onFeishuMessage(context.Background(), feishu.Inbound{EventID: "not-owner", ChatID: "chat-a", SenderID: "other-user", Text: "execute"}); err != nil {
		t.Fatal(err)
	}
	if codex.resumes != 0 || len(codex.inputs) != 0 || len(b.store.QueuedThreads()) != 0 {
		t.Fatal("non-owner message acquired control")
	}
}

func TestExplicitContinuationOfOldBoundThreadSurvivesDiscovery(t *testing.T) {
	for _, initiallyOccupied := range []bool{false, true} {
		b, codex, _ := controlFixture(t)
		b.cfg.SessionActiveHours = 72
		thread := codex.byID["thread-a"]
		thread.Turns = []json.RawMessage{recentTurn(time.Now().Add(-80*time.Hour), "old-reply")}
		codex.byID[thread.ID] = thread
		codex.thread = thread
		codex.writer = initiallyOccupied
		b.discoverAllSessions(context.Background(), true)
		if b.isObserved(thread.ID) || codex.resumes != 0 {
			t.Fatal("inactive bound thread entered automatic discovery")
		}
		message := feishu.Inbound{EventID: "old-continuation", ChatID: "chat-a", SenderID: "owner-a", Text: "continue this bound conversation"}
		if err := b.onFeishuMessage(context.Background(), message); err != nil {
			t.Fatal(err)
		}
		if initiallyOccupied {
			b.discoverAllSessions(context.Background(), false)
			codex.writer = false
			b.pollObserved(context.Background())
			b.flushIdleQueues(context.Background())
		}
		if len(codex.inputs) != 1 || codex.inputs[0] != message.Text || len(b.store.QueuedThreads()) != 0 || !b.isControlled(thread.ID) {
			t.Fatal("explicit continuation was blocked by the automatic discovery window")
		}
	}
}

func TestClosedControlledThreadReturnsToObservation(t *testing.T) {
	b, codex, _ := controlFixture(t)
	b.setControlled("thread-a", true)
	b.onNotification("thread/closed", json.RawMessage(`{"threadId":"thread-a"}`))
	b.discoverAllSessions(context.Background(), false)
	if b.isControlled("thread-a") || !b.isObserved("thread-a") || codex.resumes != 0 {
		t.Fatal("closed thread retained control or was eagerly resumed")
	}
}

func TestControlModePreservesInactiveGroupCleanup(t *testing.T) {
	b, _, chat, _ := lifecycleFixture(t)
	b.cfg.ReadOnly = false
	b.cleanupInactiveGroups(context.Background(), time.Now())
	if len(chat.deleted) != 1 || len(b.store.Bindings()) != 0 {
		t.Fatal("control mode disabled inactive group cleanup")
	}
}
