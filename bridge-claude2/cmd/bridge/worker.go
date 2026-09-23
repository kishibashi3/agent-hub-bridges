// worker.go — Bridge worker main loop (Python: worker.py の直訳)
//
// runWorker: bridge のメインエントリポイント。cursor / journal / tracker / gap_tracker を
//
//	初期化して runHubSession を reconnect ループで回す。
//	claudeRunner は状態を持たない (on-demand) ため reconnect をまたいで単一インスタンスを共有する。
//
// runHubSession: 1 回ぶんの hub session を最後まで走らせる。
//
//	journal replay → startup catchup → SSE push 駆動ループ (CommandRouter + handleOne)
//	SSE SubscribeInbox で is_online=true を維持し、push 受信時のみ GetMessages を発火する (issue #218)。
//	SIGTERM 受信時は SSE ループ内で runGracefulDrain() を呼んでから exit する (issue #178)。
//	safety-net poll / heartbeat: SSE が silently dead の場合も定期的に GetMessages を呼ぶ (issue #234)。
//	poll が失敗 → get_messages エラー → runWorker の reconnect ループが起動する。
//
// startupCatchup: bridge 起動時に未読メッセージを処理する (issue #98)。
//
// handleOne: message 1 件を Claude に流して応答を待つ。
//
//	claude subprocess は on-demand で spawn/exit する。
//
// journalledSend: journal write → hub.SendMessage → journal delete の順で送信を永続化する。
//
// replayJournal: 起動時に pending journal entries を replay する (issue #183)。
//
// runGracefulDrain: SIGTERM 時の graceful drain (issue #178)。
//
//	compact → 未処理メッセージ確認 → メッセージがあれば処理 → exit。
//	active session (client が生きている) 内で呼ぶことで final poll が可能になる。
//	idle compact watchdog は on-demand bridge では不要なため削除済み (issue #179)。
//
// limit 休眠 (issue #268): handleOne が limitReachedError を返したら processMessages が
//
//	limitSleeper に状態を入れ、SSE ループは reset 時刻まで GetMessages を呼ばずに待つ
//	(limit.go 参照)。復帰後は deferred → hub 未読の順に処理する。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
)

const (
	defaultReconnectBackoffS = 5.0
	maxRetriesEnv            = "AGENT_HUB_BRIDGE_MAX_RETRIES"
	defaultMaxRetries        = 10
)

// safety-net poll 兼 heartbeat の設定 (issue #234)
const (
	// inboxPollIntervalEnv は safety-net poll 間隔を秒数で指定する環境変数。
	// Python SDK の AGENT_HUB_INBOX_POLL_INTERVAL_S と同名で統一。
	inboxPollIntervalEnv     = "AGENT_HUB_INBOX_POLL_INTERVAL_S"
	defaultInboxPollInterval = 30 * time.Second
)

// runWorker はブリッジの outer loop。
// Python の run_worker() + run_with_reconnect() に相当。
// cursor / journal / tracker / gap_tracker を
// outer loop をまたいで共有する (= reconnect 後も状態を持ち越す)。
//
// claudeRunner は on-demand モードのため状態を持たず、
// runnerHolder ではなく単一インスタンスを複数 hub session をまたいで再利用する。
//
// SIGTERM 受信時の graceful drain は runHubSession 内の polling loop で実施する (issue #178)。
// idle compact watchdog は on-demand bridge では不要なため削除済み (issue #179)。
func runWorker(ctx context.Context, cfg *config, mcpConfigPath string) {
	// issue #288: tenant 指定時は記録ファイル名に tenant を入れる。旧ファイルがあれば引き継ぐ。
	migrateStateFiles(cfg)

	// reconnect をまたいで共有する state
	cursor := loadCursor(cfg.stateKey())
	journal := newJournal(cfg.JournalDir, cfg.stateKey())
	tracker := &activityTracker{}
	gapTracker := &messageGapTracker{}
	// issue #268: limit 休眠状態。reconnect をまたいで休眠と deferred を引き継ぐ。
	// issue #271: deferred はファイルにも記録する。前回プロセスが休眠中に落ちていれば
	// その deferred (MarkAsRead 済み・未処理) を WARN で出す。
	sleeper := &limitSleeper{store: newDeferredStore(cfg.JournalDir, cfg.stateKey())}
	warnLostDeferred(sleeper.store)

	// on-demand モード: runner は状態を持たないため単一インスタンスを使い回す。
	// Python の ClaudeSDKClient と違い、subprocess はフィールドに保持しない。
	runner := newClaudeRunner(cfg, mcpConfigPath)

	// circuit breaker (issue #82)
	maxRetries := cfg.MaxRetries // 0 = unlimited
	consecutiveFailures := 0

	for {
		select {
		case <-ctx.Done():
			// active session 開始前に SIGTERM → drain 対象の client がないためそのまま exit
			slog.Info("runWorker: shutting down (no active session)")
			return
		default:
		}

		// hub セッション開始
		newCursor, established, err := runHubSession(
			ctx, cfg, mcpConfigPath,
			runner, cursor, tracker, gapTracker, journal, sleeper,
		)
		cursor = newCursor // セッション終了時点の cursor を引き継ぐ

		if ctx.Err() != nil {
			// issue #178: runHubSession 内の polling loop で graceful drain 済み
			slog.Info("runWorker: context cancelled, shutting down")
			return
		}

		if err != nil {
			// issue #185: 再接続成功後 (established=true) は accumulated failures をリセットしてから
			// 今回のエラーを 1 としてカウントする。TCP idle timeout などで session が切れても
			// backoff が累積しないようにする。
			if established {
				consecutiveFailures = 0
			}
			consecutiveFailures++
			slog.Warn("runWorker: hub session ended with error",
				"err", err,
				"consecutive_failures", consecutiveFailures,
			)

			// circuit breaker
			if maxRetries > 0 && consecutiveFailures >= maxRetries {
				slog.Error("[circuit-breaker] ALERT: hub connection assumed lost",
					"user", cfg.Participant,
					"consecutive_failures", consecutiveFailures,
					"max_retries", maxRetries,
				)
				// dead marker + inventory 通知 (issue #82)
				writeDeadMarker(cfg.Participant)
				writeLostHubToInventory(cfg.Participant, os.Getpid())
				slog.Error("[circuit-breaker] dead marker written — run stop-bridge.sh --dead to clean up")
				return
			}
		} else {
			consecutiveFailures = 0
		}

		slog.Info("runWorker: reconnecting",
			"backoff_s", cfg.ReconnectBackoff.Seconds(),
			"consecutive_failures", consecutiveFailures,
		)
		sleepWithContext(ctx, cfg.ReconnectBackoff)
	}
}

