package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	channel "github.com/larksuite/channel-sdk-go"
	"github.com/zhangwei/codex-feishu-sync/internal/appserver"
	"github.com/zhangwei/codex-feishu-sync/internal/config"
	feishutypes "github.com/zhangwei/codex-feishu-sync/internal/feishu"
	"github.com/zhangwei/codex-feishu-sync/internal/hooks"
	"github.com/zhangwei/codex-feishu-sync/internal/router"
	"github.com/zhangwei/codex-feishu-sync/internal/state"
)

type observerCodex struct {
	thread        appserver.Thread
	writer        bool
	inputs        []string
	inputThreads  []string
	resumeThreads []string
	resumes       int
	interrupts    int
}

func (c *observerCodex) ListThreads(context.Context) ([]appserver.Thread, error) {
	return []appserver.Thread{c.thread}, nil
}
func (c *observerCodex) ReadThread(context.Context, string) (appserver.Thread, error) {
	return c.thread, nil
}
func (c *observerCodex) ResumeThread(_ context.Context, threadID string) (appserver.Thread, error) {
	c.resumes++
	c.resumeThreads = append(c.resumeThreads, threadID)
	if c.writer {
		return appserver.Thread{}, errors.New("thread thread-a already has an active writer")
	}
	thread := c.thread
	thread.Status = json.RawMessage(`{"type":"idle"}`)
	return thread, nil
}
func (c *observerCodex) StartTurn(_ context.Context, threadID, text string) (string, error) {
	c.inputs = append(c.inputs, text)
	c.inputThreads = append(c.inputThreads, threadID)
	return "new-turn", nil
}
func (c *observerCodex) InterruptTurn(context.Context, string, string) error {
	c.interrupts++
	return nil
}
func (c *observerCodex) Close() error { return nil }

type observerFeishu struct {
	messages       []string
	chatIDs        []string
	created        []string
	onCreate       func(string)
	deleted        []string
	deleteError    error
	ownershipError error
	protectChat    bool
	humanActivity  time.Time
	onHistory      func()
}

func (f *observerFeishu) SendText(_ context.Context, chatID, text string) error {
	f.messages = append(f.messages, text)
	f.chatIDs = append(f.chatIDs, chatID)
	return nil
}
func (f *observerFeishu) SendCard(context.Context, string, string) error { return nil }
func (f *observerFeishu) Start(context.Context) error                    { return nil }
func (f *observerFeishu) Stop(context.Context) error                     { return nil }
func (f *observerFeishu) StartMarkdownStream(context.Context, string, string, string) (channel.StreamController, error) {
	return nil, errors.New("unexpected stream")
}
func (f *observerFeishu) CreateThreadChat(_ context.Context, threadID, _, _ string) (string, error) {
	f.created = append(f.created, threadID)
	if f.onCreate != nil {
		f.onCreate(threadID)
	}
	if threadID == "thread-a" {
		return "chat-a", nil
	}
	return "chat-" + threadID, nil
}

func (f *observerFeishu) ThreadChatCanBeDeleted(context.Context, string, string) (bool, error) {
	return !f.protectChat, f.ownershipError
}
func (f *observerFeishu) LastHumanMessage(context.Context, string, time.Time) (time.Time, error) {
	if f.onHistory != nil {
		f.onHistory()
	}
	return f.humanActivity, nil
}
func (f *observerFeishu) DeleteThreadChat(_ context.Context, chatID string) error {
	if f.deleteError != nil {
		return f.deleteError
	}
	f.deleted = append(f.deleted, chatID)
	return nil
}

func observerFixture(t *testing.T) (*Bridge, *observerCodex, *observerFeishu) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Bind("thread-a", "chat-a"); err != nil {
		t.Fatal(err)
	}
	codex := &observerCodex{writer: true, thread: appserver.Thread{
		ID: "thread-a", SessionID: "session-a", CWD: "/work",
		Source: json.RawMessage(`"cli"`),
		Status: json.RawMessage(`{"type":"notLoaded"}`),
	}}
	feishu := &observerFeishu{}
	bridge := &Bridge{
		ctx: context.Background(), configDir: dir, store: store, codex: codex, feishu: feishu,
		cfg:         config.Config{SyncLevel: config.ConversationStatus, OwnerOpenID: "owner-a"},
		threadLocks: make(map[string]*sync.Mutex), threads: make(map[string]appserver.Thread),
		busy: make(map[string]bool), turnIDs: make(map[string]string),
		observed: make(map[string]bool), lastTakeover: make(map[string]time.Time),
		echoSuppress: make(map[string][]string),
	}
	bridge.router = router.New(store, bridge.cfg.OwnerOpenID)
	return bridge, codex, feishu
}

