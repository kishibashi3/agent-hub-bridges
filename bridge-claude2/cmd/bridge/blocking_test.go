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

// #426: detach に渡した引数は判定しない。detach の後ろに続けたコマンドと、
// 読み切れない形は今までどおり判定する。
func TestCheckBlockingCommandDetach(t *testing.T) {
	exe := bridgeSelfExe()
	if exe == "" {
		t.Fatal("os.Executable() failed")
	}
	const (
		sleepPat = "sleep <N>=60s+"
		bin      = `"$AGENT_HUB_BRIDGE_BIN"`
		w        = "sleep 70; date"
	)

	allowed := []string{
		// 試験で haiku が実際に呼んだ形 (#426 の bridge ログ)
		`"$AGENT_HUB_BRIDGE_BIN" detach -- 'sleep 70; date -u'`,
		`$AGENT_HUB_BRIDGE_BIN detach -- 'sleep 120 && date -u'`,
		`${AGENT_HUB_BRIDGE_BIN} detach -- 'sleep 70; date -u'`,
		`"${AGENT_HUB_BRIDGE_BIN}" detach -- 'sleep 70; date -u'`,
		exe + ` detach -- 'sleep 70; date -u'`,
		`"` + exe + `" detach -- 'sleep 70; date -u'`,
		`  "$AGENT_HUB_BRIDGE_BIN"  detach  --  'sleep 1h; date -u'`,
		`"$AGENT_HUB_BRIDGE_BIN" detach -- 'sleep infinity'`,
		`"$AGENT_HUB_BRIDGE_BIN" detach -- 'gh run watch 123; tail -f x.log'`,
		`"$AGENT_HUB_BRIDGE_BIN" detach --name wait70 -- 'sleep 70; date -u'`,
		`"$AGENT_HUB_BRIDGE_BIN" detach --name=wait70 -- 'sleep 70; date -u'`,
		`"$AGENT_HUB_BRIDGE_BIN" detach -name 'wait 70' -- 'sleep 70; date -u'`,
		// argv 形式 (引数 2 つ以上) と二重引用符
		`"$AGENT_HUB_BRIDGE_BIN" detach -- sleep 70`,
		`"$AGENT_HUB_BRIDGE_BIN" detach -- "sleep 70; date -u"`,
		`"$AGENT_HUB_BRIDGE_BIN" detach -- bash -c 'sleep 70; date -u'`,
		// 後ろに続けたコマンドに待ちが無い
		`"$AGENT_HUB_BRIDGE_BIN" detach -- 'sleep 70; date -u'; echo started`,
		`"$AGENT_HUB_BRIDGE_BIN" detach -- 'sleep 70; date -u' && echo started`,
		`"$AGENT_HUB_BRIDGE_BIN" detach -- 'sleep 70' 2>&1`,
		`"$AGENT_HUB_BRIDGE_BIN" detach -- 'sleep 70; date -u'` + "\n" + `echo started`,
		// 後ろに続けた 2 つ目も detach
		`"$AGENT_HUB_BRIDGE_BIN" detach -- 'sleep 70' && "$AGENT_HUB_BRIDGE_BIN" detach -- 'sleep 80'`,
	}
	for _, c := range allowed {
		if got := checkBlockingCommand(c); got != "" {
			t.Errorf("checkBlockingCommand(%q) = %q, want \"\"", c, got)
		}
	}

	blocked := []string{
		// detach の後ろに続けた待ち
		`"$AGENT_HUB_BRIDGE_BIN" detach -- 'sleep 70'; sleep 120`,
		`"$AGENT_HUB_BRIDGE_BIN" detach -- 'date -u' && sleep 120`,
		`"$AGENT_HUB_BRIDGE_BIN" detach -- 'date -u' || sleep 2m`,
		`"$AGENT_HUB_BRIDGE_BIN" detach -- 'date -u' & sleep 70`,
		`"$AGENT_HUB_BRIDGE_BIN" detach -- 'date -u' | sleep 70`,
		`"$AGENT_HUB_BRIDGE_BIN" detach -- 'date -u'` + "\n" + `sleep 70`,
		`"$AGENT_HUB_BRIDGE_BIN" detach -- 'date -u' > /dev/null; sleep 70`,
		`"$AGENT_HUB_BRIDGE_BIN" detach -- 'a' && "$AGENT_HUB_BRIDGE_BIN" detach -- 'b'; sleep 70`,
		// detach を使わない待ち
		`sleep 70`,
		`sleep 70; date -u`,
		`date -u; sleep 120`,
		// 読み切れない形 (コマンド全体を判定する)。
		// 注: 今の sleep の正規表現は秒数の直後に ' や ) が来ると一致しない (#426 の前からの
		// 挙動)。ここでは D1 の判定だけを見るため、秒数の後ろに "; date" を置いている。
		bin + ` detach -- '` + w,                         // 閉じていない単一引用符
		bin + ` detach -- "` + w,                         // 閉じていない二重引用符
		bin + ` detach -- "$(` + w + `)"`,                // 外側で走るコマンド置換
		bin + ` detach -- $(` + w + `)`,                  // 同上 (引用符なし)
		bin + " detach -- `" + w + "`",                   // 同上 (backquote)
		bin + ` detach -- 'a'"$(` + w + `)"`,             // 連結した語の中のコマンド置換
		bin + ` detach -- 'x' <(` + w + `)`,              // プロセス置換
		bin + ` detach -- a\ b '` + w + `'`,              // バックスラッシュ
		bin + ` detach -- 'echo' # ; ` + w,               // コメント
		bin + ` detach '` + w + `'`,                      // -- が無い
		bin + ` detach --bogus -- '` + w + `'`,           // 知らないオプション
		bin + "\n" + `detach -- '` + w + `'`,             // 起動と detach の間に改行
		`"$AGENT_HUB_BRIDGE_BINX" detach -- '` + w + `'`, // 別の変数
		`/usr/local/bin/other detach -- '` + w + `'`,     // bridge 以外の binary
		`cd /tmp && ` + bin + ` detach -- '` + w + `'`,   // 前に別のコマンド
		`echo ` + bin + ` detach -- '` + w + `'`,         // 先頭が detach の起動ではない
		`( ` + bin + ` detach -- '` + w + `' )`,          // subshell
		bin + ` detach -- ('` + w + `')`,                 // 括弧
	}
	for _, c := range blocked {
		if got := checkBlockingCommand(c); got != sleepPat {
			t.Errorf("checkBlockingCommand(%q) = %q, want %q", c, got, sleepPat)
		}
	}

	// sleep 以外のパターンも、detach の後ろなら今までどおり
	if got := checkBlockingCommand(`"$AGENT_HUB_BRIDGE_BIN" detach -- 'date -u'; gh run watch 1`); got != "gh run watch" {
		t.Errorf("trailing gh run watch: got %q", got)
	}
	if got := checkBlockingCommand(`"$AGENT_HUB_BRIDGE_BIN" detach -- 'date -u' | watch ls`); got != "watch <command>" {
		t.Errorf("trailing watch: got %q", got)
	}
}
