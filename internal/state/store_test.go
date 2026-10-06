package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBindingsAndDedupSurviveRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Bind("thread-a", "chat-a"); err != nil {
		t.Fatal(err)
	}
	first, err := store.MarkEvent("evt-a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.MarkEvent("evt-a")
	if err != nil {
		t.Fatal(err)
	}
	if !first || second {
		t.Fatal("MarkEvent must accept the first occurrence only")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	threadID, ok := store.ThreadForChat("chat-a")
	if !ok || threadID != "thread-a" {
		t.Fatalf("ThreadForChat() = %q, %v", threadID, ok)
	}
	duplicate, err := store.MarkEvent("evt-a")
	if err != nil {
		t.Fatal(err)
	}
	if duplicate {
		t.Fatal("duplicate event was accepted after reopening state")
	}
}

func TestHistoryInitializationSurvivesRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if store.HistoryInitialized("thread-a") {
		t.Fatal("new thread history must not be initialized")
	}
	if err := store.MarkHistoryInitialized("thread-a"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if !store.HistoryInitialized("thread-a") {
		t.Fatal("thread history initialization was not restored")
	}
}

func TestHasEventTracksPersistedDedupState(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if store.HasEvent("event-a") {
		t.Fatal("unseen event was reported as handled")
	}
	if _, err := store.MarkEvent("event-a"); err != nil {
		t.Fatal(err)
	}
	if !store.HasEvent("event-a") {
		t.Fatal("marked event was not reported as handled")
	}
}

func TestInitializeHistoryPersistsBaselineAndDedupEvents(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InitializeHistory("thread-a", []string{"event-a", "event-b", "event-a"}); err != nil {
		t.Fatal(err)
	}
	if !store.HistoryInitialized("thread-a") || !store.HasEvent("event-a") || !store.HasEvent("event-b") {
		t.Fatal("history baseline and dedup events were not persisted together")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if !store.HistoryInitialized("thread-a") || !store.HasEvent("event-a") || !store.HasEvent("event-b") {
		t.Fatal("history baseline was not restored")
	}
}

func TestBindingCannotPointOneChatAtTwoThreads(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Bind("thread-a", "chat-a"); err != nil {
		t.Fatal(err)
	}
	if err := store.Bind("thread-b", "chat-a"); err == nil {
		t.Fatal("Bind() allowed a chat to route to two threads")
	}
}

func TestBindRollsBackMemoryWhenPersistenceFails(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	store.dir = filepath.Join(blocker, "state")
	if err := store.Bind("thread-a", "chat-a"); err == nil {
		t.Fatal("Bind() succeeded without a writable state directory")
	}
	if _, ok := store.ChatForThread("thread-a"); ok {
		t.Fatal("failed Bind() left the thread mapping in memory")
	}
	if _, ok := store.ThreadForChat("chat-a"); ok {
		t.Fatal("failed Bind() left the chat mapping in memory")
	}
}

func TestEnqueueRollsBackMemoryWhenPersistenceFails(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	store.dir = filepath.Join(blocker, "state")
	if err := store.Enqueue("thread-a", QueuedMessage{EventID: "event-a", Text: "next", AddedAt: time.Now()}); err == nil {
		t.Fatal("Enqueue() succeeded without a writable state directory")
	}
	queued, err := store.Queued("thread-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 0 {
		t.Fatalf("failed Enqueue() left messages in memory: %#v", queued)
	}
}

func TestEnqueueOnceAtomicallyQueuesAndDeduplicates(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	message := QueuedMessage{EventID: "event-a", Text: "next"}
	accepted, err := store.EnqueueOnce("thread-a", message, false)
	if err != nil || !accepted {
		t.Fatalf("first EnqueueOnce() = %v, %v; want accepted", accepted, err)
	}
	accepted, err = store.EnqueueOnce("thread-a", message, false)
	if err != nil || accepted {
		t.Fatalf("duplicate EnqueueOnce() = %v, %v; want ignored", accepted, err)
	}
	queued, err := store.Queued("thread-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 1 || queued[0].Text != "next" {
		t.Fatalf("queued messages = %#v", queued)
	}
}

func TestEnqueueOnceRollsBackEventAndQueueWhenPersistenceFails(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	store.dir = filepath.Join(blocker, "state")
	accepted, err := store.EnqueueOnce("thread-a", QueuedMessage{EventID: "event-a", Text: "next"}, false)
	if err == nil || accepted {
		t.Fatalf("EnqueueOnce() = %v, %v; want persistence error", accepted, err)
	}
	if store.data.Events["event-a"] {
		t.Fatal("failed EnqueueOnce() left the event marked as handled")
	}
	if len(store.data.Queues["thread-a"]) != 0 {
		t.Fatal("failed EnqueueOnce() left a message in the queue")
	}
}

func TestQueuedThreadsReturnsOnlyThreadsWithPendingMessages(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Enqueue("thread-b", QueuedMessage{EventID: "event-b", Text: "next"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Enqueue("thread-a", QueuedMessage{EventID: "event-a", Text: "next"}); err != nil {
		t.Fatal(err)
	}
	threads := store.QueuedThreads()
	if len(threads) != 2 || threads[0] != "thread-a" || threads[1] != "thread-b" {
		t.Fatalf("QueuedThreads() = %#v, want sorted queued thread IDs", threads)
	}
	if _, err := store.Dequeue("thread-a"); err != nil {
		t.Fatal(err)
	}
	threads = store.QueuedThreads()
	if len(threads) != 1 || threads[0] != "thread-b" {
		t.Fatalf("QueuedThreads() after dequeue = %#v", threads)
	}
}

func TestBindingCountReadsStateWithoutRewritingIt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Bind("thread-a", "chat-a"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	count, err := BindingCount(dir)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("BindingCount() = %d, want 1", count)
	}
	if string(before) != string(after) {
		t.Fatal("BindingCount() rewrote the state file")
	}
}

func TestPollingInboxPersistsOrderCursorAndDedupAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"second-alphabetically", "first-alphabetically"} {
		if err := s.QueueDelivery(Delivery{ID: id, ChatID: "chat", Kind: "message", Payload: []byte(`{"Text":"instruction"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetFeishuCursor("chat", 2000); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFeishuCursor("chat", 1000); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDeliveryStatus("second-alphabetically", "processing"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	items := s.Deliveries()
	if len(items) != 2 || items[0].ID != "second-alphabetically" || items[0].Status != "processing" || items[1].Sequence <= items[0].Sequence {
		t.Fatalf("lost delivery order/state: %#v", items)
	}
	if n, ok := s.FeishuCursor("chat"); !ok || n != 2000 {
		t.Fatal("cursor was lost or moved backwards")
	}
	if err := s.FinishDelivery(items[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.QueueDelivery(items[0]); err != nil {
		t.Fatal(err)
	}
	if len(s.Deliveries()) != 1 || !s.HasEvent(items[0].ID) {
		t.Fatal("finished event re-entered the inbox")
	}
}

func TestPollingPersistenceFailureDoesNotAdvanceCursorOrConsumeDelivery(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.QueueDelivery(Delivery{ID: "message", ChatID: "chat", Kind: "message", Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFeishuCursor("chat", 1000); err != nil {
		t.Fatal(err)
	}
	original := s.dir
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("block"), 0600); err != nil {
		t.Fatal(err)
	}
	s.dir = blocker
	if err := s.SetFeishuCursor("chat", 2000); err == nil {
		t.Fatal("failed cursor write was accepted")
	}
	if err := s.FinishDelivery("message"); err == nil {
		t.Fatal("failed completion write was accepted")
	}
	if n, _ := s.FeishuCursor("chat"); n != 1000 || len(s.Deliveries()) != 1 || s.HasEvent("message") {
		t.Fatal("failed transaction changed in-memory state")
	}
	s.dir = original
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
