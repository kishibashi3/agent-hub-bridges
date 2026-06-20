package main

import (
	"os"
	"strings"
	"testing"
	"time"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
)

// TestResolveInboxPollInterval は safety-net poll 間隔の解決優先順位を検証する (issue #234)。
// AGENT_HUB_INBOX_POLL_INTERVAL_S env → 30s デフォルト
func TestResolveInboxPollInterval(t *testing.T) {
	tests := []struct {
		name    string
		envVal  string
		wantD   time.Duration
		wantLog bool // 不正値 → デフォルト + warn ログ
	}{
		{
			name:   "env=unset → default 30s",
			envVal: "",
			wantD:  30 * time.Second,
		},
		{
			name:   "env=60 → 60s",
			envVal: "60",
			wantD:  60 * time.Second,
		},
		{
			name:   "env=15.5 → 15.5s",
			envVal: "15.5",
			wantD:  time.Duration(15.5 * float64(time.Second)),
		},
		{
			name:    "env=invalid → default 30s (warn)",
			envVal:  "notanumber",
			wantD:   30 * time.Second,
			wantLog: true,
		},
		{
			name:    "env=0 → default 30s (warn: non-positive)",
			envVal:  "0",
			wantD:   30 * time.Second,
			wantLog: true,
		},
		{
			name:    "env=-5 → default 30s (warn: non-positive)",
			envVal:  "-5",
			wantD:   30 * time.Second,
			wantLog: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envVal != "" {
				t.Setenv(inboxPollIntervalEnv, tc.envVal)
			} else {
				os.Unsetenv(inboxPollIntervalEnv)
			}

			got := resolveInboxPollInterval()
			if got != tc.wantD {
				t.Errorf("got %v, want %v", got, tc.wantD)
			}
		})
	}
}

// TestParseConfig_SubprocessTimeoutDefaults は subprocess-timeout の解決優先順位を検証する。
// --subprocess-timeout フラグ (-1=未指定) → AGENT_HUB_SUBPROCESS_TIMEOUT env → 30m デフォルト (issue #226)
func TestParseConfig_SubprocessTimeoutDefaults(t *testing.T) {
	// parseConfig は flag.Parse() に依存するため直接呼べないが、
	// 解決ロジックだけを抽出して検証するヘルパーを使う。
	tests := []struct {
		name    string
		flagVal time.Duration // -1 = フラグ未指定 (デフォルト動作)
		envVal  string
		wantD   time.Duration
		wantErr bool
	}{
		{
			name:    "flag=-1 env=unset → default 30m",
			flagVal: -1,
			envVal:  "",
			wantD:   30 * time.Minute,
		},
		{
			name:    "flag=-1 env=1h → 1h",
			flagVal: -1,
			envVal:  "1h",
			wantD:   time.Hour,
		},
		{
			name:    "flag=-1 env=0 → 0 (no timeout)",
			flagVal: -1,
			envVal:  "0",
			wantD:   0,
		},
		{
			name:    "flag=0 env=1h → flag wins (0 = no timeout)",
			flagVal: 0,
			envVal:  "1h",
			wantD:   0,
		},
		{
			name:    "flag=5m env=1h → flag wins (5m)",
			flagVal: 5 * time.Minute,
			envVal:  "1h",
			wantD:   5 * time.Minute,
		},
		{
			name:    "flag=-1 env=invalid → error",
			flagVal: -1,
			envVal:  "notaduration",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envVal != "" {
				t.Setenv("AGENT_HUB_SUBPROCESS_TIMEOUT", tc.envVal)
			} else {
				os.Unsetenv("AGENT_HUB_SUBPROCESS_TIMEOUT")
			}

			got, err := resolveSubprocessTimeout(tc.flagVal)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantD {
				t.Errorf("got %v, want %v", got, tc.wantD)
			}
		})
	}
}

