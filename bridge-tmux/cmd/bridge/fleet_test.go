package main

import (
	"context"
	"testing"
	"time"
)

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
