package main

import "testing"

// #419: tail -f 検出は tail と同じコマンドの中だけを見る。
func TestCheckBlockingCommandTailFollow(t *testing.T) {
	const want = "tail -f / --follow"
	blocked := []string{
		"tail -f x.log",
		"tail --follow x.log",
		"tail -F x.log",
		"tail -n 20 -f x.log",
		"tail -100f x.log",
		"tail -nF 5 x.log",
		"cat a; tail -f x.log",
		"tail -f x.log | grep ERROR",
		"tail -f x.log 2>&1",
	}
	for _, c := range blocked {
		if got := checkBlockingCommand(c); got != want {
			t.Errorf("checkBlockingCommand(%q) = %q, want %q", c, got, want)
		}
	}

	allowed := []string{
		// tail の後ろにある別コマンドのフラグ (issue の実例)
		"ls ~/.local/share/claude/versions 2>/dev/null | sort -V | tail -8; which claude; readlink -f $(which claude)",
		"tail -8 /tmp/x.log | cut -c1-400; pgrep -af i371",
		"python3 ledger.py status 2>&1 | tail -15; readlink -f x",
		"git log | tail -n 20; ls -F",
		"tail -n 20 x.log && readlink -f y",
		"tail -n 20 x.log || pgrep -af z",
		"tail -n 20 x.log & pgrep -af z",
		"tail -n 20 x.log\nreadlink -f y",
		"tail -n 20 x.log",
	}
	for _, c := range allowed {
		if got := checkBlockingCommand(c); got != "" {
			t.Errorf("checkBlockingCommand(%q) = %q, want \"\"", c, got)
		}
	}
}
