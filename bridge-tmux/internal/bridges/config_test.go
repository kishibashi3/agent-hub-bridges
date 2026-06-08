package bridges

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_ValidConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridges.json")
	if err := os.WriteFile(path, []byte(`{
		"bridges": [
			{
				"handle": "reviewer",
				"workdir": "/tmp/reviewer",
				"tenant": "kishibashi3",
				"model": "claude-sonnet-4-6",
				"timeout": "10m"
			},
			{
				"handle": "@planner",
				"workdir": "/tmp/planner"
			}
		]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Bridges) != 2 {
		t.Fatalf("got %d bridges, want 2", len(cfg.Bridges))
	}
	// @ prefix should be stripped
	if cfg.Bridges[1].Handle != "planner" {
		t.Errorf("Handle = %q, want planner", cfg.Bridges[1].Handle)
	}
}

func TestLoad_NotExist(t *testing.T) {
	_, err := Load("/nonexistent/bridges.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist in error chain, got: %v", err)
	}
}

func TestLoad_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridges.json")
	os.WriteFile(path, []byte("not json"), 0o644)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected parse error")
	}
}

func TestLoad_MissingHandle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridges.json")
	os.WriteFile(path, []byte(`{"bridges":[{"workdir":"/tmp"}]}`), 0o644)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for missing handle")
	}
}

func TestLoad_MissingWorkdir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridges.json")
	os.WriteFile(path, []byte(`{"bridges":[{"handle":"reviewer"}]}`), 0o644)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for missing workdir")
	}
}

func TestLoad_InvalidTimeout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridges.json")
	os.WriteFile(path, []byte(`{"bridges":[{"handle":"reviewer","workdir":"/tmp","timeout":"bad"}]}`), 0o644)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid timeout")
	}
}

func TestLookup_Found(t *testing.T) {
	cfg := &Config{
		Bridges: []Entry{
			{Handle: "reviewer", Workdir: "/tmp/reviewer"},
			{Handle: "planner", Workdir: "/tmp/planner"},
		},
	}
	e := cfg.Lookup("@reviewer")
	if e == nil {
		t.Fatal("expected to find reviewer")
	}
	if e.Workdir != "/tmp/reviewer" {
		t.Errorf("Workdir = %q, want /tmp/reviewer", e.Workdir)
	}
}

func TestLookup_WithoutAt(t *testing.T) {
	cfg := &Config{Bridges: []Entry{{Handle: "reviewer", Workdir: "/tmp"}}}
	if cfg.Lookup("reviewer") == nil {
		t.Error("Lookup without @ should work")
	}
}

func TestLookup_NotFound(t *testing.T) {
	cfg := &Config{Bridges: []Entry{{Handle: "reviewer", Workdir: "/tmp"}}}
	if cfg.Lookup("@planner") != nil {
		t.Error("expected nil for unknown handle")
	}
}

func TestDefaultConfigPath_WithEnv(t *testing.T) {
	t.Setenv("AGENTHUBCTL_CONFIG_DIR", "/custom/dir")
	path, err := DefaultConfigPath()
	if err != nil {
		t.Fatalf("DefaultConfigPath: %v", err)
	}
	if path != "/custom/dir/bridges.json" {
		t.Errorf("got %q, want /custom/dir/bridges.json", path)
	}
}

func TestDefaultConfigPath_Default(t *testing.T) {
	t.Setenv("AGENTHUBCTL_CONFIG_DIR", "")
	path, err := DefaultConfigPath()
	if err != nil {
		t.Fatalf("DefaultConfigPath: %v", err)
	}
	if path == "" {
		t.Error("expected non-empty path")
	}
}
