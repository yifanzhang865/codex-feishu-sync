package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/zhangwei/codex-feishu-sync/internal/appserver"
	"github.com/zhangwei/codex-feishu-sync/internal/feishu"
	"github.com/zhangwei/codex-feishu-sync/internal/hooks"
	"github.com/zhangwei/codex-feishu-sync/internal/state"
)

type discoveryCodex struct {
	observerCodex
	listed []appserver.Thread
	byID   map[string]appserver.Thread
}

func (c *discoveryCodex) ListThreads(context.Context) ([]appserver.Thread, error) {
	return c.listed, nil
}

func (c *discoveryCodex) ReadThread(_ context.Context, threadID string) (appserver.Thread, error) {
	thread, ok := c.byID[threadID]
	if !ok {
		return appserver.Thread{}, errors.New("thread not retained")
	}
	return thread, nil
}

func completedReply(itemID, text string) json.RawMessage {
	value, _ := json.Marshal(map[string]any{
		"status": "completed",
		"items":  []map[string]any{{"id": itemID, "type": "agentMessage", "text": text}},
	})
	return value
}

func discoveryFixture(t *testing.T) (*Bridge, *discoveryCodex, *observerFeishu) {
	t.Helper()
	b, original, chat := observerFixture(t)
	b.cfg.ReadOnly = true
	b.cfg.SyncAllSessions = true
	b.cfg.AutoCreateGroup = true
	codex := &discoveryCodex{
		observerCodex: *original,
		listed:        []appserver.Thread{original.thread},
		byID:          map[string]appserver.Thread{"thread-a": original.thread},
	}
	b.codex = codex
	return b, codex, chat
}

func TestAllSessionDiscoveryFindsIdleAndNewThreadsWithoutAcquiringWriters(t *testing.T) {
	b, codex, chat := discoveryFixture(t)
	first := codex.byID["thread-a"]
	first.Turns = []json.RawMessage{completedReply("old-a", "old answer A")}
	second := appserver.Thread{ID: "thread-b", SessionID: "session-b", CWD: "/work/b", Source: json.RawMessage(`"vscode"`), Status: json.RawMessage(`{"type":"notLoaded"}`), Turns: []json.RawMessage{completedReply("old-b", "old answer B")}}
	codex.byID[first.ID] = first
	codex.byID[second.ID] = second
	codex.listed = []appserver.Thread{
		first, second,
		{ID: "guardian", Source: json.RawMessage(`{"subagent":{"other":"guardian"}}`)},
		{ID: "child", ParentID: first.ID, Source: json.RawMessage(`"cli"`)},
		{ID: "ephemeral", Ephemeral: true, Source: json.RawMessage(`"cli"`)},
	}
	if err := b.resume(context.Background(), first.ID, false); err != nil {
		t.Fatal(err)
	}
	b.discoverAllSessions(context.Background(), true)
	if len(b.store.Bindings()) != 2 || len(chat.created) != 1 || !b.isObserved(second.ID) {
		t.Fatalf("idle thread discovery failed: %#v, %#v", b.store.Bindings(), chat.created)
	}
	for _, message := range chat.messages {
		if strings.Contains(message, "old answer") {
			t.Fatal("old history was replayed during initial all-session discovery")
		}
	}
	first.Turns = append(first.Turns, completedReply("new-a", "new answer A"))
	second.Turns = append(second.Turns, completedReply("new-b", "new answer B"))
	codex.byID[first.ID] = first
	codex.byID[second.ID] = second
	b.pollObserved(context.Background())
	want := map[string]string{"chat-a": "new answer A", "chat-thread-b": "new answer B"}
	for index, message := range chat.messages {
		if text, ok := want[chat.chatIDs[index]]; ok && strings.Contains(message, text) {
			delete(want, chat.chatIDs[index])
		}
	}
	if len(want) != 0 {
		t.Fatalf("replies not routed to their own chats: %#v", want)
	}
	delivered := len(chat.messages)
	b.pollObserved(context.Background())
	b.discoverAllSessions(context.Background(), false)
	if len(chat.messages) != delivered || len(chat.created) != 1 {
		t.Fatal("polling or discovery duplicated a reply or group")
	}
	newThread := appserver.Thread{ID: "thread-new", Source: json.RawMessage(`"cli"`), Turns: []json.RawMessage{completedReply("first-new", "first reply without a hook")}}
	codex.listed = append(codex.listed, newThread)
	codex.byID[newThread.ID] = newThread
	b.discoverAllSessions(context.Background(), false)
	if !b.isObserved(newThread.ID) || !strings.Contains(chat.messages[len(chat.messages)-2], "first reply without a hook") {
		t.Fatal("new thread first reply was missed without SessionStart")
	}
	if codex.resumes != 0 || len(codex.inputs) != 0 || codex.interrupts != 0 {
		t.Fatal("all-session read-only sync acquired a writer or submitted input")
	}
}

