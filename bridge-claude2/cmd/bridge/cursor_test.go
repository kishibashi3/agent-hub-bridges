package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
)

func msgAt(id, ts string) agenthub.Message {
	return agenthub.Message{ID: id, Sender: "@a", To: "@limit-test", Body: "body " + id, Timestamp: ts}
}

// TestCursorPos_SameMillisecond は、同じ ms のメッセージが「処理済みの ID だけ」seen になることを
// 検証する (issue #322)。
func TestCursorPos_SameMillisecond(t *testing.T) {
	const ts = "2026-09-16T09:00:01.000Z"
	c := cursorPos{}.advance(msgAt("m1", ts))

	if !c.seen(msgAt("m1", ts)) {
		t.Error("processed m1 must be seen")
	}
	if c.seen(msgAt("m2", ts)) {
		t.Error("m2 has the same timestamp but was not processed; must not be seen")
	}
	if !c.seen(msgAt("m0", "2026-09-16T09:00:00.999Z")) {
		t.Error("older message must be seen")
	}
	if c.seen(msgAt("m3", "2026-09-16T09:00:01.001Z")) {
		t.Error("newer message must not be seen")
	}

	c2 := c.advance(msgAt("m2", ts))
	if !reflect.DeepEqual(c2.IDs, []string{"m1", "m2"}) || c2.TS != ts {
		t.Errorf("advance same ts = %+v", c2)
	}
	if !reflect.DeepEqual(c.IDs, []string{"m1"}) {
		t.Errorf("advance must not modify the original cursor: %+v", c)
	}

	c3 := c2.advance(msgAt("m3", "2026-09-16T09:00:01.001Z"))
	if !reflect.DeepEqual(c3, cursorPos{TS: "2026-09-16T09:00:01.001Z", IDs: []string{"m3"}}) {
		t.Errorf("advance newer ts = %+v", c3)
	}
	if got := c3.advance(msgAt("old", ts)); !reflect.DeepEqual(got, c3) {
		t.Errorf("advance must not move the cursor back: %+v", got)
	}
}

func TestCursorPos_ZeroSeesNothing(t *testing.T) {
	if (cursorPos{}).seen(msgAt("m1", "2026-09-16T09:00:01.000Z")) {
		t.Error("zero cursor must not see anything")
	}
}

// TestCursor_SaveLoadRoundTrip は ID 付きで保存・復元できることを検証する。
func TestCursor_SaveLoadRoundTrip(t *testing.T) {
	t.Setenv(cursorFileEnv, filepath.Join(t.TempDir(), "cursor.json"))
	want := cursorPos{TS: "2026-09-16T09:00:01.000Z", IDs: []string{"m1", "m2"}}
	saveCursor("k", want)
	if got := loadCursor("k"); !reflect.DeepEqual(got, want) {
		t.Errorf("loadCursor = %+v, want %+v", got, want)
	}
}