// runHubSession は 1 回ぶんの hub session を最後まで走らせる。
// Python の _run_hub_session() に相当。
// established は SSE ループに入った（hub に到達できた）ことを示す。
// エラーが発生した場合は cursor・established・エラーを返す。
func runHubSession(
	ctx context.Context,
	cfg *config,
	mcpConfigPath string,
	runner *claudeRunner,
	cursor cursorPos,
	tracker *activityTracker,
	gapTracker *messageGapTracker,
	journal *Journal,
	sleeper *limitSleeper,
) (cursorPos, bool, error) {
	// --- hub client 初期化 ---
	client, err := agenthub.New(
		cfg.AgentHubURL, cfg.GitHubPAT, cfg.Participant, cfg.Tenant,
		agenthub.WithClientName(bridgeType),
	)
	if err != nil {
		return cursor, false, fmt.Errorf("agenthub.New: %w", err)
	}
	if err := client.Initialize(ctx); err != nil {
		return cursor, false, fmt.Errorf("initialize: %w", err)
	}
	// issue #268: 休眠中に reconnect した場合は休眠中の display_name で登録し直す
	// (get_participants から休眠状態が見え続けるようにする)。
	registerName := cfg.DisplayName
	if sleeping, until, kind := sleeper.state(); sleeping {
		registerName = sleepingDisplayName(cfg.DisplayName, until, kind)
	}
	if _, err := client.Register(ctx, registerName, cfg.Mode); err != nil {
		return cursor, false, fmt.Errorf("register: %w", err)
	}
	// SSE keepalive: claude subprocess 実行中の MCP セッション expire を防ぐ (issue #41)
	if err := client.StartSSE(ctx); err != nil {
		return cursor, false, fmt.Errorf("start SSE: %w", err)
	}
	defer client.StopSSE()

	// is_online = true に更新し、inbox push を受け取る (issue #198)
	if err := client.SubscribeInbox(ctx); err != nil {
		slog.Warn("runHubSession: SubscribeInbox failed — falling back to polling only", "err", err)
	}
	// push 受信時にポーリングループへ即時 GetMessages シグナルを送る (issue #198)
	// バッファ 1: push が連続しても積み上がらない
	pushCh := make(chan struct{}, 1)
	client.OnInboxPush(func() {
		select {
		case pushCh <- struct{}{}:
		default:
		}
	})

	slog.Info("runHubSession: registered and listening",
		"handle", "@"+cfg.Participant,
		"mode", cfg.Mode,
		"display_name", registerName,
	)

	// safety-net poll / heartbeat: SSE が silently dead になった場合も定期的に
	// GetMessages を呼び、失敗したら reconnect ループへ戻す (issue #234)。
	// Python SDK の inbox() の poll_loop + heartbeat_loop に相当。
	// Go SDK はツール呼び出しを単一 goroutine で行う制約があるため、
	// ticker を main select に組み込んで main goroutine から呼ぶ。
	pollInterval := resolveInboxPollInterval()
	pollTicker := time.NewTicker(pollInterval)
	defer pollTicker.Stop()
	slog.Info("runHubSession: safety-net poll enabled",
		"interval_s", pollInterval.Seconds(),
	)

	// CommandRouter を生成 (Python の router = CommandRouter() に相当)
	// SDK の CommandRouter を使う (issue #43)。
	// on-demand モードでは /restart は no-op になる。
	router := agenthub.NewCommandRouter()
	router.SetStatusFunc(tracker.status)
	router.SetRestartHandler(func(ctx context.Context) error {
		return runner.restart(ctx)
	})

	// journal replay: 前回クラッシュ時の pending entries を再送 (issue #183)
	replayJournal(ctx, client, journal)

	selfHandle := "@" + cfg.Participant

	// startup catchup: bridge 起動時に未読メッセージを処理する (issue #98)
	// issue #268: 休眠中 (reconnect 後) は inbox を読まない。
	if sleeping, _, _ := sleeper.state(); !sleeping {
		cursor, err = startupCatchup(
			ctx, cfg, client,
			runner, cursor,
			tracker, gapTracker, journal, sleeper,
		)
		if err != nil {
			slog.Warn("runHubSession: startup catchup error (continuing)", "err", err)
		}
	}

	// --- SSE push 駆動ループ (issue #218, #234) ---
	// 通常は inbox push 通知を受信したときのみ GetMessages を呼ぶ。
	// SSE が silently dead の場合は pollTicker が定期的にトリガーして
	// GetMessages を呼ぶ (safety-net poll / heartbeat)。
	// GetMessages 失敗時は runWorker の reconnect ループが起動する。
	// is_online=true は SSE 接続 (StartSSE + SubscribeInbox) が維持する。
	//
	// issue #268: limit 休眠中は push / poll を無視して reset 時刻まで待つ
	// (GetMessages を呼ばない = hub の queue に未読を残す)。SSE は維持するので
	// is_online=true のまま、display_name で休眠状態を示す。
	for {
		if sleeping, until, kind := sleeper.state(); sleeping {
			slog.Info("[limit] sleeping — inbox fetch suspended",
				"kind", kind, "until", until.Format(time.RFC3339),
				"remaining_s", fmt.Sprintf("%.0f", time.Until(until).Seconds()),
				"deferred", sleeper.deferredCount(),
			)
			wakeTimer := time.NewTimer(time.Until(until))
			select {
			case <-ctx.Done():
				wakeTimer.Stop()
				runGracefulDrain(client, runner, cfg, cursor, tracker, journal, selfHandle, sleeper)
				return cursor, true, ctx.Err()
			case <-wakeTimer.C:
			}
			// 復帰: display_name を通常に戻す。失敗したら session を張り直す
			// (reconnect 時の Register は sleeper.state() を見るので、wake() より前に行う)。
			if _, err := client.Register(ctx, cfg.DisplayName, cfg.Mode); err != nil {
				return cursor, true, fmt.Errorf("register after limit sleep: %w", err)
			}
			deferred := sleeper.wake()
			slog.Info("[limit] woke up — resuming inbox fetch",
				"kind", kind, "deferred", len(deferred))
			// 休眠中に積まれた push シグナルは捨てる (この直後に GetMessages を呼ぶ)
			select {
			case <-pushCh:
			default:
			}
			// deferred (limit 到達時点で MarkAsRead 済み・未処理) を hub 未読より先に処理。
			// batch 途中で limit に当たった場合、残りはまだ router を通っていないので
			// 復帰時も router を渡してスラッシュコマンドを claude に流さない (PR #269 review M3)
			cursor = processMessages(ctx, cfg, client, runner, router, cursor,
				tracker, gapTracker, journal, sleeper, deferred, "[limit-resume]")
			// issue #271: 処理し終えたら deferred の記録を消す。SIGTERM で中断された場合は
			// 残りが未処理のまま流れているので、次回起動時の WARN のために残す。
			if ctx.Err() == nil {
				sleeper.finishResume()
			}
			if sleeping, _, _ := sleeper.state(); sleeping {
				continue // deferred 処理中に再度 limit → もう一度休眠
			}
			// fall through: hub の未読を取りに行く
		} else {
			// push / safety-net poll / SIGTERM を待つ
			select {
			case <-ctx.Done():
				// issue #178: graceful drain — compact → 未処理メッセージ確認 → 処理 → exit
				// client が生きているこのタイミングで drain を実施する。
				runGracefulDrain(client, runner, cfg, cursor, tracker, journal, selfHandle, sleeper)
				return cursor, true, ctx.Err()
			case <-pushCh:
				slog.Debug("runHubSession: inbox push — calling GetMessages")
			case <-pollTicker.C:
				slog.Debug("runHubSession: safety-net poll / heartbeat — calling GetMessages")
			}
		}

		msgs, err := client.GetMessages(ctx)
		if err != nil {
			slog.Warn("runHubSession: get_messages error", "err", err)
			return cursor, true, fmt.Errorf("get_messages: %w", err)
		}

		cursor = processMessages(ctx, cfg, client, runner, router, cursor,
			tracker, gapTracker, journal, sleeper, msgs, "runHubSession")
	}
}

