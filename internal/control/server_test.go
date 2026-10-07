package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/zhangwei/codex-feishu-sync/internal/appserver"
)

type fakeBackend struct {
	mu           sync.Mutex
	server       *Server
	methods      []string
	turn         int
	interrupt    func(context.Context, string, string) error
	fastComplete bool
}

func (b *fakeBackend) Initialization() json.RawMessage {
	return json.RawMessage(`{"userAgent":"codex/test","platformFamily":"unix","platformOs":"linux"}`)
}
func (b *fakeBackend) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	encoded, _ := json.Marshal(params)
	thread, turn := identity(encoded)
	b.mu.Lock()
	b.methods = append(b.methods, method)
	if method == "turn/start" {
		b.turn++
		turn = fmt.Sprintf("turn-%d", b.turn)
	}
	fast, interrupt := b.fastComplete, b.interrupt
	b.mu.Unlock()
	switch method {
	case "turn/start":
		result := json.RawMessage(fmt.Sprintf(`{"turn":{"id":%q,"status":"inProgress","items":[]}}`, turn))
		b.server.Notify("turn/started", json.RawMessage(fmt.Sprintf(`{"threadId":%q,"turn":{"id":%q}}`, thread, turn)))
		if fast {
			b.server.Notify("turn/completed", json.RawMessage(fmt.Sprintf(`{"threadId":%q,"turn":{"id":%q,"status":"completed"}}`, thread, turn)))
		}
		return result, nil
	case "turn/interrupt":
		if interrupt != nil {
			if err := interrupt(ctx, thread, turn); err != nil {
				return nil, err
			}
		} else {
			b.server.Notify("turn/completed", json.RawMessage(fmt.Sprintf(`{"threadId":%q,"turn":{"id":%q,"status":"interrupted"}}`, thread, turn)))
		}
		return json.RawMessage(`{}`), nil
	case "thread/resume":
		return json.RawMessage(fmt.Sprintf(`{"thread":{"id":%q,"source":"cli","status":{"type":"idle"},"turns":[]}}`, thread)), nil
	case "test/error":
		return nil, &appserver.RPCError{Code: -32123, Message: "original error", Data: map[string]string{"kind": "original"}}
	default:
		return json.RawMessage(`{}`), nil
	}
}

func fixture(t *testing.T, hooks Hooks) (*Server, *fakeBackend) {
	t.Helper()
	backend := &fakeBackend{}
	server := New(context.Background(), backend, hooks)
	backend.server = server
	server.Manage(appserver.Thread{ID: "thread-a"})
	t.Cleanup(func() { _ = server.Close() })
	return server, backend
}

