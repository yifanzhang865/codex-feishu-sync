package router

import (
	"path/filepath"
	"testing"

	"github.com/zhangwei/codex-feishu-sync/internal/state"
)

func TestBusyMessagesQueuePerBoundThread(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Bind("thread-a", "chat-a"); err != nil {
		t.Fatal(err)
	}
	if err := store.Bind("thread-b", "chat-b"); err != nil {
		t.Fatal(err)
	}
	r := New(store, "owner")

	a, err := r.Route(Incoming{EventID: "evt-a", ChatID: "chat-a", SenderID: "owner", Text: "next"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != Queued || a.ThreadID != "thread-a" {
		t.Fatalf("route = %#v, want queued for thread-a", a)
	}

	b, err := r.Route(Incoming{EventID: "evt-b", ChatID: "chat-b", SenderID: "owner", Text: "other"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if b.ThreadID != "thread-b" {
		t.Fatalf("second route = %#v, want thread-b", b)
	}
	queued, err := store.Queued("thread-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].Text != "next" {
		t.Fatalf("queued messages = %#v", queued)
	}
}

func TestInterruptCommandBypassesQueue(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Bind("thread-a", "chat-a"); err != nil {
		t.Fatal(err)
	}
	a, err := New(store, "owner").Route(Incoming{
		EventID: "evt-a", ChatID: "chat-a", SenderID: "owner", Text: "/interrupt stop and summarize",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != Interrupt || a.Content != "stop and summarize" {
		t.Fatalf("route = %#v, want interrupt with stripped command", a)
	}
}

func TestBusyInterruptIsQueuedAheadOfOrdinaryMessagesAndDeduplicated(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Bind("thread-a", "chat-a"); err != nil {
		t.Fatal(err)
	}
	if err := store.Enqueue("thread-a", state.QueuedMessage{EventID: "event-old", Text: "ordinary"}); err != nil {
		t.Fatal(err)
	}
	r := New(store, "owner")
	message := Incoming{EventID: "event-stop", ChatID: "chat-a", SenderID: "owner", Text: "/interrupt stop now"}
	action, err := r.Route(message, true)
	if err != nil || action.Kind != Interrupt {
		t.Fatalf("Route() = %#v, %v; want interrupt", action, err)
	}
	action, err = r.Route(message, true)
	if err != nil || action.Kind != Ignored {
		t.Fatalf("duplicate Route() = %#v, %v; want ignored", action, err)
	}
	queued, err := store.Queued("thread-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 2 || queued[0].EventID != "event-stop" || !queued[0].Interrupt || queued[1].EventID != "event-old" {
		t.Fatalf("queued messages = %#v", queued)
	}
}

func TestUntrustedAndDuplicateMessagesAreIgnored(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Bind("thread-a", "chat-a"); err != nil {
		t.Fatal(err)
	}
	r := New(store, "owner")
	msg := Incoming{EventID: "evt-a", ChatID: "chat-a", SenderID: "other", Text: "ignore me"}
	a, err := r.Route(msg, false)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != Ignored {
		t.Fatalf("untrusted sender route = %#v", a)
	}

	msg.SenderID = "owner"
	a, err = r.Route(msg, false)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != Submit {
		t.Fatalf("trusted sender route = %#v", a)
	}
	a, err = r.Route(msg, false)
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != Ignored {
		t.Fatalf("duplicate route = %#v", a)
	}
}