// processMessages は GetMessages で得たメッセージ列を順に処理し、更新後の cursor を返す。
// runHubSession の SSE ループ・startupCatchup・limit 復帰時の deferred 処理で共通に使う。
//
// router が非 nil ならスラッシュコマンドを CommandRouter で処理する (MarkAsRead は Handle 内部)。
// nil の場合はコマンドの分離を呼び出し側が済ませている前提。
//
// issue #268: handleOne が limitReachedError を返した場合、そのメッセージと残りを
// sleeper に deferred として預けて休眠に入り、即 return する。limit に当たったメッセージの
// cursor は保存しない (復帰後に再処理するため)。
func processMessages(
	ctx context.Context,
	cfg *config,
	client *agenthub.Client,
	runner *claudeRunner,
	router *agenthub.CommandRouter,
	cursor cursorPos,
	tracker *activityTracker,
	gapTracker *messageGapTracker,
	journal *Journal,
	sleeper *limitSleeper,
	msgs []agenthub.Message,
	logPrefix string,
) cursorPos {
	selfHandle := "@" + cfg.Participant

	for i, msg := range msgs {
		// 自己ループ防止
		if msg.Sender == selfHandle {
			slog.Debug(logPrefix+": skip self-sent message", "msg_id", msg.ID)
			_ = client.MarkAsRead(ctx, msg.ID)
			continue
		}

		// スラッシュコマンドを CommandRouter で処理 (MarkAsRead は Handle 内部で呼ばれる)
		if router != nil && router.Handle(ctx, client, msg) {
			continue
		}

		// issue #26: safety-net 発火推定 (gap 計測)
		gapTracker.onMessageReceived(msg.ID)

		// issue #37: cursor skip — 再起動後の重複 dispatch 防止
		if cursor.seen(msg) {
			slog.Info(logPrefix+": skipping already-seen message",
				"msg_id", msg.ID, "ts", msg.Timestamp, "cursor", cursor.TS)
			_ = client.MarkAsRead(ctx, msg.ID)
			continue
		}

		// issue #264: 直前までの内側セッションが先回りで返信/既読化済みの inbound は
		// 再 dispatch しない (二重応答防止)。
		if runner.innerHandled.has(msg.ID) {
			slog.Warn(logPrefix+": skipping already-replied-by-inner-session message (issue #264)",
				"msg_id", msg.ID, "from", msg.Sender)
			_ = client.MarkAsRead(ctx, msg.ID)
			continue
		}

		// issue #176: MarkAsRead を handleOne 前に呼ぶ。
		// SSE 駆動でも処理前に MarkAsRead しないと次回 GetMessages で
		// 同一メッセージが返ってきて二重 dispatch が発生する。
		// cursor check が secondary guard として機能するが、in-memory cursor は
		// reconnect でリセットされるため、server-side の既読状態を先に確定させる。
		if err := client.MarkAsRead(ctx, msg.ID); err != nil {
			slog.Warn(logPrefix+": pre-process mark_as_read failed; cursor will guard on retry",
				"msg_id", msg.ID, "err", err)
		}

		handleErr := handleOne(ctx, client, runner, msg, cfg, tracker, journal)
		if lim := asLimitError(handleErr); lim != nil {
			// issue #268: limit 到達 → このメッセージと残りを deferred にして休眠する。
			deferred := append([]agenthub.Message{msg}, msgs[i+1:]...)
			sleeper.enter(lim, deferred)
			slog.Info("[limit] entering sleep — no auto-reply sent, inbox fetch suspended",
				"kind", lim.Kind, "until", lim.Until.Format(time.RFC3339),
				"reset_parsed", lim.Parsed, "trigger_msg_id", msg.ID,
				"deferred", len(deferred), "cause", truncate(lim.Cause.Error(), 200),
			)
			name := sleepingDisplayName(cfg.DisplayName, lim.Until, lim.Kind)
			if _, err := client.Register(ctx, name, cfg.Mode); err != nil {
				slog.Warn("[limit] failed to update display_name for sleep (continuing)",
					"display_name", name, "err", err)
			}
			return cursor
		}
		if handleErr != nil {
			slog.Error(logPrefix+": handleOne error", "msg_id", msg.ID, "err", handleErr)
		}

		// issue #37, #176: process → save_cursor の順 (crash-safe secondary guard)。
		// MarkAsRead は上記で処理前に呼び済み。
		cursor = cursor.advance(msg)
		saveCursor(cfg.stateKey(), cursor)
	}
	return cursor
}

