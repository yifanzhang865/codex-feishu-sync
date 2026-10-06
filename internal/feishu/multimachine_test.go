package feishu

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	channel "github.com/larksuite/channel-sdk-go"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

type routingPlatform struct {
	mu          sync.Mutex
	description string
	created     int
	relays      []map[string]any
	uuids       map[string]bool
	listError   bool
	brokenPage  bool
}

func historyMessage(id, senderType, senderID, text string, at int64) map[string]any {
	content, _ := json.Marshal(map[string]string{"text": text})
	return map[string]any{"message_id": id, "create_time": strconv.FormatInt(at, 10), "msg_type": "text", "sender": map[string]any{"sender_type": senderType, "id": senderID, "id_type": "open_id"}, "body": map[string]string{"content": string(content)}}
}
func (p *routingPlatform) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		result := map[string]any{"code": 0}
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			result["tenant_access_token"] = "routing-test-token"
			result["expire"] = 7200
		case "/open-apis/bot/v3/info":
			result["bot"] = map[string]any{"open_id": "bot-id", "app_name": "test-bot", "activate_status": 2}
		case "/open-apis/im/v1/chats":
			if r.Method == http.MethodPost {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if users, ok := body["user_id_list"].([]any); ok && len(users) > 0 {
					t.Error("routing mailbox invited a human")
				}
				if r.URL.Query().Get("uuid") == "" {
					t.Error("mailbox creation was not idempotent")
				}
				p.description, _ = body["description"].(string)
				p.created++
				result["data"] = map[string]any{"chat_id": "mailbox"}
			} else {
				items := []any{}
				if p.description != "" {
					items = append(items, map[string]string{"chat_id": "mailbox", "description": p.description})
				}
				result["data"] = map[string]any{"items": items, "has_more": false}
			}
		case "/open-apis/im/v1/chats/mailbox":
			result["data"] = map[string]any{"description": p.description, "chat_mode": "group", "chat_type": "private", "chat_status": "normal", "user_count": "0", "bot_count": "1"}
		case "/open-apis/im/v1/messages":
			if r.Method == http.MethodPost {
				var body struct {
					ReceiveID string `json:"receive_id"`
					Content   string `json:"content"`
					UUID      string `json:"uuid"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.ReceiveID != "mailbox" || body.UUID == "" {
					t.Error("approval was not sent to the idempotent shared mailbox")
				}
				var content struct {
					Text string `json:"text"`
				}
				if err := json.Unmarshal([]byte(body.Content), &content); err != nil {
					t.Error(err)
				}
				if !p.uuids[body.UUID] {
					p.uuids[body.UUID] = true
					p.relays = append(p.relays, historyMessage("relay-"+strconv.Itoa(len(p.relays)), "app", "app", content.Text, 2000))
				}
				result["data"] = map[string]any{"message_id": "relay"}
				break
			}
			if p.listError {
				result["code"] = 99991672
				result["msg"] = "missing permission"
				break
			}
			if r.URL.Query().Get("sort_type") != "ByCreateTimeAsc" || r.URL.Query().Get("container_id_type") != "chat" {
				t.Error("history query lost chat/order")
			}
			chat := r.URL.Query().Get("container_id")
			items := []any{}
			more := false
			token := ""
			switch chat {
			case "mailbox":
				for _, m := range p.relays {
					items = append(items, m)
				}
			case "host-a-chat":
				if r.URL.Query().Get("page_token") == "next" {
					items = append(items, historyMessage("a-second", "user", "owner", "second A", 2000))
				} else {
					m := historyMessage("a-first", "user", "owner", "@_user_1 first A", 2000)
					m["mentions"] = []any{map[string]any{"id": "bot-id", "id_type": "open_id", "key": "@_user_1"}}
					items = append(items, historyMessage("old", "user", "owner", "old instruction", 500), m, historyMessage("foreign", "user", "other-owner", "do not run", 2000), historyMessage("bot", "app", "app", "do not loop", 2000))
					more = true
					token = "next"
					if p.brokenPage {
						token = ""
					}
				}
			case "host-b-chat":
				items = append(items, historyMessage("b-first", "user", "owner", "first B", 2000))
			default:
				t.Errorf("unexpected chat %s", chat)
			}
			result["data"] = map[string]any{"items": items, "has_more": more, "page_token": token}
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(result)
	})
}
func routingClients(t *testing.T) (*Client, *Client, *routingPlatform) {
	t.Helper()
	p := &routingPlatform{uuids: map[string]bool{}}
	server := httptest.NewServer(p.handler(t))
	t.Cleanup(server.Close)
	makeClient := func(machine string) *Client {
		sdk, err := channel.New("routing-"+t.Name(), "shared-secret", channel.WithDomain(server.URL))
		if err != nil {
			t.Fatal(err)
		}
		return &Client{sdk: sdk, appID: "shared-app", ownerID: "owner", relayKey: []byte("shared-secret"), machineID: machine, multiMachine: true}
	}
	return makeClient("host-a"), makeClient("host-b"), p
}

func TestTwoMachinesDiscoverOneMailboxAndRelayApprovalToTarget(t *testing.T) {
	a, b, platform := routingClients(t)
	ctx := context.Background()
	first, err := a.EnsureRoutingChat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.EnsureRoutingChat(ctx)
	if err != nil || first != second || platform.created != 1 {
		t.Fatalf("hosts did not share one mailbox: %s %s %v", first, second, err)
	}
	// Feishu delivered host B's card callback to host A's WS connection.
	action := CardAction{EventID: "card-event", ChatID: "host-b-chat", SenderID: "owner", Value: map[string]any{"machine_id": "host-b", "request_id": "fresh-request", "decision": "accept", "private_prompt": "must not be forwarded"}}
	if err := a.RelayCardAction(ctx, action); err != nil {
		t.Fatal(err)
	}
	if err := a.RelayCardAction(ctx, action); err != nil {
		t.Fatal(err)
	}
	wrong, err := a.ListInbound(ctx, first, 1000, true)
	if err != nil || len(wrong.Actions) != 0 {
		t.Fatal("approval reached wrong machine")
	}
	right, err := b.ListInbound(ctx, second, 1000, true)
	if err != nil || len(right.Actions) != 1 || right.Actions[0].Action.Value["decision"] != "accept" {
		t.Fatalf("target did not receive approval: %#v %v", right, err)
	}
	if len(platform.relays) != 1 || len(right.Actions[0].Action.Value) != 3 {
		t.Fatal("callback duplicate or arbitrary prompt was relayed")
	}
	packet := historyMessageText(&larkim.Message{MsgType: stringPtr("text"), Body: &larkim.MessageBody{Content: stringPtr(platform.relays[0]["body"].(map[string]string)["content"])}})
	if strings.Contains(packet, "must not be forwarded") {
		t.Fatal("relay leaked callback form data")
	}
	tampered := strings.Replace(packet, "accept", "decline", 1)
	if _, ok := b.decodeRelay(tampered); ok {
		t.Fatal("accepted a tampered approval packet")
	}
	otherKey := &Client{ownerID: b.ownerID, machineID: b.machineID, relayKey: []byte("different-secret")}
	if _, ok := otherKey.decodeRelay(packet); ok {
		t.Fatal("accepted approval signed with another secret")
	}
	action.EventID = "unauthorized"
	action.SenderID = "someone-else"
	if err := a.RelayCardAction(ctx, action); err != nil {
		t.Fatal(err)
	}
	if len(platform.relays) != 1 {
		t.Fatal("non-owner relayed an approval")
	}
}

func TestHistoryPaginationPreservesEqualTimestampsAndFiltersOtherSenders(t *testing.T) {
	a, b, _ := routingClients(t)
	first, err := a.ListInbound(context.Background(), "host-a-chat", 2000, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.ListInbound(context.Background(), "host-b-chat", 2000, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Messages) != 2 || first.Messages[0].Text != "first A" || first.Messages[1].Text != "second A" || first.Messages[0].EventID == first.Messages[1].EventID || first.Cursor != 2000 {
		t.Fatalf("pagination lost equal-time messages or sender filtering: %#v", first)
	}
	if len(second.Messages) != 1 || second.Messages[0].Text != "first B" {
		t.Fatal("second machine read another chat")
	}
}

func TestHistoryErrorsReturnNoPartialCursor(t *testing.T) {
	a, _, platform := routingClients(t)
	for _, permissionError := range []bool{true, false} {
		platform.listError = permissionError
		platform.brokenPage = !permissionError
		batch, err := a.ListInbound(context.Background(), "host-a-chat", 2000, false)
		if err == nil || batch.Cursor != 0 || len(batch.Messages) != 0 {
			t.Fatal("failed history read returned a partial advancing cursor")
		}
	}
}

func TestPolledRichTextSupportsFlatAndLocalizedHistoryBodies(t *testing.T) {
	for _, content := range []string{
		`{"title":"Task","content":[[{"tag":"text","text":"run test"}]]}`,
		`{"zh_cn":{"title":"Task","content":[[{"tag":"text","text":"run test"}]]}}`,
		`{"en_us":{"title":"Task","content_v2":[[{"tag":"text","text":"run test"}]]}}`,
	} {
		message := &larkim.Message{MsgType: stringPtr("post"), Body: &larkim.MessageBody{Content: stringPtr(content)}}
		if got := historyMessageText(message); got != "Task\nrun test" {
			t.Fatalf("rich text command was lost: %q", got)
		}
	}
}
