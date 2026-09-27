package config

import "testing"

func TestDefaultsUseVisibleConversationAndTurnCompletion(t *testing.T) {
	cfg := Defaults()
	if cfg.SyncLevel != ConversationStatus {
		t.Fatalf("SyncLevel = %q, want %q", cfg.SyncLevel, ConversationStatus)
	}
	if cfg.SendTiming != AfterTurn {
		t.Fatalf("SendTiming = %q, want %q", cfg.SendTiming, AfterTurn)
	}
	if cfg.Marketplace != "codex-feishu-sync" {
		t.Fatalf("Marketplace = %q, want codex-feishu-sync", cfg.Marketplace)
	}
}

func TestValidateRejectsUnsupportedValues(t *testing.T) {
	cfg := Defaults()
	cfg.SyncLevel = "reasoning"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() accepted unsupported sync level")
	}

	cfg = Defaults()
	cfg.Region = "unknown"
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate() accepted unsupported region")
	}
}
