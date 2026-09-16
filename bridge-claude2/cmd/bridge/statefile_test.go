package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestStateKey(t *testing.T) {
	if got := (&config{Participant: "bot"}).stateKey(); got != "bot" {
		t.Errorf("no tenant: got %q", got)
	}
	if got := (&config{Participant: "bot", Tenant: "kaz"}).stateKey(); got != "kaz__bot" {
		t.Errorf("tenant: got %q", got)
	}
}

func TestValidateTenantForFileName(t *testing.T) {
	for _, ok := range []string{"", "kaz", "test-tenant_1"} {
		if err := validateTenantForFileName(ok); err != nil {
			t.Errorf("%q: unexpected error %v", ok, err)
		}
	}
	for _, bad := range []string{".", "..", "a/b", `a\b`} {
		if err := validateTenantForFileName(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

// TestMigrateStateFiles: 旧名の journal / deferred / cursor を tenant 付きの名前へ copy し、
// 旧ファイルは .pre-tenant として残す。2 回目の起動では何もしない。
func TestMigrateStateFiles(t *testing.T) {
	dir := t.TempDir()
	cursorDir := t.TempDir()
	t.Setenv(cursorFileEnv, "")
	cfg := &config{Participant: "bot-" + filepath.Base(cursorDir), Tenant: "kaz", JournalDir: dir}
	oldCursor := cursorPath(cfg.Participant)
	t.Cleanup(func() {
		for _, p := range []string{oldCursor, oldCursor + preTenantSuffix, cursorPath(cfg.stateKey())} {
			os.Remove(p)
		}
	})
	oldJournal := filepath.Join(dir, cfg.Participant+".journal")
	oldDeferred := filepath.Join(dir, cfg.Participant+".deferred")
	writeFile(t, oldJournal, "journal\n", 0o600)
	writeFile(t, oldDeferred, "deferred\n", 0o600)
	writeFile(t, oldCursor, `{"last_processed_at":"2026-09-17T00:00:00.000Z"}`, 0o644)

	migrateStateFiles(cfg)

	checks := []struct{ old, new, content string }{
		{oldJournal, filepath.Join(dir, "kaz__"+cfg.Participant+".journal"), "journal\n"},
		{oldDeferred, filepath.Join(dir, "kaz__"+cfg.Participant+".deferred"), "deferred\n"},
		{oldCursor, cursorPath(cfg.stateKey()), `{"last_processed_at":"2026-09-17T00:00:00.000Z"}`},
	}
	for _, c := range checks {
		if got := readFile(t, c.new); got != c.content {
			t.Errorf("%s = %q; want %q", c.new, got, c.content)
		}
		if got := readFile(t, c.old+preTenantSuffix); got != c.content {
			t.Errorf("backup %s = %q; want %q", c.old+preTenantSuffix, got, c.content)
		}
		if _, err := os.Stat(c.old); !os.IsNotExist(err) {
			t.Errorf("legacy %s must not remain under its old name: %v", c.old, err)
		}
	}
	if info, err := os.Stat(checks[0].new); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("journal perm = %v, %v; want 0600", info, err)
	}
	if got := loadCursor(cfg.stateKey()); got != "2026-09-17T00:00:00.000Z" {
		t.Errorf("loadCursor after migration = %q", got)
	}

	// 移行後に新しい記録が進んでから旧名のファイルが再び現れても、上書きしない
	writeFile(t, checks[0].new, "newer\n", 0o600)
	writeFile(t, oldJournal, "stale\n", 0o600)
	migrateStateFiles(cfg)
	if got := readFile(t, checks[0].new); got != "newer\n" {
		t.Errorf("tenant-scoped journal overwritten: %q", got)
	}
	if got := readFile(t, oldJournal); got != "stale\n" {
		t.Errorf("legacy journal must be left untouched: %q", got)
	}
}

// TestMigrateStateFiles_NoTenant: tenant 未指定なら従来の名前のまま何もしない。
func TestMigrateStateFiles_NoTenant(t *testing.T) {
	dir := t.TempDir()
	cfg := &config{Participant: "bot", JournalDir: dir}
	old := filepath.Join(dir, "bot.journal")
	writeFile(t, old, "journal\n", 0o600)
	migrateStateFiles(cfg)
	if got := readFile(t, old); got != "journal\n" {
		t.Errorf("journal = %q", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("dir entries = %v; want only bot.journal", entries)
	}
}

// TestStateFiles_TenantsDoNotShare: 同名 participant でも tenant が違えば別ファイルになり、
// 一方の起動時 loadAndClear が他方の deferred 記録を消さない。
func TestStateFiles_TenantsDoNotShare(t *testing.T) {
	dir := t.TempDir()
	a := &config{Participant: "bot", Tenant: "a", JournalDir: dir}
	b := &config{Participant: "bot", Tenant: "b", JournalDir: dir}
	sa := newDeferredStore(a.JournalDir, a.stateKey())
	sb := newDeferredStore(b.JournalDir, b.stateKey())
	if sa.path == sb.path {
		t.Fatalf("same deferred path for different tenants: %s", sa.path)
	}
	writeFile(t, sa.path, `{"id":"x"}`+"\n", 0o600)
	if recs := sb.loadAndClear(); len(recs) != 0 {
		t.Errorf("tenant b read tenant a's records: %+v", recs)
	}
	if recs := sa.loadAndClear(); len(recs) != 1 {
		t.Errorf("tenant a records = %+v; want 1", recs)
	}
	if newJournal(dir, a.stateKey()).path == newJournal(dir, b.stateKey()).path {
		t.Error("same journal path for different tenants")
	}
	if cursorPath(a.stateKey()) == cursorPath(b.stateKey()) {
		t.Error("same cursor path for different tenants")
	}
}

// TestJournalDir_NoHome: HOME がなく AGENT_HUB_JOURNAL_DIR もなければエラー (issue #288 項目 4)。
func TestJournalDir_NoHome(t *testing.T) {
	t.Setenv(journalDirEnv, "")
	t.Setenv("HOME", "")
	if dir, err := journalDir(); err == nil {
		t.Fatalf("want error, got %q", dir)
	} else if !strings.Contains(err.Error(), journalDirEnv) {
		t.Errorf("error should mention %s: %v", journalDirEnv, err)
	}

	t.Setenv(journalDirEnv, "/x/journals")
	if dir, err := journalDir(); err != nil || dir != "/x/journals" {
		t.Errorf("env override: %q, %v", dir, err)
	}
	t.Setenv(journalDirEnv, "")
	t.Setenv("HOME", "/home/u")
	if dir, err := journalDir(); err != nil || dir != "/home/u/.agent-hub/journals" {
		t.Errorf("home: %q, %v", dir, err)
	}
}
