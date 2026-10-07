package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/zhangwei/codex-feishu-sync/internal/control"
	"github.com/zhangwei/codex-feishu-sync/internal/feishu"
	"github.com/zhangwei/codex-feishu-sync/internal/state"
)

type handoffBackend struct {
	codex    *discoveryCodex
	server   *control.Server
	sequence int
}

func (c *handoffBackend) Initialization() json.RawMessage { return json.RawMessage(`{}`) }
func (c *handoffBackend) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	data, _ := json.Marshal(params)
	var p struct {
		Thread string `json:"threadId"`
		Turn   string `json:"turnId"`
		Input  []struct {
			Text string `json:"text"`
		} `json:"input"`
	}
	_ = json.Unmarshal(data, &p)
	switch method {
	case "thread/resume":
		thread, err := c.codex.ResumeThread(ctx, p.Thread)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"thread": thread})
	case "turn/start":
		c.sequence++
		_, err := c.codex.StartTurn(ctx, p.Thread, p.Input[0].Text)
		id := fmt.Sprintf("handoff-%d", c.sequence)
		result := json.RawMessage(fmt.Sprintf(`{"turn":{"id":%q,"status":"inProgress"}}`, id))
		c.server.Notify("turn/started", json.RawMessage(fmt.Sprintf(`{"threadId":%q,"turn":{"id":%q}}`, p.Thread, id)))
		return result, err
	case "turn/interrupt":
		_ = c.codex.InterruptTurn(ctx, p.Thread, p.Turn)
		c.server.Notify("turn/completed", json.RawMessage(fmt.Sprintf(`{"threadId":%q,"turn":{"id":%q,"status":"interrupted"}}`, p.Thread, p.Turn)))
		return json.RawMessage(`{}`), nil
	default:
		return json.RawMessage(`{}`), nil
	}
}

func handoffFixture(t *testing.T) (*Bridge, *discoveryCodex, *observerFeishu) {
	t.Helper()
	b, codex, chat := controlFixture(t)
	backend := &handoffBackend{codex: codex}
	b.localControl = control.New(b.ctx, backend, control.Hooks{Call: b.onLocalCLICall, Takeover: b.onControlTakeover, FeishuRequest: b.onFeishuServerRequest})
	backend.server = b.localControl
	t.Cleanup(func() { _ = b.localControl.Close() })
	b.localControl.Manage(codex.thread)
	b.setControlled("thread-a", true)
	return b, codex, chat
}

func TestManagedCLIAndFeishuHandoffWithoutExitingCLI(t *testing.T) {
	b, codex, chat := handoffFixture(t)
	params := json.RawMessage(`{"threadId":"thread-a","input":[{"type":"text","text":"from CLI"}]}`)
	if _, err := b.onLocalCLICall(context.Background(), "cli-a", "turn/start", params); err != nil {
		t.Fatal(err)
	}
	b.busy["thread-a"] = true
	b.turnIDs["thread-a"] = "handoff-1"
	message := feishu.Inbound{EventID: "handoff-message", ChatID: "chat-a", SenderID: "owner-a", Text: "from Feishu"}
	if err := b.onFeishuMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if codex.interrupts != 1 || len(codex.inputs) != 2 || codex.inputs[1] != message.Text || b.localControl.Owner("thread-a") != control.Feishu {
		t.Fatalf("inputs=%v interrupts=%d", codex.inputs, codex.interrupts)
	}
	if err := b.onFeishuMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if len(codex.inputs) != 2 {
		t.Fatal("duplicate event reacquired control")
	}
	if len(chat.messages) < 2 || !strings.Contains(strings.Join(chat.messages, "\n"), "控制权已交给 飞书") {
		t.Fatal("ownership was not announced")
	}
}