// TestCursor_LoadLegacyFile は ID を持たない旧形式のファイルを読めること、同じ timestamp の
// メッセージを捨てないことを検証する。
func TestCursor_LoadLegacyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor.json")
	t.Setenv(cursorFileEnv, path)
	if err := os.WriteFile(path, []byte(`{"last_processed_at":"2026-09-16T09:00:01.000Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c := loadCursor("k")
	if c.TS != "2026-09-16T09:00:01.000Z" || len(c.IDs) != 0 {
		t.Fatalf("loadCursor legacy = %+v", c)
	}
	if !c.seen(msgAt("old", "2026-09-16T09:00:00.000Z")) {
		t.Error("older message must still be seen with a legacy cursor")
	}
	if c.seen(msgAt("m2", "2026-09-16T09:00:01.000Z")) {
		t.Error("same-ts message must not be dropped with a legacy cursor")
	}
}

// TestProcessMessages_SameMillisecondAllDispatched は、同じ ms のメッセージが全件 dispatch され、
// 再起動 (cursor の読み直し) 後は同じメッセージを再 dispatch しないことを検証する (issue #322)。
func TestProcessMessages_SameMillisecondAllDispatched(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "dispatched.log")
	script := writeRecordingFakeClaude(t, dir, logPath, okResultLine(), 0)
	client, cfg, runner, _, journal := newTestEnv(t, script)

	const ts = "2026-09-16T09:00:01.000Z"
	msgs := []agenthub.Message{msgAt("m1", ts), msgAt("m2", ts), msgAt("m3", ts)}

	cursor := processMessages(context.Background(), cfg, client, runner, nil, cursorPos{},
		&activityTracker{}, &messageGapTracker{}, journal, &limitSleeper{}, msgs, "test")

	if n := countLines(t, logPath); n != 3 {
		t.Fatalf("dispatched %d messages; want 3 (same-ms messages must not be skipped)", n)
	}
	if want := (cursorPos{TS: ts, IDs: []string{"m1", "m2", "m3"}}); !reflect.DeepEqual(cursor, want) {
		t.Errorf("cursor = %+v, want %+v", cursor, want)
	}

	// 再起動相当: 保存された cursor を読み直し、同じ 3 件 + 同じ ms の新着 1 件を流す
	reloaded := loadCursor(cfg.stateKey())
	if !reflect.DeepEqual(reloaded, cursor) {
		t.Fatalf("reloaded cursor = %+v, want %+v", reloaded, cursor)
	}
	again := append(append([]agenthub.Message{}, msgs...), msgAt("m4", ts))
	processMessages(context.Background(), cfg, client, runner, nil, reloaded,
		&activityTracker{}, &messageGapTracker{}, journal, &limitSleeper{}, again, "test")

	if n := countLines(t, logPath); n != 4 {
		t.Errorf("dispatched %d messages in total; want 4 (m1-m3 skipped, m4 dispatched)", n)
	}
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

// TestRunGracefulDrain_SameMillisecondAllDispatched は、drain 中に同じ ms のメッセージが
// 複数届いたとき、処理済みでないものが全件 dispatch され、保存された cursor にその ID が
// 全部入ることを検証する (issue #326)。
func TestRunGracefulDrain_SameMillisecondAllDispatched(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "dispatched.log")
	script := writeRecordingFakeClaude(t, dir, logPath, okResultLine(), 0)
	client, cfg, runner, hub, journal := newTestEnv(t, script)

	// m1 は処理済み。m2 / m3 は m1 と同じ ms に届いた未処理のメッセージ
	const ts = "2026-09-16T09:00:01.000Z"
	cursor := cursorPos{TS: ts, IDs: []string{"m1"}}
	hub.setInbox(`[` +
		`{"id":"m1","from":"@a","to":"@limit-test","message":"body m1","timestamp":"` + ts + `"},` +
		`{"id":"m2","from":"@a","to":"@limit-test","message":"body m2","timestamp":"` + ts + `"},` +
		`{"id":"m3","from":"@a","to":"@limit-test","message":"body m3","timestamp":"` + ts + `"}]`)

	runGracefulDrain(client, runner, cfg, cursor, &activityTracker{}, journal, "@limit-test", &limitSleeper{})

	// 1 行目は /compact。その後に dispatch されたメッセージが並ぶ
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	for _, id := range []string{"m2", "m3"} {
		if !strings.Contains(log, "body "+id) {
			t.Errorf("%s was not dispatched during drain (same-ms messages must not be skipped)", id)
		}
	}
	if strings.Contains(log, "body m1") {
		t.Error("m1 is already processed; must not be dispatched again")
	}
	want := cursorPos{TS: ts, IDs: []string{"m1", "m2", "m3"}}
	if got := loadCursor(cfg.stateKey()); !reflect.DeepEqual(got, want) {
		t.Errorf("saved cursor = %+v, want %+v", got, want)
	}
}

// TestSaveCursor_FileMode は、cursor ファイルが 0o600 で作られ、以前の版が 0o644 で
// 作った既存ファイルも 0o600 に直ることを検証する (issue #326)。
func TestSaveCursor_FileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor")
	t.Setenv("AGENT_HUB_CURSOR_FILE", path)
	c := cursorPos{TS: "2026-09-16T09:00:01.000Z", IDs: []string{"m1"}}

	saveCursor("key", c)
	assertMode(t, path, 0o600)

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	saveCursor("key", c)
	assertMode(t, path, 0o600)
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("mode of %s = %o, want %o", path, got, want)
	}
}
