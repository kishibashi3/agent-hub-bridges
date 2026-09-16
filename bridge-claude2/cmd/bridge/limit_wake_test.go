package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
)

// ──────────────────────────────────────────────────────────────────────── //
// limit 休眠の wake 経路 / deferred 記録のテスト (issue #271 M2 / M4)       //
// ──────────────────────────────────────────────────────────────────────── //

func okResultLine() string {
	return `{"type":"result","subtype":"success","is_error":false,"result":"done"}`
}

func deferredMsgs() []agenthub.Message {
	return []agenthub.Message{
		{ID: "d1", Sender: "@a", To: "@limit-test", Body: "deferred-body", Timestamp: "2026-09-16T09:00:01.000Z"},
		{ID: "d2", Sender: "@b", To: "@limit-test", Body: "/ping", Timestamp: "2026-09-16T09:00:02.000Z"},
	}
}

func fileExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat %s: %v", path, err)
	}
	return false
}

// TestLimitSleeper_DeferredStore: enter で記録され、再休眠中は finishResume で消えず、
// 処理し終えたら消える。前回プロセスの記録は loadAndClear で読めてファイルが消える。
func TestLimitSleeper_DeferredStore(t *testing.T) {
	t.Setenv(journalDirEnv, t.TempDir())
	store := newDeferredStore("limit-test")
	s := &limitSleeper{store: store}
	until := time.Now().Add(time.Hour)

	s.enter(&limitReachedError{Kind: "spend limit", Until: until}, deferredMsgs())
	if !fileExists(t, store.path) {
		t.Fatal("deferred file must exist after enter")
	}

	// 復帰 → deferred 処理中に再 limit (残り 1 件で enter) → finishResume しても残る
	_ = s.wake()
	s.enter(&limitReachedError{Kind: "spend limit", Until: until}, deferredMsgs()[1:])
	s.finishResume()
	if !fileExists(t, store.path) {
		t.Fatal("deferred file must remain while sleeping again")
	}

	// 別プロセスとして読み直す: 再休眠時の残り 1 件だけが記録されている
	recs := newDeferredStore("limit-test").loadAndClear()
	if len(recs) != 1 || recs[0].ID != "d2" || recs[0].From != "@b" || recs[0].Kind != "spend limit" {
		t.Fatalf("records = %+v; want only d2", recs)
	}
	if recs[0].BodyPreview != "/ping" || recs[0].Until == "" || recs[0].DeferredAt == "" {
		t.Errorf("record fields = %+v", recs[0])
	}
	if fileExists(t, store.path) {
		t.Fatal("loadAndClear must remove the file")
	}

	// 正常復帰: 処理し終えたら消える
	s.enter(&limitReachedError{Kind: "session limit", Until: until}, nil)
	_ = s.wake()
	s.enter(&limitReachedError{Kind: "session limit", Until: until}, deferredMsgs())
	_ = s.wake()
	s.finishResume()
	if fileExists(t, store.path) {
		t.Fatal("deferred file must be removed after resume completes")
	}
}

// TestRunGracefulDrain_SleepingKeepsDeferredRecord: 休眠中の SIGTERM では記録を消さない
// (次回起動時に WARN を出すため)。
func TestRunGracefulDrain_SleepingKeepsDeferredRecord(t *testing.T) {
	t.Setenv(journalDirEnv, t.TempDir())
	store := newDeferredStore("limit-test")
	s := &limitSleeper{store: store}
	s.enter(&limitReachedError{Kind: "spend limit", Until: time.Now().Add(time.Hour)}, deferredMsgs())

	// 休眠中は client / runner に触れずに return する
	runGracefulDrain(nil, nil, &config{Participant: "limit-test"}, "", nil, nil, "@limit-test", s)

	if sleeping, _, _ := s.state(); sleeping {
		t.Error("drain must clear in-memory sleep state")
	}
	if recs := store.loadAndClear(); len(recs) != 2 {
		t.Fatalf("records after drain = %d; want 2 (kept for next startup WARN)", len(recs))
	}
}

