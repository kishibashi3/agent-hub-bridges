// blocking.go — Blocking command detection (Python: blocking_commands.py の直訳)
//
// bridge は DM を受信してから Claude に prompt を流す「メッセージ駆動型」のプロセス。
// Claude が gh run watch / 長時間 sleep / tail -f / watch などのブロッキングコマンドを
// 実行すると、bridge の receive_response ループが完了しなくなり後続 DM を受信できなくなる。
//
// checkBlockingCommand: コマンド文字列を検査してブロッキングパターン名を返す純粋関数。
// 一致しなければ "" を返す。
//
// 既知の検出限界 (Python 実装と同じ):
//   - sudo watch df などの prefix コマンドを挟んだ watch は検出できない。
//   - sleep $WAIT のような変数展開は静的解析では検出不可。
//
// detach (#426): コマンドが bridge の detach の起動で始まるときは、detach に渡した引数を
// 判定しない。detach した処理は bridge の外 (見届け役) で走るので、受信は止まらない。
// 引数を読み切れない形 (クォートが閉じていない・$ や ` や \ を含む等) は detach とみなさず、
// コマンド全体を今までどおり判定する。detach の後ろに続けたコマンドは今までどおり判定する。
// detach の前に別のコマンドを置いた形 (cd x && "$AGENT_HUB_BRIDGE_BIN" detach ...) も、
// 今までどおり全体を判定する。
package main

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

var (
	// ブロッキングパターン (Python: _BLOCKING_PATTERNS)
	blockingPatterns = []struct {
		name    string
		pattern *regexp.Regexp
	}{
		{
			"gh run watch",
			regexp.MustCompile(`\bgh\s+run\s+watch\b`),
		},
		{
			"tail -f / --follow",
			// -[a-zA-Z0-9]* で -f, -F, -100f, -nF など combined flags も捕捉する。
			// フラグを探すのは tail と同じコマンドの中だけ。; & | 改行で区切られた
			// 後ろの別コマンドのフラグ (readlink -f, pgrep -af 等) には当てない (#419)。
			regexp.MustCompile(`\btail\b[^;&|\n]*(?:\s-[a-zA-Z0-9]*[fF]\b|\s--follow\b)`),
		},
		{
			"watch <command>",
			// 行頭 or シェル演算子 (;,|,&,(,&&,||) の直後の watch のみ。
			// watchman / watchdog 等の別コマンドの引数 "watch" は除外する。
			regexp.MustCompile(`(?:^|&&|\|\||[;&|(])\s*watch\s`),
		},
	}

	// sleep <N> — 整数・小数値 (秒単位)
	sleepSecondsPattern = regexp.MustCompile(`\bsleep\s+(\d+(?:\.\d+)?)(?:\s|$|[;&|])`)
	sleepMinSeconds     = 60.0

	// sleep <N>m / <N>h / <N>d — 時間単位付き (m=分, h=時間, d=日)
	sleepUnitPattern = regexp.MustCompile(`\bsleep\s+\d+(?:\.\d+)?[mhd]\b`)

	// sleep infinity / sleep inf — 無限 sleep
	sleepInfinityPattern = regexp.MustCompile(`(?i)\bsleep\s+(?:infinity|inf)\b`)
)

// checkBlockingCommand はコマンド文字列を検査してブロッキングパターン名を返す。
// どのパターンにも一致しなければ "" を返す。
// detach の起動で始まるときは、detach の引数を除いた後ろだけを判定する (#426)。
func checkBlockingCommand(command string) string {
	if rest, ok := skipDetachInvocation(command); ok {
		return checkBlockingCommand(rest)
	}
	return matchBlockingPatterns(command)
}

// matchBlockingPatterns はコマンド文字列全体をブロッキングパターンと突き合わせる。
func matchBlockingPatterns(command string) string {
	// sleep 秒数チェック
	for _, m := range sleepSecondsPattern.FindAllStringSubmatch(command, -1) {
		if f, err := strconv.ParseFloat(m[1], 64); err == nil && f >= sleepMinSeconds {
			return "sleep <N>=60s+"
		}
	}
	// sleep 時間単位付き
	if sleepUnitPattern.MatchString(command) {
		return "sleep <N>=60s+"
	}
	// sleep 無限
	if sleepInfinityPattern.MatchString(command) {
		return "sleep <N>=60s+"
	}
	// その他パターン
	for _, p := range blockingPatterns {
		if p.pattern.MatchString(command) {
			return p.name
		}
	}
	return ""
}

