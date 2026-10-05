package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type SyncLevel string
type SendTiming string

const (
	ConversationStatus SyncLevel = "conversation_status"
	WithTools          SyncLevel = "with_tools"
	AllVisible         SyncLevel = "all_visible"

	AfterTurn SendTiming = "after_turn"
	Streaming SendTiming = "streaming"
)

type Config struct {
	Region                   string     `json:"region"`
	SyncLevel                SyncLevel  `json:"sync_level"`
	SendTiming               SendTiming `json:"send_timing"`
	OwnerOpenID              string     `json:"owner_open_id"`
	CodexBinary              string     `json:"codex_binary"`
	InstalledBinary          string     `json:"installed_binary"`
	Marketplace              string     `json:"marketplace"`
	AutoCreateGroup          bool       `json:"auto_create_group"`
	ReadOnly                 bool       `json:"read_only"`
	SyncAllSessions          bool       `json:"sync_all_sessions"`
	SessionActiveHours       int        `json:"session_active_hours"`
	GroupIdleHours           int        `json:"group_idle_hours"`
	AutoDeleteInactiveGroups bool       `json:"auto_delete_inactive_groups"`
}

type Credentials struct {
	AppID     string `json:"app_id"`
	AppSecret string `json:"app_secret"`
}

func Defaults() Config {
	return Config{
		Region:                   "feishu",
		SyncLevel:                ConversationStatus,
		SendTiming:               AfterTurn,
		Marketplace:              "codex-feishu-sync",
		AutoCreateGroup:          true,
		ReadOnly:                 true,
		SyncAllSessions:          true,
		SessionActiveHours:       72,
		GroupIdleHours:           72,
		AutoDeleteInactiveGroups: true,
	}
}

func (cfg Config) Validate() error {
	if cfg.Region != "feishu" && cfg.Region != "lark" {
		return fmt.Errorf("region must be feishu or lark")
	}
	switch cfg.SyncLevel {
	case ConversationStatus, WithTools, AllVisible:
	default:
		return fmt.Errorf("unsupported sync level %q", cfg.SyncLevel)
	}
	switch cfg.SendTiming {
	case AfterTurn, Streaming:
	default:
		return fmt.Errorf("unsupported send timing %q", cfg.SendTiming)
	}
	if cfg.SessionActiveHours < 0 || cfg.SessionActiveHours > 24*365 || cfg.GroupIdleHours < 0 || cfg.GroupIdleHours > 24*365 {
		return errors.New("session_active_hours and group_idle_hours must be between 0 and 8760")
	}
	if cfg.AutoDeleteInactiveGroups && (!cfg.SyncAllSessions || cfg.GroupIdleHours == 0) {
		return errors.New("auto_delete_inactive_groups requires sync_all_sessions and positive group_idle_hours")
	}
	return nil
}

func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	return filepath.Join(base, "codex-feishu-sync"), nil
}

func Load(dir string) (Config, error) {
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return Config{}, err
	}
	// Preserve legacy control flags; only the new time windows get defaults.
	cfg := Config{SessionActiveHours: 72, GroupIdleHours: 72}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func Save(dir string, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	return writeJSON(filepath.Join(dir, "config.json"), cfg, 0600)
}

func LoadCredentials(dir string) (Credentials, error) {
	data, err := os.ReadFile(filepath.Join(dir, "credentials.json"))
	if err != nil {
		return Credentials{}, err
	}
	var credentials Credentials
	if err := json.Unmarshal(data, &credentials); err != nil {
		return Credentials{}, fmt.Errorf("decode credentials: %w", err)
	}
	if credentials.AppID == "" || credentials.AppSecret == "" {
		return Credentials{}, errors.New("app_id and app_secret are required")
	}
	return credentials, nil
}

func SaveCredentials(dir string, credentials Credentials) error {
	if credentials.AppID == "" || credentials.AppSecret == "" {
		return errors.New("app_id and app_secret are required")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	return writeJSON(filepath.Join(dir, "credentials.json"), credentials, 0600)
}

func writeJSON(path string, value any, mode os.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".codex-feishu-sync-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}