// TestRunHubSession_LimitWake: 休眠中に session を張った場合から、復帰までの経路を検証する。
//   - 休眠中の (再) 接続は sleeping の display_name で register する
//   - 復帰時は通常の display_name に戻してから hub の未読を取りに行く
//   - deferred を hub の未読より先に処理する
//   - deferred 内の slash command は router が処理し、claude には流さない (PR #269 review M3)
//   - 処理し終えたら deferred の記録ファイルを消す
func TestRunHubSession_LimitWake(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "claude-prompts.log")
	script := writeRecordingFakeClaude(t, dir, logPath, okResultLine(), 0)
	_, cfg, runner, hub, journal := newTestEnv(t, script)
	cfg.AgentHubURL = hub.srv.URL
	cfg.GitHubPAT = "ghp_test"
	hub.setInbox(
		`[{"id":"u1","from":"@c","to":"@limit-test","message":"unread-body","timestamp":"2026-09-16T09:00:03.000Z"}]`,
	)

	sleeper := &limitSleeper{store: newDeferredStore(cfg.Participant)}
	sleeper.enter(&limitReachedError{Kind: "spend limit", Until: time.Now().Add(300 * time.Millisecond)}, deferredMsgs())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := runHubSession(ctx, cfg, "", runner, "", &activityTracker{}, &messageGapTracker{}, journal, sleeper)
		done <- err
	}()

	// hub 未読 (u1) が claude に渡るまで待つ
	deadline := time.Now().Add(15 * time.Second)
	for {
		data, _ := os.ReadFile(logPath)
		if strings.Contains(string(data), "unread-body") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("unread message was not processed; prompts log:\n%s", data)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("runHubSession err = %v; want context.Canceled", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("runHubSession did not return after cancel")
	}

	// register: sleeping 名 → 通常名。通常名の register は最初の get_messages より前
	hub.mu.Lock()
	calls := append([]toolCall(nil), hub.calls...)
	hub.mu.Unlock()
	var regNames []string
	wakeRegIdx, firstGetIdx := -1, -1
	for i, c := range calls {
		switch c.Name {
		case "register":
			dn, _ := c.Args["display_name"].(string)
			regNames = append(regNames, dn)
			if dn == cfg.DisplayName && wakeRegIdx < 0 {
				wakeRegIdx = i
			}
		case "get_messages":
			if firstGetIdx < 0 {
				firstGetIdx = i
			}
		}
	}
	if len(regNames) != 2 || !strings.Contains(regNames[0], "sleeping until") || regNames[1] != cfg.DisplayName {
		t.Fatalf("register display_names = %q; want [sleeping, %q]", regNames, cfg.DisplayName)
	}
	if wakeRegIdx < 0 || firstGetIdx < 0 || wakeRegIdx > firstGetIdx {
		t.Errorf("wake register (idx %d) must precede first get_messages (idx %d)", wakeRegIdx, firstGetIdx)
	}

	// claude への投入順: deferred-body → unread-body。/ping は claude に流れない
	data, _ := os.ReadFile(logPath)
	prompts := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(prompts) < 2 || !strings.Contains(prompts[0], "deferred-body") || !strings.Contains(prompts[1], "unread-body") {
		t.Fatalf("claude prompts order wrong:\n%s", data)
	}
	if strings.Contains(string(data), "/ping") {
		t.Errorf("slash command must not reach claude:\n%s", data)
	}

	// /ping は router が pong を返す
	var pong bool
	for _, c := range hub.callsNamed("send_message") {
		if to, _ := c.Args["to"].(string); to == "@b" && c.Args["message"] == "pong" {
			pong = true
		}
	}
	if !pong {
		t.Errorf("deferred /ping must be answered by router; send_message calls = %+v", hub.callsNamed("send_message"))
	}

	if fileExists(t, sleeper.store.path) {
		t.Error("deferred record must be removed after resume completes")
	}
}
