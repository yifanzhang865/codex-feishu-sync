package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/zhangwei/codex-feishu-sync/internal/feishu"
	"github.com/zhangwei/codex-feishu-sync/internal/router"
	"github.com/zhangwei/codex-feishu-sync/internal/state"
)

type historyTransport struct {
	batches map[string]feishu.HistoryBatch
	read    []string
	err     error
}

func (h *historyTransport) EnsureRoutingChat(context.Context) (string, error) { return "mailbox", nil }
func (h *historyTransport) ListInbound(_ context.Context, chatID string, _ int64, _ bool) (feishu.HistoryBatch, error) {
	h.read = append(h.read, chatID)
	return h.batches[chatID], h.err
}

func TestTwoHostsPollOnlyTheirBoundChatsAndDeduplicateCommands(t *testing.T) {
	for _, chatID := range []string{"host-a-chat", "host-b-chat"} {
		b, codex, _ := observerFixture(t)
		codex.writer = false
		if err := b.store.Bind("thread-a", chatID); err != nil {
			t.Fatal(err)
		}
		if err := b.store.SetFeishuCursor(chatID, 1000); err != nil {
			t.Fatal(err)
		}
		if err := b.store.SetFeishuCursor("mailbox", 1000); err != nil {
			t.Fatal(err)
		}
		transport := &historyTransport{batches: map[string]feishu.HistoryBatch{
			"host-a-chat": {Cursor: 2000, Messages: []feishu.Inbound{{EventID: "a-command", ChatID: "host-a-chat", SenderID: "owner-a", Text: "run on host A"}}},
			"host-b-chat": {Cursor: 2000, Messages: []feishu.Inbound{{EventID: "b-command", ChatID: "host-b-chat", SenderID: "owner-a", Text: "run on host B"}}},
			"mailbox":     {Cursor: 2000},
		}}
		b.pollMachineMessages(context.Background(), transport, "mailbox")
		b.pollMachineMessages(context.Background(), transport, "mailbox")
		want := "run on host A"
		if chatID == "host-b-chat" {
			want = "run on host B"
		}
		if len(codex.inputs) != 1 || codex.inputs[0] != want || len(b.store.Deliveries()) != 0 || len(b.store.QueuedThreads()) != 0 {
			t.Fatalf("wrong host or repeated command: %#v", codex.inputs)
		}
		for _, id := range transport.read {
			if id != chatID && id != "mailbox" {
				t.Fatal("polled another host's conversation")
			}
		}
	}
}

func TestReadErrorKeepsCursorAndDurablePendingMessageSurvivesRestart(t *testing.T) {
	b, codex, _ := observerFixture(t)
	codex.writer = false
	if err := b.store.SetFeishuCursor("chat-a", 1000); err != nil {
		t.Fatal(err)
	}
	transport := &historyTransport{err: errors.New("network unavailable")}
	b.pollMachineMessages(context.Background(), transport, "mailbox")
	if n, _ := b.store.FeishuCursor("chat-a"); n != 1000 {
		t.Fatal("read error advanced cursor")
	}
	message := feishu.Inbound{EventID: "offline-command", ChatID: "chat-a", SenderID: "owner-a", Text: "continue after reconnect"}
	payload, _ := json.Marshal(message)
	if err := b.store.QueueDelivery(state.Delivery{ID: message.EventID, ChatID: message.ChatID, Kind: "message", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if err := b.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(filepath.Join(b.configDir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	b.store = reopened
	b.router = router.New(reopened, b.cfg.OwnerOpenID)
	b.drainDeliveries(context.Background())
	b.drainDeliveries(context.Background())
	if len(codex.inputs) != 1 || codex.inputs[0] != message.Text || len(reopened.Deliveries()) != 0 {
		t.Fatal("pending message was lost or replayed after restart")
	}
}

type failingNoticeFeishu struct {
	*observerFeishu
	fail bool
}

func (f *failingNoticeFeishu) SendText(ctx context.Context, chatID, text string) error {
	if f.fail {
		return errors.New("network unavailable")
	}
	return f.observerFeishu.SendText(ctx, chatID, text)
}

func TestInterruptedDeliveryRequiresConfirmationAndRetriesItsNotice(t *testing.T) {
	b, codex, chat := observerFixture(t)
	payload, _ := json.Marshal(feishu.Inbound{EventID: "unknown-command", ChatID: "chat-a", SenderID: "owner-a", Text: "may already have executed"})
	if err := b.store.QueueDelivery(state.Delivery{ID: "unknown-command", ChatID: "chat-a", Kind: "message", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if err := b.store.SetDeliveryStatus("unknown-command", "processing"); err != nil {
		t.Fatal(err)
	}
	client := &failingNoticeFeishu{observerFeishu: chat, fail: true}
	b.feishu = client
	b.recoverDeliveries(context.Background())
	if len(b.store.Deliveries()) != 1 || b.store.Deliveries()[0].Status != "uncertain" {
		t.Fatal("failed notice consumed uncertain instruction")
	}
	client.fail = false
	b.recoverDeliveries(context.Background())
	b.drainDeliveries(context.Background())
	if len(codex.inputs) != 0 || len(chat.messages) != 1 || len(b.store.Deliveries()) != 0 {
		t.Fatal("ambiguous instruction was replayed or notice did not retry")
	}
}

func TestPolledApprovalReachesOnlyMatchingPendingRequest(t *testing.T) {
	b, _, _ := observerFixture(t)
	b.cfg.MultiMachine = true
	b.cfg.MachineID = "host-b"
	result := make(chan string, 1)
	b.pendingApprovals = map[string]pendingApproval{"request-b": {chatID: "chat-a", result: result}}
	if err := b.store.SetFeishuCursor("mailbox", 1000); err != nil {
		t.Fatal(err)
	}
	action := feishu.CardAction{EventID: "callback-a", ChatID: "chat-a", SenderID: "owner-a", Value: map[string]any{"machine_id": "host-b", "request_id": "request-b", "decision": "accept"}}
	transport := &historyTransport{batches: map[string]feishu.HistoryBatch{"mailbox": {Cursor: 2000, Actions: []feishu.RoutedAction{{Action: action}}}}}
	b.pollMachineMessages(context.Background(), transport, "mailbox")
	b.pollMachineMessages(context.Background(), transport, "mailbox")
	if len(result) != 1 || <-result != "accept" {
		t.Fatal("approval was dropped or delivered twice")
	}
	action.EventID = "wrong-host-callback"
	action.Value["machine_id"] = "host-a"
	if err := b.onCardAction(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	if len(result) != 0 {
		t.Fatal("another host's approval affected this host")
	}
}