// startupCatchup は bridge 起動時に未読メッセージを処理する (issue #98)。
// hub 接続確立後・SSE ループ開始前に GetMessages を呼んでオフライン中に届いたメッセージを処理する。
// コマンドメッセージ (body が "/" で始まる) は SSE ループの CommandRouter に委ねるためスキップ。
func startupCatchup(
	ctx context.Context,
	cfg *config,
	client *agenthub.Client,
	runner *claudeRunner,
	cursor cursorPos,
	tracker *activityTracker,
	gapTracker *messageGapTracker,
	journal *Journal,
	sleeper *limitSleeper,
) (cursorPos, error) {
	msgs, err := client.GetMessages(ctx)
	if err != nil {
		slog.Warn("[startup-catchup] get_messages failed; skipping", "err", err)
		return cursor, nil // graceful degradation
	}

	// コマンドメッセージを分離
	var nlMsgs []agenthub.Message
	cmdCount := 0
	for _, m := range msgs {
		if len(m.Body) > 0 && m.Body[0] == '/' {
			cmdCount++
			continue
		}
		nlMsgs = append(nlMsgs, m)
	}

	if len(nlMsgs) == 0 {
		if cmdCount > 0 {
			slog.Info("[startup-catchup] command messages only; deferred to polling loop",
				"cmd_count", cmdCount)
		} else {
			slog.Info("[startup-catchup] no unread messages at startup")
		}
		return cursor, nil
	}

	slog.Info("[startup-catchup] processing unread messages",
		"nl_count", len(nlMsgs), "cmd_count", cmdCount)

	cursor = processMessages(ctx, cfg, client, runner, nil, cursor,
		tracker, gapTracker, journal, sleeper, nlMsgs, "[startup-catchup]")
	return cursor, nil
}