func TestInitialDiscoveryPreservesRepliesCompletedDuringGroupCreation(t *testing.T) {
	b, codex, chat := discoveryFixture(t)
	thread := appserver.Thread{ID: "thread-b", Source: json.RawMessage(`"cli"`), Turns: []json.RawMessage{completedReply("old", "old answer")}}
	codex.listed = []appserver.Thread{thread}
	codex.byID[thread.ID] = thread
	chat.onCreate = func(threadID string) {
		current := codex.byID[threadID]
		current.Turns = append(current.Turns, completedReply("during-create", "answer completed while creating group"))
		codex.byID[threadID] = current
	}
	b.discoverAllSessions(context.Background(), true)
	if len(chat.messages) != 3 || !strings.Contains(chat.messages[1], "answer completed while creating group") {
		t.Fatalf("reply arriving during creation was discarded: %#v", chat.messages)
	}
}

func TestConcurrentDiscoveryAndRegistrationCreateOneGroup(t *testing.T) {
	b, codex, chat := discoveryFixture(t)
	thread := appserver.Thread{ID: "thread-b", SessionID: "session-b", CWD: "/work/b", Source: json.RawMessage(`"cli"`)}
	codex.listed = []appserver.Thread{thread}
	codex.byID[thread.ID] = thread
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		b.discoverAllSessions(context.Background(), false)
	}()
	go func() {
		defer wg.Done()
		if err := b.registerSession(hooks.Registration{ThreadID: thread.ID, SessionID: thread.SessionID, CWD: thread.CWD}); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()
	if len(chat.created) != 1 || len(b.store.Bindings()) != 2 {
		t.Fatalf("concurrent discovery created duplicate groups: %#v", chat.created)
	}
}

func TestReadOnlyBlocksInputApprovalsAndQueueTakeover(t *testing.T) {
	b, codex, chat := discoveryFixture(t)
	if err := b.resume(context.Background(), "thread-a", false); err != nil {
		t.Fatal(err)
	}
	if err := b.store.PrependQueue("thread-a", []state.QueuedMessage{{EventID: "old-event", Text: "keep queued instruction"}}); err != nil {
		t.Fatal(err)
	}
	b.pollObserved(context.Background())
	b.flushIdleQueues(context.Background())
	b.drainQueue(context.Background(), "thread-a")
	if err := b.submit(context.Background(), "thread-a", "new instruction"); !errors.Is(err, errReadOnly) {
		t.Fatalf("submit error = %v", err)
	}
	if err := b.takeOverObserved(context.Background(), "thread-a"); !errors.Is(err, errReadOnly) {
		t.Fatalf("takeover error = %v", err)
	}
	message := feishu.Inbound{EventID: "read-only-event", ChatID: "chat-a", SenderID: "owner-a", Text: "/interrupt new instruction"}
	if err := b.onFeishuMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if err := b.onFeishuMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if len(chat.messages) != 1 || !strings.Contains(chat.messages[0], "只读同步") {
		t.Fatal("read-only notice was missing or duplicated")
	}
	approval := make(chan string, 1)
	b.pendingApprovals = map[string]pendingApproval{"request-a": {chatID: "chat-a", result: approval}}
	if err := b.onCardAction(context.Background(), feishu.CardAction{EventID: "card-a", ChatID: "chat-a", SenderID: "owner-a", Value: map[string]any{"request_id": "request-a", "decision": "accept"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.onServerRequest(context.Background(), json.RawMessage(`1`), "item/commandExecution/requestApproval", json.RawMessage(`{"threadId":"thread-a"}`)); !errors.Is(err, errReadOnly) {
		t.Fatalf("approval request error = %v", err)
	}
	if codex.resumes != 0 || len(codex.inputs) != 0 || codex.interrupts != 0 || len(approval) != 0 || len(b.store.QueuedThreads()) != 1 {
		t.Fatal("read-only mode changed a Codex session or consumed an existing queue")
	}
}
