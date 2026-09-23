package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
)

// ──────────────────────────────────────────────────────────────────────── //
// cursor same-ms を worker loop 経由で検証する (issue #329)               //
//                                                                          //
// - mock hub (httptest) が get_messages を順に返す                          //
// - fake codex (shell script) が受け取った本文を log に追記する             //
// - runHubSession を startup catchup / polling loop / graceful drain の    //
//   3 経路それぞれで回し、同じ ms の 3 通が全件 dispatch されることと、     //
//   保存された cursor にその ID が全部入っていることを確かめる              //
// ──────────────────────────────────────────────────────────────────────── //

// mockHub は MCP initialize + tools/call を最低限処理する。
type mockHub struct {
	srv *httptest.Server
	mu  sync.Mutex
	// inbox は get_messages が順に返す JSON 配列テキスト。尽きたら "[]" を返す。
	inbox []string
}

func newMockHub(t *testing.T, inbox ...string) *mockHub {
	t.Helper()
	h := &mockHub{inbox: inbox}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("mcp-session-id", "test-session")
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound) // SSE は使わない
			return
		}
		var req struct {
			Method string `json:"method"`
			ID     *int64 `json:"id"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		id := int64(0)
		if req.ID != nil {
			id = *req.ID
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"mock","version":"0"}}}`, id)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			text := "ok"
			if req.Params.Name == "get_messages" {
				h.mu.Lock()
				text = "[]"
				if len(h.inbox) > 0 {
					text, h.inbox = h.inbox[0], h.inbox[1:]
				}
				h.mu.Unlock()
			}
			quoted, _ := json.Marshal(text)
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":%s}],"isError":false}}`, id, quoted)
		default:
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{}}`, id)
		}
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// writeRecordingFakeCodex は stdin の prompt から `body <id>` を取り出して logPath に追記し、
// turn.completed を 1 行出して終了する codex CLI 代替を書く。
func writeRecordingFakeCodex(t *testing.T, dir, logPath string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-codex")
	script := fmt.Sprintf("#!/bin/sh\ngrep -o 'body [a-z0-9]*' | head -n 1 >> '%s'\necho '{\"type\":\"turn.completed\"}'\n", logPath)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func inboxJSON(t *testing.T, msgs ...agenthub.Message) string {
	t.Helper()
	data, err := json.Marshal(msgs)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func readDispatched(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(strings.ReplaceAll(string(data), "body ", ""))
}

// TestRunHubSession_SameMillisecondAllDispatched は、同じ ms の 3 通が startup catchup /
// polling loop / graceful drain のどの経路でも全件 dispatch され、保存された cursor が
// {TS, IDs:[m1 m2 m3]} になることを検証する (issue #329)。
func TestRunHubSession_SameMillisecondAllDispatched(t *testing.T) {
	const ts = "2026-09-16T09:00:01.000Z"
	// drain は入口の cursor で 1 度だけ seen を判定する。drain の経路では polling loop で m1 を
	// 処理している間に ctx を cancel し、cursor が「m1 まで処理済み」の状態で drain に入れる。
	// hub は m1・m2・m3 を返し、m1 は skip、m2・m3 は dispatch されるはず。
	m1Only := inboxJSON(t, msgAt("m1", ts))

	cases := []struct {
		name string
		// inbox は get_messages の返す順。1 番目は startup catchup、2 番目以降は polling loop。
		// ctx を cancel した後の 1 回は drain が取る。
		inbox func(batch string) []string
		// cancelAfter の件数が dispatch されたら ctx を cancel する
		cancelAfter  int
		wantDispatch []string
	}{
		{
			name:         "startup catchup",
			inbox:        func(batch string) []string { return []string{batch} },
			cancelAfter:  3,
			wantDispatch: []string{"m1", "m2", "m3"},
		},
		{
			name:         "polling loop",
			inbox:        func(batch string) []string { return []string{"[]", batch} },
			cancelAfter:  3,
			wantDispatch: []string{"m1", "m2", "m3"},
		},
		{
			name: "graceful drain",
			inbox: func(batch string) []string {
				return []string{"[]", m1Only, batch}
			},
			cancelAfter:  1,
			wantDispatch: []string{"m1", "m2", "m3"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv(cursorFileEnv, filepath.Join(dir, "cursor.json"))
			t.Setenv(journalDirEnv, filepath.Join(dir, "journals"))
			logPath := filepath.Join(dir, "dispatched.log")

			batch := inboxJSON(t, msgAt("m1", ts), msgAt("m2", ts), msgAt("m3", ts))
			hub := newMockHub(t, tc.inbox(batch)...)
			cfg := &config{
				User:              "codex-test",
				AgentHubURL:       hub.srv.URL,
				GitHubPAT:         "ghp_test",
				Mode:              "stateful",
				Workdir:           dir,
				CodexCLI:          writeRecordingFakeCodex(t, dir, logPath),
				CodexHomeDir:      dir,
				PollInterval:      time.Hour, // 次の get_messages は ctx cancel (drain) まで起こさない
				SubprocessTimeout: 10 * time.Second,
				ScannerBufferSize: 64 * 1024,
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _, _ = runHubSession(ctx, cfg, newCodexRunner(cfg), cursorPos{},
					&activityTracker{}, &messageGapTracker{}, newJournal(cfg.User))
			}()

			// cancelAfter 件の dispatch を待ってから cancel する。skip されて届かない場合も
			// deadline で cancel し、下の assert で落とす。
			deadline := time.Now().Add(5 * time.Second)
			for len(readDispatched(t, logPath)) < tc.cancelAfter && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			cancel()

			select {
			case <-done:
			case <-time.After(15 * time.Second):
				t.Fatal("runHubSession did not return after ctx cancel")
			}

			if got := readDispatched(t, logPath); !reflect.DeepEqual(got, tc.wantDispatch) {
				t.Errorf("dispatched = %v, want %v (same-ms messages must not be skipped)", got, tc.wantDispatch)
			}
			// runGracefulDrain は cursor を返さないので、保存された cursor で見る
			want := cursorPos{TS: ts, IDs: []string{"m1", "m2", "m3"}}
			if saved := loadCursor(cfg.User); !reflect.DeepEqual(saved, want) {
				t.Errorf("saved cursor = %+v, want %+v", saved, want)
			}
		})
	}
}
