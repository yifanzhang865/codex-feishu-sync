package bridge

import "testing"

func TestPlanEventIDIsStableAndScopedToTurn(t *testing.T) {
	update := planNotification{
		TurnID:      "turn-a",
		Explanation: "implementation plan",
		Plan:        []planStep{{Step: "write tests", Status: "inProgress"}},
	}
	first := planEventID("thread-a", update)
	if second := planEventID("thread-a", update); first != second {
		t.Fatalf("same plan update produced event IDs %q and %q", first, second)
	}
	update.TurnID = "turn-b"
	if nextTurn := planEventID("thread-a", update); first == nextTurn {
		t.Fatal("same plan text in another turn reused the event ID")
	}
}
