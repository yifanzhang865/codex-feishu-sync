package state

import (
	"testing"
	"time"
)

func TestManagedLifecycleSurvivesRestartWithoutLosingHistory(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindManaged("thread", "chat"); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-80 * time.Hour).UTC().Truncate(time.Second)
	if err := store.RecordActivity("thread", old); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordActivity("thread", old.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.InitializeHistory("thread", []string{"reply"}); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginChatDeletion("thread", "chat"); err != nil {
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
	managed := store.ManagedChats()["thread"]
	if !managed.LastActivity.Equal(old) || !managed.DeleteStarted {
		t.Fatal("restart lost lifecycle state")
	}
	if err := store.MarkChatDeleted("thread", "chat"); err != nil {
		t.Fatal(err)
	}
	if err := store.UnbindDeletedChat("thread", "chat"); err != nil {
		t.Fatal(err)
	}
	if !store.HistoryInitialized("thread") || !store.HasEvent("reply") || len(store.Bindings()) != 0 {
		t.Fatal("group cleanup deleted conversation history")
	}
}
