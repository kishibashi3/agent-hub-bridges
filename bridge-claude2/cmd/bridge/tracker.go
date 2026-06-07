// tracker.go — Activity Tracker + Message Gap Tracker (Python: worker.py の直訳)
//
// activityTracker: Claude が応答中かどうかを追跡して /status の busy 判定に使う (issue #46)。
// messageGapTracker: メッセージ受信間の gap を計測して SSE push silent death を推定する (issue #26)。
package main

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"
)

var (
	busyWindowS = func() float64 {
		if v := os.Getenv("AGENT_HUB_BUSY_WINDOW_S"); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f
			}
		}
		return 60.0
	}()

	pushSilentThresholdS = func() float64 {
		if v := os.Getenv("AGENT_HUB_PUSH_SILENT_THRESHOLD_S"); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f
			}
		}
		return 25.0
	}()
)

// activityTracker は Claude の最終アクティビティ時刻を追跡して /status の busy 判定に使う。
//
// issue #46: bridge が Claude を呼び出し中 (runner.query が実行中) にもかかわらず
// /status が idle を返す問題への対処。markActive() は runner.query の実行中に
// stream-json の assistant イベントを受信したときに呼ぶ。
//
// reconnect をまたいで 1 インスタンスを共有する。
type activityTracker struct {
	mu          sync.Mutex
	lastActive  time.Time
	hasActivity bool
}

// markActive は assistant メッセージ受信時に呼ぶ。
func (t *activityTracker) markActive() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastActive = time.Now()
	t.hasActivity = true
}

// status は直近 busyWindowS 秒以内にアクティブなら "busy"、それ以外は "idle" を返す。
func (t *activityTracker) status() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.hasActivity {
		return "idle"
	}
	if time.Since(t.lastActive).Seconds() < busyWindowS {
		return "busy"
	}
	return "idle"
}

// messageGapTracker はメッセージ受信間の gap を計測して SSE push silent death を推定する。
//
// issue #26: Go bridge は polling-only なので SSE push silent death の直接監視は不要だが、
// polling 間隔を超えるような gap (= hub から長時間メッセージが届かない) を検出するために使う。
// 精度の限界: 単純に「しばらくメッセージが来なかっただけ」との区別が不可能。
//
// reconnect をまたいで 1 インスタンスを共有する。
type messageGapTracker struct {
	mu              sync.Mutex
	lastReceived    time.Time
	hasLastReceived bool
}

// onMessageReceived はメッセージ受信時に呼ぶ。
// gap が pushSilentThresholdS 以上なら WARNING を出す。
func (t *messageGapTracker) onMessageReceived(msgID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if t.hasLastReceived {
		gap := now.Sub(t.lastReceived).Seconds()
		if gap >= pushSilentThresholdS {
			slog.Warn("[safety-net] message arrived after long gap",
				"msg_id", msgID,
				"gap_s", fmt.Sprintf("%.0f", gap),
				"threshold_s", fmt.Sprintf("%.0f", pushSilentThresholdS),
			)
		}
	}
	t.lastReceived = now
	t.hasLastReceived = true
}
