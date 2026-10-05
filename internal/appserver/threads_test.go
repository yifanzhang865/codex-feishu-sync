package appserver

import (
	"encoding/json"
	"testing"

	"github.com/zhangwei/codex-feishu-sync/internal/hooks"
)

func TestMatchRegistrationPrefersExactThreadID(t *testing.T) {
	threads := []Thread{
		{ID: "thread-a", SessionID: "session-a", CWD: "/work/a"},
		{ID: "thread-b", SessionID: "session-a", CWD: "/work/a/sub"},
	}
	thread, ok := MatchRegistration(threads, hooks.Registration{SessionID: "session-a", CWD: "/work/a", ThreadID: "thread-b"})
	if !ok || thread.ID != "thread-b" {
		t.Fatalf("match = %#v, %v", thread, ok)
	}
}

func TestParseThreadListResponseReadsDataAndCursor(t *testing.T) {
	input := json.RawMessage(`{"data":[{"id":"thread-a","sessionId":"session-a","cwd":"/work/a"}],"nextCursor":"page-2","backwardsCursor":"older"}`)
	threads, cursor, err := parseThreadListResponse(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 || threads[0].ID != "thread-a" || cursor != "page-2" {
		t.Fatalf("threads=%#v cursor=%q", threads, cursor)
	}
}

func TestMatchRegistrationUsesUniqueSessionAndCWD(t *testing.T) {
	threads := []Thread{
		{ID: "thread-a", SessionID: "session-a", CWD: "/work/a"},
		{ID: "thread-b", SessionID: "session-b", CWD: "/work/b"},
	}
	thread, ok := MatchRegistration(threads, hooks.Registration{SessionID: "session-b", CWD: "/work/b"})
	if !ok || thread.ID != "thread-b" {
		t.Fatalf("match = %#v, %v", thread, ok)
	}
}

func TestMatchRegistrationRejectsAmbiguousSession(t *testing.T) {
	threads := []Thread{
		{ID: "thread-a", SessionID: "session-a", CWD: "/work/a"},
		{ID: "thread-b", SessionID: "session-a", CWD: "/work/a"},
	}
	if _, ok := MatchRegistration(threads, hooks.Registration{SessionID: "session-a", CWD: "/work/a"}); ok {
		t.Fatal("ambiguous session matched a thread")
	}
}

func TestReadOnlyThreadBusyStateUsesLatestPersistedTurn(t *testing.T) {
	thread := Thread{Status: json.RawMessage(`{"type":"notLoaded"}`), Turns: []json.RawMessage{
		json.RawMessage(`{"id":"turn-a","status":"inProgress"}`),
	}}
	if !thread.IsBusy() {
		t.Fatal("persisted active turn was treated as idle")
	}
	thread.Turns = append(thread.Turns, json.RawMessage(`{"id":"turn-b","status":"completed"}`))
	if thread.IsBusy() {
		t.Fatal("an older active marker overrode the latest completed turn")
	}
}

func TestResumedIdleRuntimeOverridesAbandonedTurn(t *testing.T) {
	thread := Thread{Status: json.RawMessage(`{"type":"idle"}`), Turns: []json.RawMessage{
		json.RawMessage(`{"id":"abandoned-turn","status":"inProgress"}`),
	}}
	if thread.IsBusy() {
		t.Fatal("idle resumed runtime stayed busy because of an abandoned CLI turn")
	}
	thread.Status = json.RawMessage(`{"type":"active"}`)
	if !thread.IsBusy() {
		t.Fatal("live active runtime was ignored")
	}
}
