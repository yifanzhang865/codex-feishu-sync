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
	if cfg.ReadOnly || !cfg.SyncAllSessions || !cfg.AutoDeleteInactiveGroups || cfg.SessionActiveHours != 72 || cfg.GroupIdleHours != 72 {
		t.Fatalf("fresh deployment must enable automatic continuation with the 72-hour lifecycle: %#v", cfg)
	}
}

func TestAllSessionsAndLifecycleSupportReadOnlyAndControl(t *testing.T) {
	for _, readOnly := range []bool{true, false} {
		cfg := Defaults()
		cfg.ReadOnly = readOnly
		dir := t.TempDir()
		if err := Save(dir, cfg); err != nil {
			t.Fatal(err)
		}
		loaded, err := Load(dir)
		if err != nil || loaded.ReadOnly != readOnly || !loaded.SyncAllSessions {
			t.Fatalf("session control mode did not persist: %#v, %v", loaded, err)
		}
		if loaded.SessionActiveHours != 72 || loaded.GroupIdleHours != 72 || !loaded.AutoDeleteInactiveGroups {
			t.Fatal("lifecycle settings did not persist")
		}
	}
}

func TestLegacyConfigurationPreservesControlMode(t *testing.T) {
	dir := t.TempDir()
	data := []byte(`{"region":"feishu","sync_level":"conversation_status","send_timing":"after_turn"}`)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil || cfg.ReadOnly || cfg.SyncAllSessions || cfg.AutoDeleteInactiveGroups {
		t.Fatalf("legacy configuration changed mode: %#v, %v", cfg, err)
	}
}

func TestInvalidLifecycleCannotDeleteGroups(t *testing.T) {
	for _, change := range []func(*Config){
		func(c *Config) { c.GroupIdleHours = 0 },
		func(c *Config) { c.SyncAllSessions = false },
		func(c *Config) { c.SessionActiveHours = -1 },
	} {
		cfg := Defaults()
		change(&cfg)
		if cfg.Validate() == nil {
			t.Fatalf("unsafe lifecycle was accepted: %#v", cfg)
		}
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

func TestMachineIdentityIsStableAndNotSharedBetweenInstalls(t *testing.T) {
	firstDir, secondDir := t.TempDir(), t.TempDir()
	first, second := Defaults(), Defaults()
	if err := EnsureMachine(firstDir, &first); err != nil {
		t.Fatal(err)
	}
	if err := EnsureMachine(secondDir, &second); err != nil {
		t.Fatal(err)
	}
	if first.MachineID == "" || first.MachineID == second.MachineID || first.MachineName == "" {
		t.Fatal("installations did not get independent machine identities")
	}
	reloaded, err := Load(firstDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureMachine(firstDir, &reloaded); err != nil {
		t.Fatal(err)
	}
	if reloaded.MachineID != first.MachineID {
		t.Fatal("machine identity changed on restart")
	}
	for _, interval := range []int{-1, 1, 301} {
		reloaded.MessagePollSeconds = interval
		if reloaded.Validate() == nil {
			t.Fatalf("accepted invalid poll interval %d", interval)
		}
	}
}