func input(thread string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"threadId":%q,"input":[{"type":"text","text":"next instruction"}]}`, thread))
}

func TestBidirectionalHandoffAndReadOnlyOperations(t *testing.T) {
	var owners []string
	server, backend := fixture(t, Hooks{Takeover: func(_ context.Context, _, owner string) error { owners = append(owners, owner); return nil }})
	for _, owner := range []string{"cli-a", Feishu, "cli-a", "cli-b"} {
		if _, err := server.Execute(context.Background(), owner, "turn/start", input("thread-a")); err != nil {
			t.Fatal(err)
		}
		if server.Owner("thread-a") != owner {
			t.Fatal("control did not transfer")
		}
		other := Feishu
		if owner == Feishu {
			other = "cli-a"
		}
		if _, err := server.Execute(context.Background(), other, "turn/interrupt", json.RawMessage(`{"threadId":"thread-a","turnId":"old"}`)); err == nil {
			t.Fatal("read-only endpoint interrupted the current sender")
		}
		if _, err := server.Execute(context.Background(), other, "thread/name/set", json.RawMessage(`{"threadId":"thread-a","name":"unauthorized"}`)); err == nil {
			t.Fatal("read-only endpoint mutated the thread")
		}
		if _, err := server.Execute(context.Background(), other, "thread/read", json.RawMessage(`{"threadId":"thread-a"}`)); err != nil {
			t.Fatal("read-only endpoint cannot read")
		}
	}
	if len(owners) != 4 || backend.turn != 4 {
		t.Fatalf("owners=%v turns=%d", owners, backend.turn)
	}
	interrupts := 0
	for _, method := range backend.methods {
		if method == "turn/interrupt" {
			interrupts++
		}
	}
	if interrupts != 3 {
		t.Fatalf("interrupts=%d", interrupts)
	}
}

func TestHandoffWaitsForCompletionAcknowledgement(t *testing.T) {
	server, backend := fixture(t, Hooks{})
	if _, err := server.Execute(context.Background(), "cli-a", "turn/start", input("thread-a")); err != nil {
		t.Fatal(err)
	}
	interrupted := make(chan struct{})
	backend.interrupt = func(context.Context, string, string) error { close(interrupted); return nil }
	done := make(chan error, 1)
	go func() {
		_, err := server.Execute(context.Background(), Feishu, "turn/start", input("thread-a"))
		done <- err
	}()
	select {
	case <-interrupted:
	case <-time.After(time.Second):
		t.Fatal("interrupt was not requested")
	}
	select {
	case err := <-done:
		t.Fatalf("new turn started before acknowledgement: %v", err)
	default:
	}
	backend.mu.Lock()
	turns := backend.turn
	backend.mu.Unlock()
	if turns != 1 {
		t.Fatal("two turns are running")
	}
	server.Notify("turn/completed", json.RawMessage(`{"threadId":"thread-a","turn":{"id":"unrelated","status":"completed"}}`))
	select {
	case err := <-done:
		t.Fatalf("unrelated completion released handoff: %v", err)
	default:
	}
	server.Notify("turn/completed", json.RawMessage(`{"threadId":"thread-a","turn":{"id":"turn-1","status":"interrupted"}}`))
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("handoff stayed blocked")
	}
}

func TestInterruptFailureDoesNotSubmitNewTurn(t *testing.T) {
	server, backend := fixture(t, Hooks{})
	_, _ = server.Execute(context.Background(), "cli-a", "turn/start", input("thread-a"))
	backend.interrupt = func(context.Context, string, string) error { return errors.New("unavailable") }
	if _, err := server.Execute(context.Background(), Feishu, "turn/start", input("thread-a")); err == nil {
		t.Fatal("interrupt failure was ignored")
	}
	if backend.turn != 1 {
		t.Fatal("new turn submitted while previous turn might be active")
	}
}

func TestSteerTakeoverStartsNewTurnAndReturnsSteerShape(t *testing.T) {
	server, backend := fixture(t, Hooks{})
	_, _ = server.Execute(context.Background(), Feishu, "turn/start", input("thread-a"))
	result, err := server.Execute(context.Background(), "cli-a", "turn/steer", json.RawMessage(`{"threadId":"thread-a","expectedTurnId":"turn-1","input":[{"type":"text","text":"take over"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(result) != `{"turnId":"turn-2"}` || backend.turn != 2 {
		t.Fatalf("result=%s turns=%d", result, backend.turn)
	}
}

func TestCompletionBeforeStartReplyDoesNotLeaveBusyMarker(t *testing.T) {
	server, backend := fixture(t, Hooks{})
	backend.fastComplete = true
	_, err := server.Execute(context.Background(), "cli-a", "turn/start", input("thread-a"))
	if err != nil {
		t.Fatal(err)
	}
	if server.ActiveTurn("thread-a") != "" {
		t.Fatal("late RPC reply restored completed turn as active")
	}
}

func TestSteerTakeoverAfterOldTurnAlreadyCompleted(t *testing.T) {
	server, backend := fixture(t, Hooks{})
	_, _ = server.Execute(context.Background(), Feishu, "turn/start", input("thread-a"))
	server.Notify("turn/completed", json.RawMessage(`{"threadId":"thread-a","turn":{"id":"turn-1","status":"completed"}}`))
	result, err := server.Execute(context.Background(), "cli-a", "turn/steer", json.RawMessage(`{"threadId":"thread-a","expectedTurnId":"turn-1","input":[{"type":"text","text":"take over"}]}`))
	if err != nil || string(result) != `{"turnId":"turn-2"}` || backend.turn != 2 {
		t.Fatalf("idle takeover did not start a new turn: %s %v", result, err)
	}
}

func TestHandoffInvalidatesFeishuApproval(t *testing.T) {
	requested := make(chan struct{})
	server, _ := fixture(t, Hooks{FeishuRequest: func(ctx context.Context, _ json.RawMessage, _ string, _ json.RawMessage) (any, error) {
		close(requested)
		<-ctx.Done()
		// Even a handler returning acceptance after cancellation is rejected.
		return map[string]string{"decision": "accept"}, nil
	}})
	_, _ = server.Execute(context.Background(), Feishu, "turn/start", input("thread-a"))
	done := make(chan error, 1)
	go func() {
		_, err := server.Request(context.Background(), json.RawMessage(`99`), "item/commandExecution/requestApproval", json.RawMessage(`{"threadId":"thread-a"}`))
		done <- err
	}()
	<-requested
	_, err := server.Execute(context.Background(), "cli-a", "turn/start", input("thread-a"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "失效") {
			t.Fatalf("stale approval accepted: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("old approval did not cancel")
	}
}

func TestApprovalRevokedBetweenHandlerReturnAndWireWrite(t *testing.T) {
	server, _ := fixture(t, Hooks{FeishuRequest: func(context.Context, json.RawMessage, string, json.RawMessage) (any, error) {
		return map[string]string{"decision": "accept"}, nil
	}})
	_, _ = server.Execute(context.Background(), Feishu, "turn/start", input("thread-a"))
	result, err := server.Request(context.Background(), json.RawMessage(`9`), "item/fileChange/requestApproval", json.RawMessage(`{"threadId":"thread-a"}`))
	if err != nil {
		t.Fatal(err)
	}
	guard, ok := result.(appserver.GuardedResponse)
	if !ok {
		t.Fatal("approval did not include wire-write guard")
	}
	_, err = server.Execute(context.Background(), "cli-a", "turn/start", input("thread-a"))
	if err != nil {
		t.Fatal(err)
	}
	var accepted bool
	var responseErr error
	_ = guard(func(_ any, err error) error { accepted = err == nil; responseErr = err; return nil })
	if accepted || responseErr == nil {
		t.Fatal("late accepted response escaped ownership guard")
	}
}

func connect(t *testing.T, server *Server) *websocket.Conn {
	t.Helper()
	conn, response, err := websocket.DefaultDialer.Dial(server.endpoint.URL, http.Header{"Authorization": {"Bearer " + server.endpoint.Token}})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func receive(t *testing.T, conn *websocket.Conn) packet {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var reply packet
	if err := conn.ReadJSON(&reply); err != nil {
		t.Fatal(err)
	}
	return reply
}

func initialize(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	if err := conn.WriteJSON(packet{ID: json.RawMessage(`1`), Method: "initialize", Params: json.RawMessage(`{"clientInfo":{"name":"codex_cli_rs"},"capabilities":{"experimentalApi":true}}`)}); err != nil {
		t.Fatal(err)
	}
	reply := receive(t, conn)
	if reply.Error != nil || !strings.Contains(string(reply.Result), "userAgent") {
		t.Fatalf("initialize: %#v", reply)
	}
}

func TestAuthenticatedWebsocketRetainsSubscriptionsAndProtocolErrors(t *testing.T) {
	server, _ := fixture(t, Hooks{})
	dir := t.TempDir()
	if err := server.Start(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "control.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("token file mode=%v", info.Mode().Perm())
	}
	conn, response, err := websocket.DefaultDialer.Dial(server.endpoint.URL, nil)
	if conn != nil {
		_ = conn.Close()
	}
	if response != nil {
		defer response.Body.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatal("unauthenticated local connection accepted")
	}
	conn = connect(t, server)
	initialize(t, conn)
	_ = conn.WriteJSON(packet{ID: json.RawMessage(`"resume"`), Method: "thread/resume", Params: json.RawMessage(`{"threadId":"thread-a"}`)})
	if reply := receive(t, conn); reply.Error != nil {
		t.Fatalf("resume: %#v", reply)
	}
	_, _ = server.Execute(context.Background(), Feishu, "turn/start", input("thread-a"))
	if notification := receive(t, conn); notification.Method != "turn/started" {
		t.Fatal("read-only CLI lost Feishu output subscription")
	}
	_ = conn.WriteJSON(packet{ID: json.RawMessage(`2`), Method: "turn/interrupt", Params: json.RawMessage(`{"threadId":"thread-a","turnId":"turn-1"}`)})
	if reply := receive(t, conn); reply.Error == nil || reply.Error.Code != -32001 {
		t.Fatal("read-only interrupt reached backend")
	}
	_ = conn.WriteJSON(packet{ID: json.RawMessage(`3`), Method: "test/error", Params: json.RawMessage(`{}`)})
	if reply := receive(t, conn); reply.Error == nil || reply.Error.Code != -32123 || reply.Error.Data == nil {
		t.Fatal("backend error code/data not preserved")
	}
	_ = conn.WriteJSON(packet{ID: json.RawMessage(`4`), Method: "thread/unsubscribe", Params: json.RawMessage(`{"threadId":"thread-a"}`)})
	if reply := receive(t, conn); string(reply.Result) != `{"status":"unsubscribed"}` {
		t.Fatal("unsubscribe not handled locally")
	}
	if !server.Managed("thread-a") || server.Owner("thread-a") != Feishu {
		t.Fatal("terminal disconnected bridge writer")
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "control.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("runtime token retained on shutdown")
	}
}

func TestCLILateApprovalAndWrongConnectionAreRejected(t *testing.T) {
	server, _ := fixture(t, Hooks{})
	if err := server.Start(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	first, second := connect(t, server), connect(t, server)
	initialize(t, first)
	initialize(t, second)
	server.mu.Lock()
	var owner string
	// Identify the connection by temporarily subscribing only this peer.
	server.mu.Unlock()
	_ = first.WriteJSON(packet{ID: json.RawMessage(`2`), Method: "thread/resume", Params: json.RawMessage(`{"threadId":"thread-a"}`)})
	_ = receive(t, first)
	server.mu.Lock()
	for id, p := range server.peers {
		if p.threads["thread-a"] {
			owner = id
		}
	}
	server.mu.Unlock()
	_, _ = server.Execute(context.Background(), owner, "turn/start", input("thread-a"))
	_ = receive(t, first)
	done := make(chan error, 1)
	go func() {
		_, err := server.Request(context.Background(), json.RawMessage(`200`), "item/fileChange/requestApproval", json.RawMessage(`{"threadId":"thread-a"}`))
		done <- err
	}()
	if request := receive(t, first); request.Method != "item/fileChange/requestApproval" {
		t.Fatal("approval not sent to owner")
	}
	_ = second.WriteJSON(packet{ID: json.RawMessage(`200`), Result: json.RawMessage(`{"decision":"accept"}`)})
	select {
	case <-done:
		t.Fatal("wrong CLI connection answered approval")
	default:
	}
	_, err := server.Execute(context.Background(), Feishu, "turn/start", input("thread-a"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("old approval accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("approval not canceled")
	}
	_ = first.WriteJSON(packet{ID: json.RawMessage(`200`), Result: json.RawMessage(`{"decision":"accept"}`)})
}