// handleOne は message 1 件を Claude に流して応答を待つ。
// Python の _handle_one() に相当。
// hub.MarkAsRead は caller が handleOne 呼び出し前に担当する (issue #176)。
// claude subprocess は on-demand で spawn/exit される (runner.query 内部で処理)。
//
// SubprocessTimeout による中断は errSubprocessTimeout で sentinel され、
// cfg.MaxQueryRetries 回までリトライする (issue #226)。
// SIGTERM による ctx.Canceled は retry しない。
func handleOne(
	ctx context.Context,
	client *agenthub.Client,
	runner *claudeRunner,
	msg agenthub.Message,
	cfg *config,
	tracker *activityTracker,
	journal *Journal,
) error {
	// issue #51: workdir が存在しない場合は early return
	if _, err := os.Stat(cfg.Workdir); err != nil {
		slog.Error("handleOne: workdir gone",
			"workdir", cfg.Workdir, "msg_id", msg.ID)
		// issue #272: この返信も echo guard / 送信元別 cooldown を通す (#267 と同型の往復防止)
		sendAutoErrorReply(ctx, client, runner, journal, msg,
			fmt.Sprintf("bridge の workdir が存在しません: %s", cfg.Workdir))
		return nil // caller が MarkAsRead する
	}

	slog.Info("← message",
		"msg_id", msg.ID, "from", msg.Sender, "body_preview", truncate(msg.Body, 120))

	prompt := formatPrompt("@"+cfg.Participant, msg, cfg.GitHubFooter)

	const retryBackoff = 5 * time.Second
	var lastUsage queryUsage
	var lastErr error

	for attempt := 0; attempt <= cfg.MaxQueryRetries; attempt++ {
		if attempt > 0 {
			slog.Warn("handleOne: retrying after subprocess timeout",
				"msg_id", msg.ID, "attempt", attempt, "max_retries", cfg.MaxQueryRetries,
				"backoff_s", retryBackoff.Seconds(),
			)
			sleepWithContext(ctx, retryBackoff)
			if ctx.Err() != nil {
				// SIGTERM が来ていたらリトライを中断
				lastErr = ctx.Err()
				break
			}
		}

		usage, err := runner.query(ctx, prompt, msg.Sender, tracker)
		// issue #267: telemetry span は query 成否に関わらず emit する (usage があれば記録)
		emitSpan(msg.ID, cfg.Model, usage)
		lastUsage = usage
		lastErr = err
		// issue #264 方針 A: 内側セッションが自分で返信/既読化した他の inbound を記録し、
		// 次の GetMessages で再 dispatch されないようにする (query の成否に関わらず、
		// tool_result で確認済みの分は hub に効いているので記録する)。
		recordInnerHandled(runner, msg.ID, usage.InnerHandledIDs)

		if err == nil {
			slog.Info("→ message processed", "msg_id", msg.ID, "from", msg.Sender)
			return nil
		}

		// issue #264/#266: subprocess が result 到達前に timeout kill されても、その前に
		// `mcp__agent-hub__send_message` の tool_result が is_error=false で確認できて
		// いれば、hub には既に応答が確実に送信済みである。ここでリトライすると同一
		// inbound message に対して 2 個目の Claude セッションが起動し、矛盾する内容の
		// 応答を二重送信してしまう (実例: 同一トピックについて食い違う応答が短時間に
		// 連続送信された)。二重送信のリスクの方が「タイムアウトでリトライして完了させる」
		// 利益より重いため、この場合はリトライせず打ち切る。
		//
		// tool_use は観測できたが tool_result での成功確認が取れていない場合
		// (usage.SentMessageObserved && !usage.SentMessageConfirmed) は、hub 側の
		// reject/失敗や tool_result 到達前の kill を含む未確定状態であり、「送信済み」
		// と決めつけて通知を握りつぶすと二重送信より悪いサイレント消失になりうる
		// (issue #266 レビュー指摘)。この場合は抑止せず通常のリトライ/エラー報告に
		// 進める。ログにも状態を残し、別チャネル (slog) から観測可能にする。
		// PR #273 レビュー M1: 抑止は「処理中の msg.ID への返信」が成功確認できた場合に限る。
		// 別 inbound への先回り返信 (issue #264) だけでは X の応答にならないので、通常の
		// リトライ/エラー通知/limit 休眠に進める。
		if usage.SentMessageObserved && !usage.repliedTo(msg.ID) {
			slog.Warn("handleOne: send_message tool_use observed but not confirmed sent "+
				"(no successful tool_result seen) — treating as unsent, not suppressing retry/notification (issue #266)",
				"msg_id", msg.ID, "attempt", attempt, "err", err,
			)
		}
		if usage.repliedTo(msg.ID) {
			slog.Warn("handleOne: subprocess failed after send_message already confirmed sent — "+
				"skipping retry to avoid duplicate/contradictory reply (issue #264)",
				"msg_id", msg.ID, "attempt", attempt, "err", err,
			)
			break
		}

		// SubprocessTimeout による中断のみリトライ対象。それ以外はすぐ break。
		if !errors.Is(err, errSubprocessTimeout) {
			break
		}
	}

	_ = lastUsage // usage は既に emitSpan 済み

	// issue #264/#266: 直前の attempt で処理中 inbound への send_message の送信成功
	// (caused_by == msg.ID) が確定していた場合のみ、
	// 送信者へのエラー通知 (journalledSend) をスキップする。tool_use のみ観測 (未確定) の
	// 場合はスキップせず通常通り通知する (詳細は上記ループ内コメント参照)。
	if lastUsage.repliedTo(msg.ID) {
		return lastErr
	}

	// issue #240: SIGTERM 等で ctx がキャンセルされた状態の query 失敗
	// (context.Canceled) はシャットダウン時の期待動作であり実エラーではない。
	// 送信者への擬陽性エラー報告をスキップする。journalledSend を呼ばないことで
	// journal.write 自体を回避し、次回起動時の replay による遅延誤送信も防ぐ。
	//
	// この分岐 (ctx.Err() != nil) は context.Canceled だけでなく
	// context.DeadlineExceeded も捕捉する点に注意 (issue #250):
	//   - 通常の polling loop では ctx が cancel → context.Canceled。
	//   - graceful drain 中は handleOne に drainCtx (WithTimeout) が渡る。drain が
	//     時間内に終われば ctx.Err() == nil なので通常どおりエラー報告される。
	//     ただし drain timeout (runGracefulDrain の compactTimeout + SubprocessTimeout
	//     + 1分) を超過した場合は ctx.Err() == context.DeadlineExceeded となり、
	//     この分岐で抑制される。drain は時間制限付きの best-effort なので、timeout
	//     超過後のエラー報告も実質的に擬陽性であり、抑制は意図的な挙動である。
	if ctx.Err() != nil {
		slog.Info("handleOne: query cancelled by shutdown — suppressing error report",
			"msg_id", msg.ID, "err", lastErr)
		return lastErr
	}

	slog.Error("handleOne: claude query error", "msg_id", msg.ID, "err", lastErr)

	// issue #268: spend/session limit 到達は送信元へ auto 返信せず、呼び出し側に
	// 休眠を指示する (limitReachedError)。auto 返信すると受信側 (scheduler の bounce /
	// 同じく limit 中の bridge) との間でピンポンが増幅する (issue #267 の 3 変種)。
	if lim := detectLimit(lastErr, time.Now()); lim != nil {
		return lim
	}

	sendAutoErrorReply(ctx, client, runner, journal, msg, fmt.Sprint(lastErr))
	return lastErr
}

