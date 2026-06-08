package main

import (
	"os"
	"path/filepath"
	"testing"
)

// ──────────────────────────────────────────────────────────────────────── //
// handleRe                                                                  //
// ──────────────────────────────────────────────────────────────────────── //

func TestHandleRe_Valid(t *testing.T) {
	valid := []string{
		"deep-research", "reviewer", "planner", "_template",
		"bridge-claude2", "a", "A1", "my_role",
	}
	for _, h := range valid {
		if !handleRe.MatchString(h) {
			t.Errorf("handleRe should match %q", h)
		}
	}
}

func TestHandleRe_Invalid(t *testing.T) {
	invalid := []string{
		"", "../etc", "foo/bar", "foo bar", "-start",
	}
	for _, h := range invalid {
		if handleRe.MatchString(h) {
			t.Errorf("handleRe should not match %q", h)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// resolveWorkdir                                                            //
// ──────────────────────────────────────────────────────────────────────── //

func TestResolveWorkdir_Explicit_Exists(t *testing.T) {
	dir := t.TempDir()
	got, err := resolveWorkdir("reviewer", dir)
	if err != nil {
		t.Fatalf("resolveWorkdir: %v", err)
	}
	if got != dir {
		t.Errorf("got %q, want %q", got, dir)
	}
}

func TestResolveWorkdir_Explicit_Creates(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "new-dir")
	got, err := resolveWorkdir("reviewer", target)
	if err != nil {
		t.Fatalf("resolveWorkdir: %v", err)
	}
	if got != target {
		t.Errorf("got %q, want %q", got, target)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("expected dir to be created: %v", err)
	}
}

func TestResolveWorkdir_AutoDetect_Found(t *testing.T) {
	rolesDir := t.TempDir()
	handleDir := filepath.Join(rolesDir, "reviewer")
	if err := os.MkdirAll(handleDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Setenv("AGENT_HUB_ROLES", rolesDir)

	got, err := resolveWorkdir("reviewer", "")
	if err != nil {
		t.Fatalf("resolveWorkdir: %v", err)
	}
	if got != handleDir {
		t.Errorf("got %q, want %q", got, handleDir)
	}
}

func TestResolveWorkdir_AutoDetect_NoRolesEnv(t *testing.T) {
	t.Setenv("AGENT_HUB_ROLES", "")
	_, err := resolveWorkdir("reviewer", "")
	if err == nil {
		t.Fatal("expected error when AGENT_HUB_ROLES is unset, got nil")
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// maybeApplyTemplate                                                        //
// ──────────────────────────────────────────────────────────────────────── //

func TestMaybeApplyTemplate_CopiesWhenAbsent(t *testing.T) {
	rolesDir := t.TempDir()
	templateDir := filepath.Join(rolesDir, "_template")
	if err := os.MkdirAll(templateDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(templateDir, "CLAUDE.md"), []byte("# Template\n"), 0o644); err != nil {
		t.Fatalf("write template: %v", err)
	}
	t.Setenv("AGENT_HUB_ROLES", rolesDir)

	workdir := t.TempDir()
	if err := maybeApplyTemplate(workdir, "_template"); err != nil {
		t.Fatalf("maybeApplyTemplate: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(workdir, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("read CLAUDE.md: %v", err)
	}
	if string(data) != "# Template\n" {
		t.Errorf("unexpected content: %q", string(data))
	}
}

func TestMaybeApplyTemplate_SkipsWhenExists(t *testing.T) {
	rolesDir := t.TempDir()
	templateDir := filepath.Join(rolesDir, "_template")
	if err := os.MkdirAll(templateDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(templateDir, "CLAUDE.md"), []byte("# Template\n"), 0o644); err != nil {
		t.Fatalf("write template: %v", err)
	}
	t.Setenv("AGENT_HUB_ROLES", rolesDir)

	workdir := t.TempDir()
	existing := "# Existing content\n"
	if err := os.WriteFile(filepath.Join(workdir, "CLAUDE.md"), []byte(existing), 0o644); err != nil {
		t.Fatalf("write existing: %v", err)
	}

	if err := maybeApplyTemplate(workdir, "_template"); err != nil {
		t.Fatalf("maybeApplyTemplate: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(workdir, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("read CLAUDE.md: %v", err)
	}
	if string(data) != existing {
		t.Errorf("expected existing content preserved, got %q", string(data))
	}
}

func TestMaybeApplyTemplate_MissingTemplateWarnsOnly(t *testing.T) {
	rolesDir := t.TempDir()
	t.Setenv("AGENT_HUB_ROLES", rolesDir)

	workdir := t.TempDir()
	// _template dir does not exist — should warn but not error
	if err := maybeApplyTemplate(workdir, "_template"); err != nil {
		t.Fatalf("expected nil error for missing template, got: %v", err)
	}
}

func TestMaybeApplyTemplate_NoRolesEnv(t *testing.T) {
	t.Setenv("AGENT_HUB_ROLES", "")
	workdir := t.TempDir()
	// No CLAUDE.md in workdir, no AGENT_HUB_ROLES — should warn but not error
	if err := maybeApplyTemplate(workdir, "_template"); err != nil {
		t.Fatalf("expected nil error when AGENT_HUB_ROLES unset, got: %v", err)
	}
}

func TestMaybeApplyTemplate_PathTraversal(t *testing.T) {
	rolesDir := t.TempDir()
	t.Setenv("AGENT_HUB_ROLES", rolesDir)
	workdir := t.TempDir()

	// Create a _template that looks benign but handleRe blocks "../etc"
	// handleRe rejects traversal handles before maybeApplyTemplate is called,
	// but the internal check also catches it.
	err := maybeApplyTemplate(workdir, "../outside")
	if err == nil {
		t.Fatal("expected error for path traversal template, got nil")
	}
}

// ──────────────────────────────────────────────────────────────────────── //
// resolveBin                                                                //
// ──────────────────────────────────────────────────────────────────────── //

func TestResolveBin_FromAgentHubBin(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bridge-claude2")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("create fake binary: %v", err)
	}
	t.Setenv("AGENT_HUB_BIN", dir)

	got, err := resolveBin("bridge-claude2")
	if err != nil {
		t.Fatalf("resolveBin: %v", err)
	}
	if got != bin {
		t.Errorf("got %q, want %q", got, bin)
	}
}

func TestResolveBin_FallbackPath(t *testing.T) {
	t.Setenv("AGENT_HUB_BIN", "")
	// "sh" should always be in PATH
	got, err := resolveBin("sh")
	if err != nil {
		t.Fatalf("resolveBin(sh): %v", err)
	}
	if got == "" {
		t.Error("expected non-empty path for sh")
	}
}

func TestResolveBin_NotFound(t *testing.T) {
	t.Setenv("AGENT_HUB_BIN", "")
	_, err := resolveBin("this-binary-does-not-exist-anywhere-12345")
	if err == nil {
		t.Fatal("expected error for missing binary, got nil")
	}
}
