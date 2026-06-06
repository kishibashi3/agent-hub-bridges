package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ──────────────────────────────────────────────────────────────────────── //
// LoadFleetConfig                                                           //
// ──────────────────────────────────────────────────────────────────────── //

func TestLoadFleetConfig_Valid(t *testing.T) {
	yaml := `
personas:
  - handle: reviewer
    workdir: /tmp/reviewer
    bypass_permissions: true
    idle_timeout: 10m
    model: claude-opus-4-7
  - handle: planner
    workdir: /tmp/planner
    idle_timeout: 15m
    env:
      MY_VAR: hello
`
	path := writeTempFleet(t, yaml)
	cfg, err := LoadFleetConfig(path)
	if err != nil {
		t.Fatalf("LoadFleetConfig: %v", err)
	}
	if len(cfg.Personas) != 2 {
		t.Fatalf("got %d personas, want 2", len(cfg.Personas))
	}

	r := cfg.Personas[0]
	if r.Handle != "reviewer" {
		t.Errorf("Handle = %q, want reviewer", r.Handle)
	}
	if r.Workdir != "/tmp/reviewer" {
		t.Errorf("Workdir = %q, want /tmp/reviewer", r.Workdir)
	}
	if !r.BypassPermissions {
		t.Error("BypassPermissions = false, want true")
	}
	if r.IdleTimeout.Duration != 10*time.Minute {
		t.Errorf("IdleTimeout = %v, want 10m", r.IdleTimeout.Duration)
	}
	if r.Model != "claude-opus-4-7" {
		t.Errorf("Model = %q, want claude-opus-4-7", r.Model)
	}

	p := cfg.Personas[1]
	if p.Handle != "planner" {
		t.Errorf("Handle = %q, want planner", p.Handle)
	}
	if p.Env["MY_VAR"] != "hello" {
		t.Errorf("Env[MY_VAR] = %q, want hello", p.Env["MY_VAR"])
	}
}

