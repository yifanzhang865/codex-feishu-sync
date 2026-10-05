package appserver

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"
)

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
