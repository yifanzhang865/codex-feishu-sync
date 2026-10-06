package feishu

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	channel "github.com/larksuite/channel-sdk-go"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func stringPtr(value string) *string { return &value }

func TestCleanupChecksBindingAndProtectsHumanOwner(t *testing.T) {
	chat := &larkim.GetChatRespData{Description: stringPtr("Codex thread: thread-a"), ChatMode: stringPtr("group"), ChatType: stringPtr("private")}
	if !matchesManagedThreadChat(chat, "thread-a") {
		t.Fatal("bot-owned auto-created group was not recognized")
	}
	chat.OwnerId = stringPtr("human-owner")
	if matchesManagedThreadChat(chat, "thread-a") {
		t.Fatal("human-owned group was eligible")
	}
	chat.OwnerId = nil
	chat.Description = stringPtr("another project")
	if matchesManagedThreadChat(chat, "thread-a") {
		t.Fatal("changed group binding was eligible")
	}
}

func TestReadDissolvedChatStatusSupportsCrashRecovery(t *testing.T) {
	for _, status := range []string{"normal", "dissolved", "dissolved_save", "unknown"} {
		t.Run(status, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				response := map[string]any{"code": 0}
				switch r.URL.Path {
				case "/open-apis/auth/v3/tenant_access_token/internal":
					response["tenant_access_token"] = "fake-token"
					response["expire"] = 7200
				case "/open-apis/im/v1/chats/chat":
					if r.Method != http.MethodGet {
						t.Error("recovery probe modified the group")
					}
					data := map[string]any{"chat_status": status, "bot_count": "0", "user_count": "0"}
					if status == "normal" {
						data["description"] = "Codex thread: thread-a"
						data["chat_mode"] = "group"
						data["chat_type"] = "private"
					}
					response["data"] = data
				default:
					t.Errorf("unexpected API path %s", r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			sdk, err := channel.New("status-test-"+status, "fake-secret", channel.WithDomain(server.URL))
			if err != nil {
				t.Fatal(err)
			}
			client := &Client{sdk: sdk}
			allowed, err := client.ThreadChatCanBeDeleted(context.Background(), "chat", "thread-a")
			switch status {
			case "normal":
				if !allowed || err != nil {
					t.Fatalf("normal group failed ownership check: %v, %v", allowed, err)
				}
			case "dissolved", "dissolved_save":
				if allowed || !errors.Is(err, ErrChatDissolved) {
					t.Fatalf("successful GET of dissolved group lost recovery state: %v, %v", allowed, err)
				}
			default:
				if allowed || err == nil || errors.Is(err, ErrChatDissolved) {
					t.Fatal("unknown group status did not fail closed")
				}
			}
		})
	}
}

func TestHistoryPaginationSkipsBotMessagesAndDeleteIsIdempotent(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	var pages int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		response := map[string]any{"code": 0}
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			response["tenant_access_token"] = "fake-token"
			response["expire"] = 7200
		case "/open-apis/im/v1/messages":
			pages++
			if r.URL.Query().Get("sort_type") != "ByCreateTimeDesc" || r.URL.Query().Get("start_time") == "" {
				t.Error("history query omitted order or cutoff")
			}
			sender := "app"
			more := true
			token := "page-two"
			if r.URL.Query().Get("page_token") == "page-two" {
				sender = "user"
				more = false
				token = ""
			}
			response["data"] = map[string]any{"has_more": more, "page_token": token, "items": []any{map[string]any{"msg_type": "text", "sender": map[string]any{"sender_type": sender}, "create_time": strconv.FormatInt(now.UnixMilli(), 10)}}}
		case "/open-apis/im/v1/chats/chat":
			if r.Method != http.MethodDelete {
				t.Error("wrong delete method")
			}
			response["code"] = 232009
			response["msg"] = "already dissolved"
		default:
			t.Errorf("unexpected API path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	sdk, err := channel.New("fake-app", "fake-secret", channel.WithDomain(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{sdk: sdk}
	at, err := client.LastHumanMessage(context.Background(), "chat", now.Add(-72*time.Hour))
	if err != nil || !at.Equal(now) || pages != 2 {
		t.Fatalf("human activity after bot page missed: %v, %v, pages=%d", at, err, pages)
	}
	if err := client.DeleteThreadChat(context.Background(), "chat"); err != nil {
		t.Fatalf("dissolved group was not idempotent: %v", err)
	}
}

func TestOnlyHumanMessagesRenewGroupActivity(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	message := &larkim.Message{CreateTime: stringPtr(strconv.FormatInt(now.UnixMilli(), 10)), Sender: &larkim.Sender{SenderType: stringPtr("app")}}
	at, err := humanMessageTime(message)
	if err != nil || !at.IsZero() {
		t.Fatal("bot heartbeat renewed activity")
	}
	message.Sender.SenderType = stringPtr("user")
	at, err = humanMessageTime(message)
	if err != nil || !at.Equal(now) {
		t.Fatal("human activity was missed")
	}
	message.CreateTime = stringPtr("bad timestamp")
	if _, err := humanMessageTime(message); err == nil {
		t.Fatal("invalid time did not fail closed")
	}
	if at, err := humanMessageTime(&larkim.Message{MsgType: stringPtr("system")}); err != nil || !at.IsZero() {
		t.Fatal("membership notice counted as dialogue")
	}
	if !errors.Is(lifecycleAPIError(99991672, "missing scope"), ErrPermissionDenied) || !errors.Is(lifecycleAPIError(232009, "dissolved"), ErrChatDissolved) {
		t.Fatal("recovery error categories were lost")
	}
}

func TestCleanupProtectsAnotherMachinesManagedGroup(t *testing.T) {
	chat := &larkim.GetChatRespData{Description: stringPtr("Codex thread: thread-a\nCodex machine: host-b"), ChatMode: stringPtr("group"), ChatType: stringPtr("private")}
	if matchesManagedThreadChat(chat, "thread-a", "host-a") {
		t.Fatal("another host's group could be deleted")
	}
	if !matchesManagedThreadChat(chat, "thread-a", "host-b") {
		t.Fatal("own managed group was not recognized")
	}
	chat.Description = stringPtr("Codex thread: thread-a")
	if !matchesManagedThreadChat(chat, "thread-a", "host-b") {
		t.Fatal("legacy managed group lost upgrade compatibility")
	}
}