// TestParseConfig_MaxQueryRetriesDefaults は max-query-retries の解決優先順位を検証する。
func TestParseConfig_MaxQueryRetriesDefaults(t *testing.T) {
	tests := []struct {
		name    string
		flagVal int // -1 = フラグ未指定
		envVal  string
		wantN   int
		wantErr bool
	}{
		{
			name:    "flag=-1 env=unset → default 2",
			flagVal: -1,
			envVal:  "",
			wantN:   2,
		},
		{
			name:    "flag=-1 env=5 → 5",
			flagVal: -1,
			envVal:  "5",
			wantN:   5,
		},
		{
			name:    "flag=-1 env=0 → 0 (no retry)",
			flagVal: -1,
			envVal:  "0",
			wantN:   0,
		},
		{
			name:    "flag=3 env=5 → flag wins (3)",
			flagVal: 3,
			envVal:  "5",
			wantN:   3,
		},
		{
			name:    "flag=-1 env=invalid → error",
			flagVal: -1,
			envVal:  "notanint",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envVal != "" {
				t.Setenv("AGENT_HUB_MAX_QUERY_RETRIES", tc.envVal)
			} else {
				os.Unsetenv("AGENT_HUB_MAX_QUERY_RETRIES")
			}

			got, err := resolveMaxQueryRetries(tc.flagVal)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantN {
				t.Errorf("got %d, want %d", got, tc.wantN)
			}
		})
	}
}

// TestBuildGitHubFooter は footer が真値から正しく組成され、欠けた値は
// 「省略」に倒れる (嘘を出さない) ことを検証する (issue #245)。
func TestBuildGitHubFooter(t *testing.T) {
	const bt = "bridge-claude2"
	tests := []struct {
		name    string
		handle  string
		model   string
		ghLogin string
		want    string
	}{
		{
			name:    "all present",
			handle:  "ntv-reviewer",
			model:   "opus-4.8",
			ghLogin: "kishibashi3",
			want:    "@ntv-reviewer [bridge-claude2 · opus-4.8] (operator-supervised · kishibashi3/agent-hub)",
		},
		{
			name:    "model omitted when empty",
			handle:  "ntv-reviewer",
			model:   "",
			ghLogin: "kishibashi3",
			want:    "@ntv-reviewer [bridge-claude2] (operator-supervised · kishibashi3/agent-hub)",
		},
		{
			name:    "gh-login omitted when empty",
			handle:  "ntv-reviewer",
			model:   "opus-4.8",
			ghLogin: "",
			want:    "@ntv-reviewer [bridge-claude2 · opus-4.8] (operator-supervised · agent-hub)",
		},
		{
			name:    "both model and gh-login omitted",
			handle:  "ntv-reviewer",
			model:   "",
			ghLogin: "",
			want:    "@ntv-reviewer [bridge-claude2] (operator-supervised · agent-hub)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildGitHubFooter(tc.handle, bt, tc.model, tc.ghLogin); got != tc.want {
				t.Errorf("buildGitHubFooter() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFormatPrompt_FooterInjection は footer が非空なら指示が注入され、
// 空なら base prompt のみになることを検証する (issue #245)。
func TestFormatPrompt_FooterInjection(t *testing.T) {
	msg := agenthub.Message{
		ID:     "msg-1",
		Sender: "@alice",
		To:     "@bob",
		Body:   "hello",
	}
	footer := "@bob [bridge-claude2 · opus-4.8] (operator-supervised · kishibashi3/agent-hub)"

	withFooter := formatPrompt("@bob", msg, footer)
	if !strings.Contains(withFooter, footer) {
		t.Errorf("formatPrompt() should contain footer literal %q", footer)
	}
	if !strings.Contains(withFooter, "GitHub 投稿ルール") {
		t.Error("formatPrompt() should contain the footer instruction header")
	}

	withoutFooter := formatPrompt("@bob", msg, "")
	if strings.Contains(withoutFooter, "GitHub 投稿ルール") {
		t.Error("formatPrompt() with empty footer should not inject the footer instruction")
	}
}