// sendAutoErrorReply は送信元へ `autoErrorPrefix + " " + detail` の auto 返信を送る。
// auto 返信の全経路 (claude 起動失敗 / workdir 不在) はここを通し、以下の抑止を共通に掛ける。
//
//   - issue #268 / #267: inbound 自体が auto 返信 (自分・他 bridge の echo) なら返さない。
//     返すと bridge ⇄ bridge で相互反射する。
//   - issue #267: 同一送信元への auto 返信は cooldown 内 1 回まで。往復は bridge が
//     返信することで次の周回が始まるので、2 回目以降を抑止すれば相手が何を返そうと
//     (scheduler の bounce / 他 bridge の auto 返信) 1 往復で止まる (autoreply.go 参照)。
//   - issue #272: workdir 不在経路も同じ抑止に掛ける。
func sendAutoErrorReply(
	ctx context.Context,
	client *agenthub.Client,
	runner *claudeRunner,
	journal *Journal,
	msg agenthub.Message,
	detail string,
) {
	if isAutoErrorEcho(msg.Body) {
		slog.Warn("sendAutoErrorReply: inbound is an auto error echo — suppressing auto error reply (issue #268)",
			"msg_id", msg.ID, "from", msg.Sender)
		return
	}
	if ok, wait := runner.autoReply.allow(msg.Sender, time.Now()); !ok {
		slog.Warn("sendAutoErrorReply: auto error reply suppressed by per-sender cooldown (issue #267)",
			"msg_id", msg.ID, "from", msg.Sender,
			"cooldown_s", fmt.Sprintf("%.0f", autoReplyCooldown.Seconds()),
			"retry_after_s", fmt.Sprintf("%.0f", wait.Seconds()))
		return
	}
	errMsg := fmt.Sprintf("%s %s", autoErrorPrefix, detail)
	_ = journalledSend(ctx, client, journal, msg.Sender, errMsg, msg.ID)
}

