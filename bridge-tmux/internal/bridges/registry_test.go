package bridges

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultPath(t *testing.T) {
	path, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	if filepath.Base(path) != "bridges.json" {
		t.Errorf("expected bridges.json, got %q", filepath.Base(path))
	}
	if filepath.Base(filepath.Dir(path)) != ".agent-hub" {
		t.Errorf("expected .agent-hub dir, got %q", filepath.Dir(path))
	}
}

func TestLoad_NotExist(t *testing.T) {
	r, err := Load("/nonexistent/bridges.json")
	if err != nil {
		t.Fatalf("expected empty registry, got error: %v", err)
	}
	if len(r) != 0 {
		t.Errorf("expected empty registry, got %d entries", len(r))
	}
}

func TestLoad_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridges.json")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestSave_Load_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridges.json")

	r := Registry{
		"deep-research": {
			Handle:    "deep-research",
			Bin:       "bridge-claude2",
			Workdir:   "/roles/deep-research",
			Tenant:    "kaz",
			PID:       12345,
			StartedAt: "2026-06-08T12:00:00Z",
		},
		"reviewer": {
			Handle:  "reviewer",
			Bin:     "bridge-claude2",
			Workdir: "/roles/reviewer",
		},
	}

	if err := Save(path, r); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	dr := got["deep-research"]
	if dr == nil {
		t.Fatal("deep-research entry missing")
	}
	if dr.Bin != "bridge-claude2" {
		t.Errorf("Bin = %q, want bridge-claude2", dr.Bin)
	}
	if dr.PID != 12345 {
		t.Errorf("PID = %d, want 12345", dr.PID)
	}
	if dr.Tenant != "kaz" {
		t.Errorf("Tenant = %q, want kaz", dr.Tenant)
	}
}

func TestSave_CreatesParentDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deep", "bridges.json")

	r := Registry{"x": {Handle: "x", Bin: "bridge-claude2", Workdir: "/tmp/x"}}
	if err := Save(path, r); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

func TestSave_TenantOmitEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridges.json")

	r := Registry{
		"x": {Handle: "x", Bin: "bridge-claude2", Workdir: "/tmp/x"},
	}
	if err := Save(path, r); err != nil {
		t.Fatalf("Save: %v", err)
	}

	data, _ := os.ReadFile(path)
	var raw map[string]map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := raw["x"]["tenant"]; ok {
		t.Error("expected tenant to be omitted when empty, but it was present")
	}
}
