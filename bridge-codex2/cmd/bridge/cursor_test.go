package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
)

func msgAt(id, ts string) agenthub.Message {
	return agenthub.Message{ID: id, Sender: "@a", To: "@codex-test", Body: "body " + id, Timestamp: ts}
}

// TestCursorPos_SameMillisecond は、同じ ms のメッセージが「処理済みの ID だけ」seen になることを
// 検証する (issue #323)。
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

// TestCursor_SaveLoadRoundTrip は ID 付きで保存・復元できること、同じ ms の N 通を処理した
// cursor を読み直すと N 通とも seen になり、同じ ms の新着は seen にならないことを検証する。
func TestCursor_SaveLoadRoundTrip(t *testing.T) {
	t.Setenv(cursorFileEnv, filepath.Join(t.TempDir(), "cursor.json"))
	const ts = "2026-09-16T09:00:01.000Z"
	c := cursorPos{}
	for _, id := range []string{"m1", "m2", "m3"} {
		c = c.advance(msgAt(id, ts))
		saveCursor("u", c)
	}

	got := loadCursor("u")
	if want := (cursorPos{TS: ts, IDs: []string{"m1", "m2", "m3"}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("loadCursor = %+v, want %+v", got, want)
	}
	for _, id := range []string{"m1", "m2", "m3"} {
		if !got.seen(msgAt(id, ts)) {
			t.Errorf("%s was processed before restart; must be seen", id)
		}
	}
	if got.seen(msgAt("m4", ts)) {
		t.Error("m4 has the same timestamp but was not processed; must not be seen")
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
	c := loadCursor("u")
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