// recordInnerHandled は runner.query が観測した InnerHandledIDs を runner.innerHandled に
// 積む (issue #264)。現在処理中の inbound (currentID) 自身への返信 caused_by は正規の
// 応答なので除外する (既に MarkAsRead + cursor 済みで、集合に入れる意味がない)。
func recordInnerHandled(runner *claudeRunner, currentID string, ids []string) {
	if runner == nil || runner.innerHandled == nil || len(ids) == 0 {
		return
	}
	var others []string
	for _, id := range ids {
		if id != currentID {
			others = append(others, id)
		}
	}
	if len(others) == 0 {
		return
	}
	runner.innerHandled.add(others...)
	slog.Warn("recordInnerHandled: inner session replied to / marked as read other inbound messages — "+
		"they will be skipped on next get_messages (issue #264)",
		"current_msg_id", currentID, "inner_handled_ids", others,
		"set_size", runner.innerHandled.size())
}

// journalledSend は journal write → hub.SendMessage → journal delete の順で送信を永続化する。
// Python の _journalled_send() に相当。
// hub.SendMessage が失敗した場合、entry は journal に残り次回起動時に replay される。
func journalledSend(
	ctx context.Context,
	client *agenthub.Client,
	journal *Journal,
	to, message, causedBy string,
) error {
	entry := journal.makeEntry(to, message, causedBy)
	// write → send → delete の順。write 失敗時は send を中止する (reviewer Critical: issue #183)。
	if !journal.write(entry) {
		return fmt.Errorf("journal write failed for entry %s (to=%s); send aborted", entry.ID, to)
	}
	sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := client.SendMessage(sendCtx, to, message, causedBy); err != nil {
		slog.Warn("journalledSend: hub.SendMessage failed; entry kept for replay",
			"entry_id", entry.ID, "to", to, "err", err)
		return err
	}
	journal.delete(entry.ID)
	return nil
}

// replayJournal は起動時に pending journal entries を replay する (issue #183)。
// bridge クラッシュ時に送信できなかったメッセージを再送する。
func replayJournal(ctx context.Context, client *agenthub.Client, journal *Journal) {
	entries := journal.loadAll()
	if len(entries) == 0 {
		return
	}
	slog.Warn("replayJournal: pending entries found — replaying",
		"count", len(entries))
	for _, entry := range entries {
		slog.Info("replayJournal: replaying entry",
			"id", entry.ID, "to", entry.To, "created_at", entry.CreatedAt)
		sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := client.SendMessage(sendCtx, entry.To, entry.Message, entry.CausedBy)
		cancel()
		if err != nil {
			slog.Error("replayJournal: failed to replay entry; will retry on next startup",
				"id", entry.ID, "to", entry.To, "err", err)
			continue
		}
		journal.delete(entry.ID)
		slog.Info("replayJournal: entry replayed successfully", "id", entry.ID)
	}
}

