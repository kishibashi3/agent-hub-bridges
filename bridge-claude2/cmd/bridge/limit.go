// limit.go — Claude spend/session limit 到達時の休眠 (issue #268)
//
// 背景: claude 起動が "You've hit your session limit · resets 3pm (Asia/Tokyo)" 等で
// 失敗すると、bridge は従来その文字列を `(auto) bridge-claude2 error: …` として送信元へ
// DM していた。受信側が「何か返す存在」(scheduler の bounce / 同じく limit 中の bridge) だと
// この auto 返信が相互に増幅し、毎秒数十通のピンポンになる (issue #267 の 3 変種)。
//
// 対処 (bridge 側で完結、server 変更なし):
//  1. limit 系エラーは送信元へ auto 返信しない
//  2. reset 時刻を parse し、その時刻まで inbox 取得 (GetMessages) を止めて休眠する。
//     bridge プロセス自体と SSE 接続 (is_online=true) は維持するため、fleet watchdog
//     (`agenthubctl bridge start --all`: PID 生存 + cmdline 突合で running 判定) は
//     休眠中の bridge を起こし直さない。未処理メッセージは hub 側の queue に残り、
//     復帰後に順次処理する
//  3. display_name を `… (sleeping until 18:50 JST: spend limit)` に更新して
//     get_participants で休眠状態が見えるようにする
//  4. reset 時刻を parse できない limit 系エラーは固定 30 分の休眠にフォールバック
//  5. inbound が auto 返信 (`(auto) ` または legacy の `(自動応答)` で始まる) の場合は
//     (limit 以外の失敗でも) auto 返信しない (二重防御、issue #267 対処 2 / issue #275)
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
)

const (
	// limitSleepFallback は reset 時刻を parse できなかった limit 系エラーの休眠時間。
	limitSleepFallback = 30 * time.Minute
	// limitWakeBuffer は parse した reset 時刻に加える余裕。reset 時刻ちょうどに起きると
	// Claude 側の反映遅れでもう一度 limit に当たり、無駄な subprocess 起動になるため。
	limitWakeBuffer = time.Minute
	// limitStaleResetGrace: parse した reset 時刻が「直前に過ぎたばかり」なら、Claude 側の
	// 反映遅れ (または CLI と bridge の時計ずれ) とみなして翌日扱いにせず fallback 休眠する。
	// これを超えて過去なら翌日の同時刻を指すものとして扱う。
	limitStaleResetGrace = time.Hour
)

// autoErrorPrefix は bridge が送信元へ返す auto 返信 (claude 起動失敗 / workdir 不在) の
// 先頭文字列。送信 (sendAutoErrorReply) 専用。受信判定 (isAutoErrorEcho) は他 bridge の
// auto 返信も拾うため、autoReplyCommonPrefix の前方一致で行う (issue #272 / issue #275)。
const autoErrorPrefix = "(auto) " + bridgeType + " error:"

// legacyAutoReplyPrefix は Python 版 bridge (gemini / codex / a2a 等) が今も使う auto 返信の
// 先頭文字列。bridge-claude2 自身はもう送らないが、fleet 内の他 bridge からの echo を
// 判定するために残す (issue #272)。
const legacyAutoReplyPrefix = "(自動応答)"

// autoReplyCommonPrefix は fleet 内の全 bridge が auto 返信の先頭に付ける共通文字列。
// bridge-type ごとに続く文言は異なる (`(auto) <bridge-type> error:` 等) ため、受信判定は
// この共通部分の前方一致で行い、他 bridge の auto 返信も echo として扱う (issue #275)。
const autoReplyCommonPrefix = "(auto) "

// isAutoErrorEcho は inbound message が他 bridge (または自分) の auto 返信かどうかを返す。
// これに対して auto エラー返信を返すと bridge ⇄ bridge で相互反射するため、呼び出し側は
// この場合 auto 返信を抑止する (issue #267 変種 3)。
func isAutoErrorEcho(body string) bool {
	return strings.HasPrefix(body, autoReplyCommonPrefix) || strings.HasPrefix(body, legacyAutoReplyPrefix)
}

