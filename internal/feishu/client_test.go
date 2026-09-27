package feishu

import (
	"encoding/json"
	"testing"
)

func TestThreadChatBodyInvitesOwnerWithoutInvitingItsCreatingBotAgain(t *testing.T) {
	body, err := json.Marshal(threadChatBody("thread-123", "project", "owner-open-id"))
	if err != nil {
		t.Fatal(err)
	}
	var actual struct {
		ChatType string   `json:"chat_type"`
		UserIDs  []string `json:"user_id_list"`
		BotIDs   []string `json:"bot_id_list"`
		Name     string   `json:"name"`
		Desc     string   `json:"description"`
	}
	if err := json.Unmarshal(body, &actual); err != nil {
		t.Fatal(err)
	}
	if actual.ChatType != "private" || len(actual.UserIDs) != 1 || actual.UserIDs[0] != "owner-open-id" {
		t.Fatalf("created chat fields = %#v", actual)
	}
	if len(actual.BotIDs) != 0 {
		t.Fatalf("creating bot was redundantly invited: %#v", actual.BotIDs)
	}
	if actual.Name != "Codex - project" || actual.Desc != "Codex thread: thread-123" {
		t.Fatalf("created chat metadata = %#v", actual)
	}
}
