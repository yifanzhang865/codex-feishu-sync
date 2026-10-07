package appserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"
)

type responseBuffer struct{ bytes.Buffer }

func (*responseBuffer) Close() error { return nil }

func TestGuardedApprovalIsCheckedAtWireWrite(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		output := &responseBuffer{}
		checked := false
		client := &Client{ctx: context.Background(), stdin: output, requestFn: func(context.Context, json.RawMessage, string, json.RawMessage) (any, error) {
			return GuardedResponse(func(send func(any, error) error) error {
				checked = true
				if revoked {
					return send(nil, errors.New("lease revoked"))
				}
				return send(map[string]string{"decision": "accept"}, nil)
			}), nil
		}}
		client.handleServerRequest(rpcMessage{ID: json.RawMessage(`7`), Method: "item/fileChange/requestApproval", Params: json.RawMessage(`{}`)})
		var reply rpcMessage
		if err := json.Unmarshal(output.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		if !checked || string(reply.ID) != "7" {
			t.Fatal("guard bypassed or request id changed")
		}
		if revoked && (reply.Error == nil || len(reply.Result) != 0) {
			t.Fatal("revoked approval sent acceptance")
		}
		if !revoked && (reply.Error != nil || string(reply.Result) != `{"decision":"accept"}`) {
			t.Fatal("valid approval response corrupted")
		}
	}
}

func TestSlowNotificationsDoNotBlockHandoffCompletionTap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	blocked := make(chan struct{})
	entered := make(chan struct{}, 1)
	completion := make(chan struct{}, 1)
	client := &Client{ctx: ctx, done: make(chan struct{}), pending: make(map[string]chan rpcResult), notificationWake: make(chan struct{}, 1), notificationFn: func(string, json.RawMessage) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-blocked
	}}
	client.SetNotificationTap(func(method string, _ json.RawMessage) {
		if method == "turn/completed" {
			completion <- struct{}{}
		}
	})
	go client.readLoop(reader)
	go client.notificationLoop()
	defer close(blocked)
	go func() {
		encoder := json.NewEncoder(writer)
		_ = encoder.Encode(map[string]any{"method": "turn/started", "params": map[string]string{"threadId": "thread-a"}})
		<-entered
		for i := 0; i < 1300; i++ {
			if encoder.Encode(map[string]any{"method": "item/agentMessage/delta", "params": map[string]string{"threadId": "thread-a", "delta": "output"}}) != nil {
				return
			}
		}
		_ = encoder.Encode(map[string]any{"method": "turn/completed", "params": map[string]string{"threadId": "thread-a"}})
	}()
	select {
	case <-completion:
	case <-time.After(3 * time.Second):
		t.Fatal("slow bridge blocked completion behind its notification backlog")
	}
}

func TestListThreadsIncludesOtherProvidersAcrossPages(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	requests, stdin := io.Pipe()
	stdout, responses := io.Pipe()
	defer requests.Close()
	defer stdout.Close()
	client := &Client{
		ctx: ctx, cancel: cancel, stdin: stdin,
		pending: make(map[string]chan rpcResult), done: make(chan struct{}),
	}
	defer client.Close()
	go client.readLoop(stdout)
	go func() {
		defer responses.Close()
		decoder := json.NewDecoder(requests)
		encoder := json.NewEncoder(responses)
		for {
			var request struct {
				ID     json.RawMessage `json:"id"`
				Params struct {
					Providers json.RawMessage `json:"modelProviders"`
					Cursor    string          `json:"cursor"`
				} `json:"params"`
			}
			if decoder.Decode(&request) != nil {
				return
			}
			// Model the App Server default: missing/null providers only lists
			// the current provider; [] permits a page from another provider.
			threads := []Thread{{ID: "current-provider"}}
			cursor := ""
			if string(request.Params.Providers) == "[]" {
				if request.Params.Cursor == "other-provider-page" {
					threads = []Thread{{ID: "custom-provider"}}
				} else {
					cursor = "other-provider-page"
				}
			}
			result := map[string]any{"data": threads, "nextCursor": cursor}
			if encoder.Encode(map[string]any{"id": request.ID, "result": result}) != nil {
				return
			}
		}
	}()
	threads, err := client.ListThreads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 2 || threads[0].ID != "current-provider" || threads[1].ID != "custom-provider" {
		t.Fatalf("provider filtering lost a conversation: %#v", threads)
	}
}

func TestCallPreservesOmittedNullAndEmptyParams(t *testing.T) {
	for _, test := range []struct {
		name     string
		params   any
		present  bool
		expected string
	}{
		{name: "nil"},
		{name: "nil raw message", params: json.RawMessage(nil)},
		{name: "empty raw message", params: json.RawMessage{}},
		{name: "null", params: json.RawMessage(`null`), present: true, expected: "null"},
		{name: "empty object", params: json.RawMessage(`{}`), present: true, expected: "{}"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			requests, stdin := io.Pipe()
			stdout, responses := io.Pipe()
			defer requests.Close()
			defer stdout.Close()
			client := &Client{ctx: ctx, cancel: cancel, stdin: stdin, pending: make(map[string]chan rpcResult), done: make(chan struct{})}
			defer client.Close()
			go client.readLoop(stdout)
			wire := make(chan map[string]json.RawMessage, 1)
			go func() {
				defer responses.Close()
				var request map[string]json.RawMessage
				if json.NewDecoder(requests).Decode(&request) != nil {
					return
				}
				wire <- request
				_ = json.NewEncoder(responses).Encode(map[string]any{"id": request["id"], "result": map[string]any{"requirements": nil}})
			}()
			result, err := client.Call(ctx, "configRequirements/read", test.params)
			if err != nil {
				t.Fatal(err)
			}
			request := <-wire
			params, present := request["params"]
			if present != test.present || string(params) != test.expected {
				t.Fatalf("wire params present=%v value=%s; want present=%v value=%s", present, params, test.present, test.expected)
			}
			if string(result) != `{"requirements":null}` {
				t.Fatalf("result=%s", result)
			}
		})
	}
}
