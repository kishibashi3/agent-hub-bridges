package main

import (
	"os"
	"testing"
	"time"
)

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
		flagVal int    // -1 = フラグ未指定
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
