package config

import (
	"os"
	"path/filepath"
	"testing"
)

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

func TestAllSessionsRequiresReadOnlyAndPersists(t *testing.T) {
	cfg := Defaults()
	cfg.SyncAllSessions = true
	if err := cfg.Validate(); err == nil {
		t.Fatal("all-session discovery accepted control mode")
	}
	cfg.ReadOnly = true
	dir := t.TempDir()
	if err := Save(dir, cfg); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(dir)
	if err != nil || !loaded.ReadOnly || !loaded.SyncAllSessions {
		t.Fatalf("read-only scope did not persist: %#v, %v", loaded, err)
	}
}

func TestLegacyConfigurationPreservesControlMode(t *testing.T) {
	dir := t.TempDir()
	data := []byte(`{"region":"feishu","sync_level":"conversation_status","send_timing":"after_turn"}`)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil || cfg.ReadOnly || cfg.SyncAllSessions {
		t.Fatalf("legacy configuration changed mode: %#v, %v", cfg, err)
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