func TestCLIHandoffDiscardsOldQueueAndKeepsNewTurnBusy(t *testing.T) {
	b, codex, _ := handoffFixture(t)
	message := feishu.Inbound{EventID: "first-feishu", ChatID: "chat-a", SenderID: "owner-a", Text: "first task"}
	if err := b.onFeishuMessage(context.Background(), message); err != nil {
		t.Fatal(err)
	}
	if err := b.store.PrependQueue("thread-a", []state.QueuedMessage{{EventID: "old-queue", Text: "must not execute"}}); err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(`{"threadId":"thread-a","input":[{"type":"text","text":"new CLI task"}]}`)
	if _, err := b.onLocalCLICall(context.Background(), "cli-a", "turn/start", params); err != nil {
		t.Fatal(err)
	}
	if len(b.store.QueuedThreads()) != 0 || codex.interrupts != 1 {
		t.Fatal("old Feishu queue survived CLI takeover")
	}
	// Completion delivery may lag behind the RPC that starts the new turn.
	b.finishTurn(context.Background(), "thread-a", json.RawMessage(`{"threadId":"thread-a","turn":{"id":"handoff-1","status":"interrupted","items":[]}}`))
	if !b.busy["thread-a"] || b.turnIDs["thread-a"] != "handoff-2" || len(codex.inputs) != 2 {
		t.Fatal("late old completion cleared new turn or replayed old queue")
	}
	b.localControl.Notify("turn/completed", json.RawMessage(`{"threadId":"thread-a","turn":{"id":"handoff-2","status":"completed"}}`))
	b.finishTurn(context.Background(), "thread-a", json.RawMessage(`{"threadId":"thread-a","turn":{"id":"handoff-2","status":"completed","items":[]}}`))
	if b.busy["thread-a"] || len(codex.inputs) != 2 {
		t.Fatal("queue regained control after CLI task completed")
	}
}

func TestCanceledQuestionDoesNotConsumeNewFeishuInstruction(t *testing.T) {
	b, codex, _ := handoffFixture(t)
	_, err := b.onLocalCLICall(context.Background(), "cli-a", "turn/start", json.RawMessage(`{"threadId":"thread-a","input":[{"type":"text","text":"CLI task"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b.pendingQuestions = map[string]*pendingQuestion{"thread-a": {chatID: "chat-a", result: make(chan string, 1), ctx: ctx}}
	b.pendingApprovals = map[string]pendingApproval{"stale": {chatID: "chat-a", result: make(chan string, 1), ctx: ctx}}
	b.pendingByChat = map[string]string{"chat-a": "stale"}
	if err := b.onFeishuMessage(context.Background(), feishu.Inbound{EventID: "new-after-revocation", ChatID: "chat-a", SenderID: "owner-a", Text: "new task"}); err != nil {
		t.Fatal(err)
	}
	if len(codex.inputs) != 2 || codex.inputs[1] != "new task" {
		t.Fatal("canceled question/approval swallowed a new instruction")
	}
}

func TestOpeningNativeCLIOnlySubscribesAndKeepsCurrentSender(t *testing.T) {
	b, codex, _ := handoffFixture(t)
	codex.writer = false
	if err := b.onFeishuMessage(context.Background(), feishu.Inbound{EventID: "owned-feishu", ChatID: "chat-a", SenderID: "owner-a", Text: "current Feishu task"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.onLocalCLICall(context.Background(), "cli-viewer", "thread/resume", json.RawMessage(`{"threadId":"thread-a"}`)); err != nil {
		t.Fatal(err)
	}
	if b.localControl.Owner("thread-a") != control.Feishu || !b.isControlled("thread-a") || b.isObserved("thread-a") {
		t.Fatal("opening CLI took control or returned session to polling")
	}
	if err := b.onFeishuMessage(context.Background(), feishu.Inbound{EventID: "queued-after-open", ChatID: "chat-a", SenderID: "owner-a", Text: "same sender followup"}); err != nil {
		t.Fatal(err)
	}
	if len(codex.inputs) != 1 || len(b.store.QueuedThreads()) != 1 {
		t.Fatal("a persisted idle snapshot bypassed the current sender's active turn")
	}
}