// limitReachedError は claude 起動失敗が spend/session limit によるものであることを示す。
// handleOne が返し、呼び出し側 (processMessages) が errors.As で検出して休眠に入る。
type limitReachedError struct {
	// Kind は "session limit" / "spend limit" / "limit" (種別不明の limit 系)。
	Kind string
	// Until は休眠解除時刻。
	Until time.Time
	// Parsed は Until を reset 時刻の parse から得たか (false = fallback 固定時間)。
	Parsed bool
	Cause  error
}

func (e *limitReachedError) Error() string {
	return fmt.Sprintf("claude %s reached (sleeping until %s): %v",
		e.Kind, e.Until.Format(time.RFC3339), e.Cause)
}

func (e *limitReachedError) Unwrap() error { return e.Cause }

var (
	// limitKindPatterns は limit 種別の判定順。先に一致したものを Kind にする。
	// 実ログ例:
	//   "You've hit your session limit · resets 3pm (Asia/Tokyo)"
	//   "You've hit your individual spend limit · run /usage-credits to ask your admin for
	//    a higher limit · your session limit resets 6:50pm (Asia/Tokyo)"
	limitKindPatterns = []struct {
		kind string
		re   *regexp.Regexp
	}{
		{"spend limit", regexp.MustCompile(`(?i)\bspend limit\b`)},
		{"session limit", regexp.MustCompile(`(?i)\bsession limit\b`)},
	}
	// resetTimePattern は "resets 6:50pm (Asia/Tokyo)" / "resets 3pm (Asia/Tokyo)" / "resets 9pm"
	// にマッチする。group: 1=hour 2=minute(省略可) 3=am|pm 4=tz(省略可)
	resetTimePattern = regexp.MustCompile(`(?i)\bresets?\s+(\d{1,2})(?::(\d{2}))?\s*(am|pm)\b(?:\s*\(([^)]+)\))?`)
	// limitHintPattern は limit 判定に落ちなかったエラーのうち「limit 文言の変種かもしれない」
	// ものを WARN で拾うための緩い判定 (PR #269 review M1)。判定そのものには使わない。
	limitHintPattern = regexp.MustCompile(`(?i)\blimit\b|\bresets?\b`)
)

// detectLimit は claude 起動失敗 err が limit 系かどうかを判定し、limit 系なら
// 休眠解除時刻を決めた limitReachedError を返す。limit 系でなければ nil。
// now は判定基準時刻 (テスト容易性のため引数)。
func detectLimit(err error, now time.Time) *limitReachedError {
	if err == nil {
		return nil
	}
	text := err.Error()
	kind := ""
	for _, p := range limitKindPatterns {
		if p.re.MatchString(text) {
			kind = p.kind
			break
		}
	}
	until, parsed := parseResetTime(text, now)
	if kind == "" {
		// 種別語がない場合は am/pm 付きの reset 時刻 (resetTimePattern) の一致を要求する。
		// `\bresets?\s+\d` のような緩い判定だと "connection reset 3 times" 等の一般エラーを
		// limit と誤判定して 30 分休眠してしまう (PR #269 review M1)。
		if !parsed {
			if limitHintPattern.MatchString(text) {
				slog.Warn("[limit] error mentions limit/reset but did not match a known limit wording — treating as generic error",
					"cause", truncate(text, 200))
			}
			return nil
		}
		kind = "limit"
	}
	if parsed {
		return &limitReachedError{Kind: kind, Until: until, Parsed: true, Cause: err}
	}
	return &limitReachedError{Kind: kind, Until: now.Add(limitSleepFallback), Parsed: false, Cause: err}
}