// bridgeSelfExe は bridge 自身の実行ファイルのパス。claude には同じ値を
// AGENT_HUB_BRIDGE_BIN として渡している (runner.go の detachEnv)。
var bridgeSelfExe = sync.OnceValue(func() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return exe
})

// detachLaunchers は detach の起動として認める先頭の語 (#426)。
func detachLaunchers() []string {
	ls := []string{
		`"$` + detachBinEnv + `"`,
		`"${` + detachBinEnv + `}"`,
		`$` + detachBinEnv,
		`${` + detachBinEnv + `}`,
	}
	if exe := bridgeSelfExe(); exe != "" {
		ls = append(ls, `"`+exe+`"`, `'`+exe+`'`, exe)
	}
	return ls
}

// skipDetachInvocation は、command が detach の起動
//
//	<launcher> detach [--name <word>] -- <word>...
//
// で始まるとき、detach の引数の後ろ (; & | < > 改行 から先) を返して ok=true にする。
// 前に付いた空白と ; & | は読み飛ばす (後ろに続けた 2 つ目の detach も同じ扱いにするため)。
// 読み切れない形なら ok=false (呼び出し側はコマンド全体を判定する)。
func skipDetachInvocation(command string) (string, bool) {
	s := strings.TrimLeft(command, " \t\n;&|")
	matched := false
	for _, l := range detachLaunchers() {
		if rest, ok := strings.CutPrefix(s, l); ok && startsWithBlank(rest) {
			s, matched = rest, true
			break
		}
	}
	if !matched {
		return "", false
	}
	s = trimBlank(s)
	rest, ok := strings.CutPrefix(s, "detach")
	if !ok || !startsWithBlank(rest) {
		return "", false
	}
	s = trimBlank(rest)
	// detach のオプション。--name だけを認め、-- で終わる。
	for {
		if rest, ok := strings.CutPrefix(s, "--"); ok && (rest == "" || startsWithBlank(rest)) {
			s = rest
			break
		}
		var rest string
		switch {
		case strings.HasPrefix(s, "--name="):
			rest = s[len("--name="):]
		case strings.HasPrefix(s, "-name="):
			rest = s[len("-name="):]
		case strings.HasPrefix(s, "--name") && startsWithBlank(s[len("--name"):]):
			rest = trimBlank(s[len("--name"):])
		case strings.HasPrefix(s, "-name") && startsWithBlank(s[len("-name"):]):
			rest = trimBlank(s[len("-name"):])
		default:
			return "", false
		}
		after, ok := skipShellWord(rest)
		if !ok || after == rest || !(after == "" || startsWithBlank(after)) {
			return "", false
		}
		s = trimBlank(after)
	}
	// detach に渡す引数。区切りの演算子か末尾まで語を読む。
	for {
		s = trimBlank(s)
		if s == "" || strings.ContainsRune(";&|<>\n", rune(s[0])) {
			return s, true
		}
		after, ok := skipShellWord(s)
		if !ok || after == s {
			return "", false
		}
		s = after
	}
}

// skipShellWord は s の先頭の 1 語を読み飛ばし、残りを返す。語は空白・; & | < > 改行 の手前まで。
// 外側の shell で何かを実行しうる文字 ($ ` \ ( ) #) を含む語や、閉じていないクォートは ok=false。
// 単一引用符の中は何でもよい。二重引用符の中は $ ` \ を含まなければよい。
func skipShellWord(s string) (string, bool) {
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || strings.IndexByte(";&|<>\n", c) >= 0:
			return s[i:], true
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return "", false
			}
			i += j + 2
		case c == '"':
			j := strings.IndexByte(s[i+1:], '"')
			if j < 0 || strings.ContainsAny(s[i+1:i+1+j], "$`\\") {
				return "", false
			}
			i += j + 2
		case strings.IndexByte("$`\\()#", c) >= 0:
			return "", false
		default:
			i++
		}
	}
	return "", true
}

func startsWithBlank(s string) bool { return s != "" && (s[0] == ' ' || s[0] == '\t') }

func trimBlank(s string) string { return strings.TrimLeft(s, " \t") }

// buildBlockingErrorMessage はブロッキングコマンド検出時の deny メッセージを組み立てる。
func buildBlockingErrorMessage(patternName string) string {
	return fmt.Sprintf(
		"Error: blocking command detected (`%s`).\n"+
			"Blocking waits prevent this bridge from receiving messages.\n\n"+
			cannotWaitGuidance,
		patternName,
	)
}
