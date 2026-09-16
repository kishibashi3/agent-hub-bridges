package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
)

// 実ログから採取した limit 系エラー文字列 (issue #267 / #268)。
const (
	errSessionLimit = "claude result error (subtype=success): You've hit your session limit · resets 3pm (Asia/Tokyo) (subprocess exit: exit status 1)"
	errSpendLimit   = "claude result error (subtype=success): You've hit your individual spend limit · run /usage-credits to ask your admin for a higher limit · your session limit resets 6:50pm (Asia/Tokyo) (subprocess exit: exit status 1)"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%s): %v", name, err)
	}
	return loc
}

// TestParseResetTime は "resets <time> (<tz>)" の parse と、当日/翌日/直前過ぎ の分岐を検証する。
func TestParseResetTime(t *testing.T) {
	tokyo := mustLoc(t, "Asia/Tokyo")
	// 2026-09-16 13:54 JST (実ログの発生時刻)
	now := time.Date(2026, 9, 16, 13, 54, 0, 0, tokyo)

	tests := []struct {
		name string
		text string
		now  time.Time
		want time.Time
		ok   bool
	}{
		{
			name: "3pm same day (+1m buffer)",
			text: errSessionLimit,
			now:  now,
			want: time.Date(2026, 9, 16, 15, 1, 0, 0, tokyo),
			ok:   true,
		},
		{
			name: "6:50pm with minutes",
			text: errSpendLimit,
			now:  now,
			want: time.Date(2026, 9, 16, 18, 51, 0, 0, tokyo),
			ok:   true,
		},
		{
			name: "1:50am → next day when now is 13:54",
			text: "You've hit your session limit · resets 1:50am (Asia/Tokyo)",
			now:  now,
			want: time.Date(2026, 9, 17, 1, 51, 0, 0, tokyo),
			ok:   true,
		},
		{
			name: "12am / 12pm edge",
			text: "resets 12pm (Asia/Tokyo)",
			now:  time.Date(2026, 9, 16, 9, 0, 0, 0, tokyo),
			want: time.Date(2026, 9, 16, 12, 1, 0, 0, tokyo),
			ok:   true,
		},
		{
			name: "reset just passed (<1h ago) → fallback 30m, not next day",
			text: "resets 3pm (Asia/Tokyo)",
			now:  time.Date(2026, 9, 16, 15, 0, 30, 0, tokyo),
			want: time.Date(2026, 9, 16, 15, 30, 30, 0, tokyo),
			ok:   true,
		},
		{
			name: "no tz → interpreted in now's location",
			text: "resets 9pm",
			now:  now,
			want: time.Date(2026, 9, 16, 21, 1, 0, 0, tokyo),
			ok:   true,
		},
		{
			name: "unknown tz → falls back to now's location",
			text: "resets 9pm (Mars/Olympus)",
			now:  now,
			want: time.Date(2026, 9, 16, 21, 1, 0, 0, tokyo),
			ok:   true,
		},
		{
			name: "no reset time",
			text: "You've hit your session limit",
			now:  now,
			ok:   false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseResetTime(tc.text, tc.now)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (got %v)", ok, tc.ok, got)
			}
			if ok && !got.Equal(tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDetectLimit は limit 系エラーの判定・種別・fallback を検証する (issue #268 提案 1, 2)。
func TestDetectLimit(t *testing.T) {
	tokyo := mustLoc(t, "Asia/Tokyo")
	now := time.Date(2026, 9, 16, 13, 54, 0, 0, tokyo)

	t.Run("session limit with reset time", func(t *testing.T) {
		lim := detectLimit(errors.New(errSessionLimit), now)
		if lim == nil {
			t.Fatal("expected limit error")
		}
		if lim.Kind != "session limit" || !lim.Parsed {
			t.Errorf("kind=%q parsed=%v", lim.Kind, lim.Parsed)
		}
		if want := time.Date(2026, 9, 16, 15, 1, 0, 0, tokyo); !lim.Until.Equal(want) {
			t.Errorf("until=%v want %v", lim.Until, want)
		}
	})
	t.Run("spend limit takes precedence over session limit wording", func(t *testing.T) {
		lim := detectLimit(errors.New(errSpendLimit), now)
		if lim == nil || lim.Kind != "spend limit" {
			t.Fatalf("got %+v", lim)
		}
	})
	t.Run("limit without parseable reset → 30m fallback", func(t *testing.T) {
		lim := detectLimit(errors.New("You've hit your spend limit · resets soon"), now)
		if lim == nil || lim.Parsed {
			t.Fatalf("got %+v", lim)
		}
		if want := now.Add(limitSleepFallback); !lim.Until.Equal(want) {
			t.Errorf("until=%v want %v", lim.Until, want)
		}
	})
	t.Run("'resets <time>' without kind keyword → generic limit", func(t *testing.T) {
		lim := detectLimit(errors.New("usage cap · resets 9pm (Asia/Tokyo)"), now)
		if lim == nil || lim.Kind != "limit" || !lim.Parsed {
			t.Fatalf("got %+v", lim)
		}
	})
	t.Run("non-limit errors are not limits", func(t *testing.T) {
		for _, msg := range []string{
			"claude subprocess exited without result event (EOF — crash or premature exit)",
			"claude subprocess killed (SubprocessTimeout=30m0s): subprocess timeout",
			"claude result error (subtype=error): Something went wrong",
		} {
			if lim := detectLimit(errors.New(msg), now); lim != nil {
				t.Errorf("%q: unexpected limit %+v", msg, lim)
			}
		}
		if detectLimit(nil, now) != nil {
			t.Error("nil error should not be a limit")
		}
	})
	t.Run("wrapped error is detectable with errors.As", func(t *testing.T) {
		lim := detectLimit(errors.New(errSessionLimit), now)
		wrapped := fmt.Errorf("outer: %w", lim)
		if asLimitError(wrapped) == nil {
			t.Error("asLimitError should unwrap")
		}
		if asLimitError(errors.New("plain")) != nil {
			t.Error("plain error is not a limit")
		}
	})
}

// TestSleepingDisplayName は display_name に休眠状態と reset 時刻が出ることを検証する (提案 1)。
func TestSleepingDisplayName(t *testing.T) {
	tokyo := mustLoc(t, "Asia/Tokyo")
	until := time.Date(2026, 9, 16, 18, 51, 0, 0, tokyo)
	got := sleepingDisplayName("planner — go bridge", until, "spend limit")
	want := "planner — go bridge (sleeping until 18:51 JST: spend limit)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestIsAutoErrorEcho は `(auto) bridge-claude2 error:` で始まる inbound の判定を検証する (提案 3)。
// autoErrorPrefix は handleOne が実際に送る auto 返信の先頭と同一定数であること。
func TestIsAutoErrorEcho(t *testing.T) {
	outgoing := fmt.Sprintf("%s %v", autoErrorPrefix, errors.New(errSessionLimit))
	if !strings.HasPrefix(outgoing, "(auto) bridge-claude2 error:") {
		t.Fatalf("auto reply format drifted: %q", outgoing)
	}
	if !isAutoErrorEcho(outgoing) {
		t.Error("own auto reply must be detected as echo")
	}
	if isAutoErrorEcho("(auto) bridge-claude2 error") {
		t.Error("prefix without colon is not an auto error")
	}
	if isAutoErrorEcho("please review PR #268") {
		t.Error("normal message must not be treated as echo")
	}
	if isAutoErrorEcho("  (auto) bridge-claude2 error: x") {
		t.Error("prefix must be at body start")
	}
}

// TestLimitSleeper は休眠状態の enter/state/wake と deferred の受け渡しを検証する。
func TestLimitSleeper(t *testing.T) {
	s := &limitSleeper{}
	if sleeping, _, _ := s.state(); sleeping {
		t.Fatal("fresh sleeper must not be sleeping")
	}
	until := time.Now().Add(time.Hour)
	s.enter(&limitReachedError{Kind: "spend limit", Until: until},
		[]agenthub.Message{{ID: "m1"}, {ID: "m2"}})
	sleeping, gotUntil, kind := s.state()
	if !sleeping || !gotUntil.Equal(until) || kind != "spend limit" {
		t.Fatalf("state = %v %v %q", sleeping, gotUntil, kind)
	}
	// 休眠中に再度 limit → deferred は追記される
	s.enter(&limitReachedError{Kind: "session limit", Until: until.Add(time.Minute)},
		[]agenthub.Message{{ID: "m3"}})
	if s.deferredCount() != 3 {
		t.Errorf("deferredCount = %d, want 3", s.deferredCount())
	}
	d := s.wake()
	if len(d) != 3 || d[0].ID != "m1" || d[2].ID != "m3" {
		t.Errorf("wake returned %+v", d)
	}
	if sleeping, _, _ := s.state(); sleeping {
		t.Error("after wake must not be sleeping")
	}
	if s.deferredCount() != 0 {
		t.Error("deferred must be cleared after wake")
	}
}