// runGracefulDrain は SIGTERM 受信後の graceful drain を実行する (issue #178)。
//
// フロー:
//  1. /compact を実行 (cancelled ctx ではなく drainCtx を使う)
//  2. compact 完了後に GetMessages で未処理メッセージを確認
//  3. compact 中に届いたメッセージがあれば処理してから exit
//  4. メッセージがなければ compact 完了後に exit
//
// drain タイムアウト = compactTimeout(5分) + SubprocessTimeout + 1分バッファ。
// compact・message 処理の失敗は WARN/ERROR ログのみ (exit を妨げない)。
// active session 内 (client が生きている polling loop の ctx.Done() 分岐) で呼ぶこと。
func runGracefulDrain(
	client *agenthub.Client,
	runner *claudeRunner,
	cfg *config,
	cursor cursorPos,
	tracker *activityTracker,
	journal *Journal,
	selfHandle string,
	sleeper *limitSleeper,
) {
	// issue #268: limit 休眠中は compact も message 処理も limit で失敗するだけなので
	// 何もせず exit する。deferred (MarkAsRead 済み・未処理) はプロセス終了で失われる
	// ため ID を WARN で残す。記録ファイル (issue #271) は消さないので次回起動時にも WARN が出る。
	if sleeping, until, kind := sleeper.state(); sleeping {
		deferred := sleeper.wake()
		ids := make([]string, 0, len(deferred))
		for _, m := range deferred {
			ids = append(ids, m.ID)
		}
		slog.Warn("[drain] shutdown during limit sleep — skipping compact/drain; deferred messages are dropped",
			"kind", kind, "until", until.Format(time.RFC3339),
			"deferred_count", len(deferred), "deferred_ids", ids,
		)
		return
	}

	const compactTimeout = 5 * time.Minute
	drainTimeout := compactTimeout + cfg.SubprocessTimeout + time.Minute
	drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	slog.Info("[drain] graceful drain started",
		"compact_timeout_s", compactTimeout.Seconds(),
		"drain_timeout_s", drainTimeout.Seconds(),
	)

	// Step 1: /compact
	slog.Info("[drain] running /compact")
	summary, err := runner.compact(drainCtx)
	if err != nil {
		slog.Warn("[drain] /compact failed", "err", err)
	} else {
		slog.Info("[drain] /compact completed")
		if archiveDir := compactArchiveDirFor(cfg.Workdir); archiveDir != "" {
			appendCompactSummary(summary, archiveDir)
		}
	}

	// Step 2: compact 中に届いたメッセージを確認
	msgs, err := client.GetMessages(drainCtx)
	if err != nil {
		slog.Warn("[drain] final get_messages failed", "err", err)
		return
	}

	// 未処理メッセージのみ抽出 (自己ループ・cursor 済み・コマンドを除く)
	var pending []agenthub.Message
	for _, m := range msgs {
		if m.Sender == selfHandle {
			_ = client.MarkAsRead(drainCtx, m.ID)
			continue
		}
		if cursor.seen(m) {
			_ = client.MarkAsRead(drainCtx, m.ID)
			continue
		}
		// issue #264: 内側セッションが返信/既読化済みの inbound は skip
		if runner.innerHandled.has(m.ID) {
			slog.Warn("[drain] skipping already-replied-by-inner-session message (issue #264)",
				"msg_id", m.ID, "from", m.Sender)
			_ = client.MarkAsRead(drainCtx, m.ID)
			continue
		}
		// コマンドメッセージはシャットダウン中は MarkAsRead せずスキップする。
		// 意図的に未読のまま留保 → 次回起動時の startupCatchup が CommandRouter 経由で処理する。
		// 自己ループ・cursor-seen と異なり MarkAsRead を呼ばないのはこのため。
		if len(m.Body) > 0 && m.Body[0] == '/' {
			slog.Info("[drain] skipping command message during shutdown (留保 → 次回起動で処理)", "msg_id", m.ID)
			continue
		}
		pending = append(pending, m)
	}

	if len(pending) == 0 {
		slog.Info("[drain] no pending messages, exiting cleanly")
		return
	}

	// Step 3: compact 中に届いたメッセージを処理
	slog.Info("[drain] processing messages received during compact", "count", len(pending))
	for _, msg := range pending {
		// issue #176 準拠: MarkAsRead を handleOne 前に呼ぶ
		if err := client.MarkAsRead(drainCtx, msg.ID); err != nil {
			slog.Warn("[drain] mark_as_read failed", "msg_id", msg.ID, "err", err)
		}
		err := handleOne(drainCtx, client, runner, msg, cfg, tracker, journal)
		if lim := asLimitError(err); lim != nil {
			// issue #268: drain 中に limit 到達 — 残りも同じく失敗するので打ち切る (auto 返信なし)
			slog.Warn("[drain] limit reached during drain — stopping (no auto-reply)",
				"kind", lim.Kind, "msg_id", msg.ID)
			return
		}
		if err != nil {
			slog.Error("[drain] handleOne error", "msg_id", msg.ID, "err", err)
		}
		cursor = cursor.advance(msg)
		saveCursor(cfg.stateKey(), cursor)
	}
	slog.Info("[drain] graceful drain completed")
}