func TestObserverForwardsCompletedRepliesWithoutRepeatingOrLeakingReasoning(t *testing.T) {
	b, codex, feishu := observerFixture(t)
	codex.thread.Turns = []json.RawMessage{
		json.RawMessage(`{"status":"completed","items":[{"id":"old","type":"agentMessage","text":"old answer"}]}`),
		json.RawMessage(`{"id":"turn-1","status":"inProgress","items":[{"id":"request","type":"userMessage","content":[{"type":"text","text":"new request"}]}]}`),
	}
	if err := b.resume(context.Background(), "thread-a", true); err != nil {
		t.Fatal(err)
	}
	if !b.isObserved("thread-a") || len(feishu.messages) != 0 {
		t.Fatal("observer baseline was not established")
	}
	codex.thread.Turns[1] = json.RawMessage(`{"id":"turn-1","status":"completed","items":[{"id":"request","type":"userMessage","content":[{"type":"text","text":"new request"}]},{"id":"private","type":"reasoning","text":"private thought"},{"id":"reply","type":"agentMessage","text":"new answer"}]}`)
	b.pollObserved(context.Background())
	if len(feishu.messages) != 2 || !strings.Contains(feishu.messages[1], "new answer") {
		t.Fatalf("messages = %#v", feishu.messages)
	}
	for _, message := range feishu.messages {
		if strings.Contains(message, "private thought") || strings.Contains(message, "old answer") {
			t.Fatalf("unexpected history or reasoning: %q", message)
		}
	}
	b.pollObserved(context.Background())
	if len(feishu.messages) != 2 {
		t.Fatal("unchanged history was forwarded twice")
	}
}

func TestPendingRegistrationCatchesLatestReplyBeforeRecoveryBaseline(t *testing.T) {
	b, codex, feishu := observerFixture(t)
	codex.thread.Turns = []json.RawMessage{
		json.RawMessage(`{"status":"completed","items":[{"id":"old","type":"agentMessage","text":"old answer"}]}`),
		json.RawMessage(`{"status":"completed","items":[{"id":"latest","type":"agentMessage","text":"latest answer"}]}`),
	}
	input := []byte(`{"hook_event_name":"SessionStart","session_id":"session-a","cwd":"/work"}`)
	if err := hooks.Record(b.configDir, input); err != nil {
		t.Fatal(err)
	}
	if err := b.recoverThreads(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(feishu.messages) != 1 || !strings.Contains(feishu.messages[0], "latest answer") {
		t.Fatalf("messages = %#v", feishu.messages)
	}
	if !b.store.HistoryInitialized("thread-a") {
		t.Fatal("history was not initialized")
	}
}

func TestObserverKeepsQueuedInstructionUntilCLIReleasesWriter(t *testing.T) {
	b, codex, _ := observerFixture(t)
	if err := b.resume(context.Background(), "thread-a", true); err != nil {
		t.Fatal(err)
	}
	if err := b.store.PrependQueue("thread-a", []state.QueuedMessage{{EventID: "event-a", Text: "next instruction"}}); err != nil {
		t.Fatal(err)
	}
	b.pollObserved(context.Background())
	b.flushIdleQueues(context.Background())
	if len(codex.inputs) != 0 || len(b.store.QueuedThreads()) != 1 {
		t.Fatal("instruction was submitted or lost while CLI owned the writer")
	}
	codex.writer = false
	b.lastTakeover["thread-a"] = time.Time{}
	b.pollObserved(context.Background())
	b.flushIdleQueues(context.Background())
	if b.isObserved("thread-a") || len(codex.inputs) != 1 || codex.inputs[0] != "next instruction" || len(b.store.QueuedThreads()) != 0 {
		t.Fatalf("takeover inputs = %#v", codex.inputs)
	}
}

func TestObserverInterruptQueuesInstructionWhileCLIControlsIdleOrBusyThread(t *testing.T) {
	for _, busy := range []bool{false, true} {
		name := "idle"
		if busy {
			name = "busy"
		}
		t.Run(name, func(t *testing.T) {
			b, codex, feishu := observerFixture(t)
			if busy {
				codex.thread.Turns = []json.RawMessage{json.RawMessage(`{"id":"turn-1","status":"inProgress","items":[]}`)}
			}
			if err := b.resume(context.Background(), "thread-a", true); err != nil {
				t.Fatal(err)
			}
			message := feishutypes.Inbound{EventID: "event-a", ChatID: "chat-a", SenderID: "owner-a", Text: "/interrupt next instruction"}
			if err := b.onFeishuMessage(context.Background(), message); err != nil {
				t.Fatal(err)
			}
			if len(b.store.QueuedThreads()) != 1 || len(codex.inputs) != 0 || len(feishu.messages) != 1 {
				t.Fatal("interrupt instruction was not retained while CLI held the writer")
			}
			codex.writer = false
			codex.thread.Turns = nil
			b.pollObserved(context.Background())
			b.flushIdleQueues(context.Background())
			if len(codex.inputs) != 1 || codex.inputs[0] != "next instruction" || len(b.store.QueuedThreads()) != 0 {
				t.Fatalf("takeover inputs = %#v", codex.inputs)
			}
		})
	}
}
