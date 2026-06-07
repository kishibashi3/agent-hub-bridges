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
package main

import (
	"fmt"
	"regexp"
	"strconv"
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
			// -[a-zA-Z0-9]* で -f, -F, -100f, -nF など combined flags も捕捉する
			regexp.MustCompile(`\btail\b.*(?:\s-[a-zA-Z0-9]*[fF]\b|\s--follow\b)`),
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
func checkBlockingCommand(command string) string {
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

// buildBlockingErrorMessage はブロッキングコマンド検出時の deny メッセージを組み立てる。
func buildBlockingErrorMessage(patternName string) string {
	return fmt.Sprintf(
		"Error: blocking command detected (`%s`).\n"+
			"Blocking waits prevent this bridge from receiving messages.\n\n"+
			"Instead, use @scheduler:\n"+
			"  @scheduler /run_in 10m @<your-handle> <alternative-command>",
		patternName,
	)
}