// parseResetTime は "resets 6:50pm (Asia/Tokyo)" 形式から休眠解除時刻を求める。
// 返す時刻は reset 時刻 + limitWakeBuffer。
//   - tz が省略 / 不明なら bridge のローカル時刻で解釈する
//   - reset 時刻が now より未来ならその日の時刻
//   - now より limitStaleResetGrace 以内の過去なら「反映遅れ」とみなし now + fallback
//     (翌日まで 24 時間近く寝てしまう誤動作を防ぐ)
//   - それより過去なら翌日の同時刻
func parseResetTime(text string, now time.Time) (time.Time, bool) {
	m := resetTimePattern.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}, false
	}
	var hour, minute int
	if _, err := fmt.Sscan(m[1], &hour); err != nil || hour < 1 || hour > 12 {
		return time.Time{}, false
	}
	if m[2] != "" {
		if _, err := fmt.Sscan(m[2], &minute); err != nil || minute < 0 || minute > 59 {
			return time.Time{}, false
		}
	}
	if strings.EqualFold(m[3], "pm") && hour != 12 {
		hour += 12
	} else if strings.EqualFold(m[3], "am") && hour == 12 {
		hour = 0
	}

	loc := now.Location()
	if m[4] != "" {
		if l, err := time.LoadLocation(strings.TrimSpace(m[4])); err == nil {
			loc = l
		} else {
			slog.Warn("[limit] unknown timezone in reset time; using bridge local time",
				"tz", m[4], "err", err)
		}
	} else {
		// tz 省略時も無言でローカル解釈せず一行残す (host が UTC で "resets 9pm" だと
		// 寝過ごしうる、PR #269 review S2)
		slog.Warn("[limit] reset time has no timezone; interpreting as bridge local time",
			"reset", m[0], "local_tz", loc.String())
	}
	local := now.In(loc)
	target := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
	if !target.After(local) {
		if local.Sub(target) < limitStaleResetGrace {
			return local.Add(limitSleepFallback), true
		}
		target = target.Add(24 * time.Hour)
	}
	return target.Add(limitWakeBuffer), true
}

// sleepingDisplayName は休眠中の display_name を組み立てる。
// 例: "planner — go bridge (sleeping until 18:50 JST: spend limit)"
func sleepingDisplayName(base string, until time.Time, kind string) string {
	return fmt.Sprintf("%s (sleeping until %s: %s)", base, until.Format("15:04 MST"), kind)
}

// limitSleeper は休眠状態を保持する。runWorker が生成し、reconnect をまたいで共有する
// (休眠中に hub session が切れて再接続しても休眠と deferred を引き継ぐため)。
//
// deferred は limit に当たった時点で「MarkAsRead 済みだが未処理」のメッセージ。
// bridge は処理前に MarkAsRead する (issue #176) ため、hub の queue には戻せない。
// in-memory で保持し、復帰後に hub の未読より先に処理する。
type limitSleeper struct {
	mu       sync.Mutex
	active   bool
	until    time.Time
	kind     string
	deferred []agenthub.Message
}

// enter は休眠状態に入る。既に休眠中なら until/kind を上書きし deferred を追記する。
func (s *limitSleeper) enter(e *limitReachedError, deferred []agenthub.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active = true
	s.until = e.Until
	s.kind = e.Kind
	s.deferred = append(s.deferred, deferred...)
}

// state は (休眠中か, 解除時刻, 種別) を返す。
func (s *limitSleeper) state() (bool, time.Time, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active, s.until, s.kind
}

// deferredCount は deferred メッセージ数を返す (ログ用)。
func (s *limitSleeper) deferredCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.deferred)
}

// wake は休眠を解除し、deferred メッセージを取り出して返す。
func (s *limitSleeper) wake() []agenthub.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active = false
	s.until = time.Time{}
	s.kind = ""
	d := s.deferred
	s.deferred = nil
	return d
}

// asLimitError は err が limitReachedError を含むなら取り出す。
func asLimitError(err error) *limitReachedError {
	var lim *limitReachedError
	if errors.As(err, &lim) {
		return lim
	}
	return nil
}