func TestLoadFleetConfig_MissingFile(t *testing.T) {
	_, err := LoadFleetConfig("/nonexistent/fleet.yaml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestLoadFleetConfig_InvalidYAML(t *testing.T) {
	path := writeTempFleet(t, "personas: [invalid: {unclosed")
	_, err := LoadFleetConfig(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
}

func TestLoadFleetConfig_EmptyPersonas(t *testing.T) {
	path := writeTempFleet(t, "personas: []\n")
	_, err := LoadFleetConfig(path)
	if err == nil {
		t.Fatal("expected error for empty personas, got nil")
	}
}

func TestLoadFleetConfig_InvalidDuration(t *testing.T) {
	yaml := `
personas:
  - handle: reviewer
    workdir: /tmp/reviewer
    idle_timeout: notaduration
`
	path := writeTempFleet(t, yaml)
	_, err := LoadFleetConfig(path)
	if err == nil {
		t.Fatal("expected error for invalid duration, got nil")
	}
}

func TestLoadFleetConfig_MissingHandle(t *testing.T) {
	// handle が空 → エラー
	yaml := `
personas:
  - workdir: /tmp/reviewer
`
	path := writeTempFleet(t, yaml)
	_, err := LoadFleetConfig(path)
	if err == nil {
		t.Fatal("expected error for missing handle, got nil")
	}
}

func TestLoadFleetConfig_MissingWorkdir(t *testing.T) {
	// workdir が空 → エラー
	yaml := `
personas:
  - handle: reviewer
`
	path := writeTempFleet(t, yaml)
	_, err := LoadFleetConfig(path)
	if err == nil {
		t.Fatal("expected error for missing workdir, got nil")
	}
}

func TestLoadFleetConfig_InvalidEnvKey(t *testing.T) {
	// env キー名が不正 (シェルインジェクション防止 Critical #1)
	yaml := `
personas:
  - handle: reviewer
    workdir: /tmp/reviewer
    env:
      "FOO=bar; curl evil.com": injected
`
	path := writeTempFleet(t, yaml)
	_, err := LoadFleetConfig(path)
	if err == nil {
		t.Fatal("expected error for invalid env key, got nil")
	}
}

func TestLoadFleetConfig_ValidEnvKey(t *testing.T) {
	// 有効な env キー名は通る
	yaml := `
personas:
  - handle: reviewer
    workdir: /tmp/reviewer
    env:
      MY_VAR: hello
      _UNDERSCORE: ok
      VAR123: fine
`
	path := writeTempFleet(t, yaml)
	cfg, err := LoadFleetConfig(path)
	if err != nil {
		t.Fatalf("LoadFleetConfig: %v", err)
	}
	if cfg.Personas[0].Env["MY_VAR"] != "hello" {
		t.Errorf("Env[MY_VAR] = %q, want hello", cfg.Personas[0].Env["MY_VAR"])
	}
}

func TestLoadFleetConfig_UnknownField(t *testing.T) {
	// unknown フィールドは KnownFields(true) によりエラー
	yaml := `
personas:
  - handle: reviewer
    workdir: /tmp/reviewer
    unknown_field: oops
`
	path := writeTempFleet(t, yaml)
	_, err := LoadFleetConfig(path)
	if err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}
}

func TestLoadFleetConfig_DefaultValues(t *testing.T) {
	// bypass_permissions 未指定 → false (default)、idle_timeout 未指定 → 0
	yaml := `
personas:
  - handle: writer
    workdir: /tmp/writer
`
	path := writeTempFleet(t, yaml)
	cfg, err := LoadFleetConfig(path)
	if err != nil {
		t.Fatalf("LoadFleetConfig: %v", err)
	}
	p := cfg.Personas[0]
	if p.BypassPermissions {
		t.Error("BypassPermissions should default to false")
	}
	if p.IdleTimeout.Duration != 0 {
		t.Errorf("IdleTimeout should default to 0, got %v", p.IdleTimeout.Duration)
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// personaToConfig                                                           //
// ──────────────────────────────────────────────────────────────────────── //

func TestPersonaToConfig_OverridesApplied(t *testing.T) {
	global := &config{
		User:             "old-user",
		Workdir:          "/old/workdir",
		Model:            "",
		BypassPerms:      false,
		IdleTimeout:      10 * time.Minute,
		AgentHubURL:      "http://hub:3000",
		GitHubPAT:        "ghs_token",
		SpawnTimeout:     60 * time.Second,
		ActivityIdle:     8 * time.Second,
		ResponseTimeout:  5 * time.Minute,
		PollInterval:     5 * time.Second,
		ReconnectBackoff: 5 * time.Second,
		MaxRetries:       10,
	}
	p := PersonaConfig{
		Handle:            "reviewer",
		DisplayName:       "Reviewer Agent",
		Workdir:           "/roles/reviewer",
		Model:             "claude-opus-4-7",
		BypassPermissions: true,
		IdleTimeout:       yamlDuration{15 * time.Minute},
	}
	got := personaToConfig(p, global)

	if got.User != "reviewer" {
		t.Errorf("User = %q, want reviewer", got.User)
	}
	if got.DisplayName != "Reviewer Agent" {
		t.Errorf("DisplayName = %q, want Reviewer Agent", got.DisplayName)
	}
	if got.Workdir != "/roles/reviewer" {
		t.Errorf("Workdir = %q, want /roles/reviewer", got.Workdir)
	}
	if got.Model != "claude-opus-4-7" {
		t.Errorf("Model = %q, want claude-opus-4-7", got.Model)
	}
	if !got.BypassPerms {
		t.Error("BypassPerms = false, want true")
	}
	if got.IdleTimeout != 15*time.Minute {
		t.Errorf("IdleTimeout = %v, want 15m", got.IdleTimeout)
	}
	// グローバル設定は引き継がれる
	if got.AgentHubURL != "http://hub:3000" {
		t.Errorf("AgentHubURL = %q, want http://hub:3000", got.AgentHubURL)
	}
	if got.SpawnTimeout != 60*time.Second {
		t.Errorf("SpawnTimeout = %v, want 60s", got.SpawnTimeout)
	}
}

func TestPersonaToConfig_IdleTimeoutFallback(t *testing.T) {
	// idle_timeout が 0 の場合はグローバル値を維持
	global := &config{IdleTimeout: 20 * time.Minute}
	p := PersonaConfig{Handle: "writer", Workdir: "/tmp"}
	got := personaToConfig(p, global)
	if got.IdleTimeout != 20*time.Minute {
		t.Errorf("IdleTimeout = %v, want 20m (fallback)", got.IdleTimeout)
	}
}

func TestPersonaToConfig_GlobalNotMutated(t *testing.T) {
	// personaToConfig は global をコピーするため元の config が変わらないこと
	global := &config{User: "original", IdleTimeout: 10 * time.Minute}
	p := PersonaConfig{Handle: "reviewer", Workdir: "/tmp", IdleTimeout: yamlDuration{5 * time.Minute}}
	_ = personaToConfig(p, global)
	if global.User != "original" {
		t.Errorf("global.User was mutated: %q", global.User)
	}
	if global.IdleTimeout != 10*time.Minute {
		t.Errorf("global.IdleTimeout was mutated: %v", global.IdleTimeout)
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// RunFleet                                                                  //
// ──────────────────────────────────────────────────────────────────────── //

func TestRunFleet_ContextCancel(t *testing.T) {
	// context がすぐキャンセルされた場合、RunFleet が nil を返すこと
	fleet := &FleetConfig{
		Personas: []PersonaConfig{
			{Handle: "p1", Workdir: "/tmp"},
			{Handle: "p2", Workdir: "/tmp"},
		},
	}
	global := &config{
		AgentHubURL:      "http://localhost:0",  // 接続しない
		GitHubPAT:        "test-pat",
		SpawnTimeout:     time.Second,
		ActivityIdle:     time.Second,
		ResponseTimeout:  time.Second,
		PollInterval:     10 * time.Millisecond,
		ReconnectBackoff: 10 * time.Millisecond,
		IdleTimeout:      time.Minute,
		MaxRetries:       1,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 即キャンセル

	err := RunFleet(ctx, global, fleet, NewHealthState("fleet"))
	// ctx がキャンセル済みなら各 persona は nil を返すため RunFleet も nil
	if err != nil {
		t.Errorf("RunFleet with cancelled ctx: got err %v, want nil", err)
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// firstLine                                                                 //
// ──────────────────────────────────────────────────────────────────────── //

func TestFirstLine(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"hello\nworld", "hello"},
		{"single", "single"},
		{"", ""},
		{"\nsecond", ""},
	}
	for _, tt := range tests {
		if got := firstLine(tt.in); got != tt.want {
			t.Errorf("firstLine(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// ヘルパー                                                                  //
// ──────────────────────────────────────────────────────────────────────── //

func writeTempFleet(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "fleet-*.yaml")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	f.Close()
	return filepath.Clean(f.Name())
}
