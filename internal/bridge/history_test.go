package bridge

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/zhangwei/codex-feishu-sync/internal/appserver"
	"github.com/zhangwei/codex-feishu-sync/internal/config"
	"github.com/zhangwei/codex-feishu-sync/internal/events"
	"github.com/zhangwei/codex-feishu-sync/internal/state"
)

func TestResumeBaselinesOldTurnsAndLeavesActiveTurnForCompletion(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	thread := appserver.Thread{
		ID: "thread-a",
		Turns: []json.RawMessage{
			json.RawMessage(`{"status":"completed","items":[{"id":"old-item","type":"userMessage","content":[{"type":"text","text":"old request"}]}]}`),
			json.RawMessage(`{"status":"inProgress","items":[{"id":"active-item","type":"userMessage","content":[{"type":"text","text":"current request"}]}]}`),
		},
	}
	bridge := &Bridge{store: store}
	bridge.syncHistoryOnResume(context.Background(), thread, false)
	if !store.HistoryInitialized(thread.ID) {
		t.Fatal("history was not marked initialized")
	}
	if accepted, err := markCodexItem(store, thread.ID, map[string]any{"id": "old-item"}); err != nil || accepted {
		t.Fatalf("old item state = %v, %v; want baselined", accepted, err)
	}
	if accepted, err := markCodexItem(store, thread.ID, map[string]any{"id": "active-item"}); err != nil || !accepted {
		t.Fatalf("active item state = %v, %v; want pending for turn completion", accepted, err)
	}
}

func TestItemEventForwardsConversationAndPlanButNeverReasoning(t *testing.T) {
	user, ok := itemEvent(map[string]any{"type": "userMessage", "content": []any{map[string]any{"type": "text", "text": "request"}}}, config.ConversationStatus)
	if !ok || user.Kind != events.UserMessage || user.Text != "request" {
		t.Fatalf("user item event = %#v, %v", user, ok)
	}
	plan, ok := itemEvent(map[string]any{"type": "plan", "text": "step"}, config.ConversationStatus)
	if !ok || plan.Kind != events.PlanUpdate {
		t.Fatalf("plan item event = %#v, %v", plan, ok)
	}
	if _, ok := itemEvent(map[string]any{"type": "reasoning", "summary": []any{"private"}}, config.AllVisible); ok {
		t.Fatal("reasoning item must never be forwarded")
	}
}

func TestTurnTerminalDetection(t *testing.T) {
	for _, status := range []string{"completed", "interrupted", "failed"} {
		if !turnIsTerminal(json.RawMessage(`"` + status + `"`)) {
			t.Errorf("turnIsTerminal(%q) = false", status)
		}
	}
	if turnIsTerminal(json.RawMessage(`"inProgress"`)) {
		t.Fatal("active turn was treated as terminal")
	}
}

func TestSessionHookInitialHistoryCatchesUpLatestCompletedTurn(t *testing.T) {
	thread := appserver.Thread{
		ID: "thread-a",
		Turns: []json.RawMessage{
			json.RawMessage(`{"status":"completed","items":[{"id":"old-item","type":"agentMessage","text":"old answer"}]}`),
			json.RawMessage(`{"status":"completed","items":[{"id":"latest-item","type":"agentMessage","text":"first answer"}]}`),
		},
	}
	plan := initialHistoryDisposition(thread, true)
	if len(plan.baseline) != 2 || plan.baseline[1] != "codex-item:thread-a:old-item" {
		t.Fatalf("baseline items = %#v", plan.baseline)
	}
	if len(plan.replay) != 1 || len(plan.replay[0].Items) != 1 || plan.replay[0].Items[0]["id"] != "latest-item" {
		t.Fatalf("replay turns = %#v", plan.replay)
	}
}

func TestSessionHookDoesNotReplayPreviousTurnWhenLatestTurnIsActive(t *testing.T) {
	thread := appserver.Thread{
		ID: "thread-a",
		Turns: []json.RawMessage{
			json.RawMessage(`{"status":"completed","items":[{"id":"old-item","type":"agentMessage","text":"old answer"}]}`),
			json.RawMessage(`{"status":"inProgress","items":[{"id":"active-item","type":"userMessage","content":[{"type":"text","text":"current request"}]}]}`),
		},
	}
	plan := initialHistoryDisposition(thread, true)
	if len(plan.baseline) != 2 || plan.baseline[1] != "codex-item:thread-a:old-item" || len(plan.replay) != 0 {
		t.Fatalf("active latest turn disposition = %#v; want older turns baselined and no replay", plan)
	}
}

func TestResumeInitialHistoryBaselinesEveryCompletedTurn(t *testing.T) {
	thread := appserver.Thread{
		ID: "thread-a",
		Turns: []json.RawMessage{
			json.RawMessage(`{"status":"completed","items":[{"id":"first-item","type":"agentMessage","text":"first"}]}`),
			json.RawMessage(`{"status":"completed","items":[{"id":"latest-item","type":"agentMessage","text":"latest"}]}`),
		},
	}
	plan := initialHistoryDisposition(thread, false)
	if len(plan.baseline) != 4 || len(plan.replay) != 0 {
		t.Fatalf("ordinary resume disposition = %#v; want every completed turn baselined", plan)
	}
}
