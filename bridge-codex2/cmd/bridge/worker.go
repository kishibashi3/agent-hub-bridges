// worker.go — Bridge worker main loop
//
// bridge-claude2 の worker.go に相当するが、compact ステップを省略している。
// codex CLI には /compact 相当の機能がないため、runGracefulDrain では
// compact をスキップして未処理メッセージの drain のみ実施する。
//
// runWorker: bridge のメインエントリポイント。cursor / journal / tracker / gap_tracker を
//
//	初期化して runHubSession を reconnect ループで回す。
//	codexRunner は状態を持つ (hasSession) ため、reconnect をまたいで単一インスタンスを共有する。
//
// runHubSession: 1 回ぶんの hub session を最後まで走らせる。
//
//	journal replay → startup catchup → polling loop (CommandRouter + handleOne)
//	SSE SubscribeInbox で is_online=true に更新し、push 受信時は即時 GetMessages を発火する (issue #198)。
//	push が来なくても PollInterval ごとにフォールバックポーリングを行う。
//	SIGTERM 受信時は polling loop 内で runGracefulDrain() を呼んでから exit する。
//
// startupCatchup: bridge 起動時に未読メッセージを処理する。
//
// handleOne: message 1 件を codex に流して応答を待つ。
//
//	codex subprocess は on-demand で spawn/exit する。
//
// journalledSend: journal write → hub.SendMessage → journal delete の順で送信を永続化する。
//
// replayJournal: 起動時に pending journal entries を replay する。
//
// runGracefulDrain: SIGTERM 時の graceful drain。
//
//	compact なし → 未処理メッセージ確認 → メッセージがあれば処理 → exit。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	agenthub "github.com/kishibashi3/agent-hub-sdk/go"
)

// runWorker はブリッジの outer loop。
// cursor / journal / tracker / gap_tracker を outer loop をまたいで共有する。
// codexRunner は hasSession 状態を持つため reconnect をまたいで単一インスタンスを共有する。
func runWorker(ctx context.Context, cfg *config) {
	cursor := loadCursor(cfg.User)
	journal := newJournal(cfg.User)
	tracker := &activityTracker{}
	gapTracker := &messageGapTracker{}
	runner := newCodexRunner(cfg)

	maxRetries := cfg.MaxRetries
	consecutiveFailures := 0

	for {
		select {
		case <-ctx.Done():
			slog.Info("runWorker: shutting down (no active session)")
			return
		default:
		}

		newCursor, established, err := runHubSession(
			ctx, cfg,
			runner, cursor, tracker, gapTracker, journal,
		)
		cursor = newCursor

		if ctx.Err() != nil {
			slog.Info("runWorker: context cancelled, shutting down")
			return
		}

		if err != nil {
			if established {
				consecutiveFailures = 0
			}
			consecutiveFailures++
			slog.Warn("runWorker: hub session ended with error",
				"err", err,
				"consecutive_failures", consecutiveFailures,
			)

			if maxRetries > 0 && consecutiveFailures >= maxRetries {
				slog.Error("[circuit-breaker] ALERT: hub connection assumed lost",
					"user", cfg.User,
					"consecutive_failures", consecutiveFailures,
					"max_retries", maxRetries,
				)
				writeDeadMarker(cfg.User)
				writeLostHubToInventory(cfg.User, os.Getpid())
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
func runHubSession(
	ctx context.Context,
	cfg *config,
	runner *codexRunner,
	cursor cursorPos,
	tracker *activityTracker,
	gapTracker *messageGapTracker,
	journal *Journal,
) (cursorPos, bool, error) {
	client, err := agenthub.New(
		cfg.AgentHubURL, cfg.GitHubPAT, cfg.User, cfg.Tenant,
		agenthub.WithClientName("bridge-codex2"),
	)
	if err != nil {
		return cursor, false, fmt.Errorf("agenthub.New: %w", err)
	}
	if err := client.Initialize(ctx); err != nil {
		return cursor, false, fmt.Errorf("initialize: %w", err)
	}
	if _, err := client.Register(ctx, cfg.DisplayName, cfg.Mode); err != nil {
		return cursor, false, fmt.Errorf("register: %w", err)
	}
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
		"handle", "@"+cfg.User,
		"mode", cfg.Mode,
		"display_name", cfg.DisplayName,
	)

	router := agenthub.NewCommandRouter()
	router.SetStatusFunc(tracker.status)
	router.SetRestartHandler(func(ctx context.Context) error {
		return runner.restart(ctx)
	})

	replayJournal(ctx, client, journal)

	cursor, err = startupCatchup(
		ctx, cfg, client,
		runner, cursor,
		tracker, gapTracker, journal,
	)
	if err != nil {
		slog.Warn("runHubSession: startup catchup error (continuing)", "err", err)
	}

	selfHandle := "@" + cfg.User

	for {
		select {
		case <-ctx.Done():
			runGracefulDrain(client, runner, cfg, cursor, tracker, journal, selfHandle)
			return cursor, true, ctx.Err()
		default:
		}

		msgs, err := client.GetMessages(ctx)
		if err != nil {
			slog.Warn("runHubSession: get_messages error", "err", err)
			return cursor, true, fmt.Errorf("get_messages: %w", err)
		}

		for _, msg := range msgs {
			if msg.Sender == selfHandle {
				slog.Debug("runHubSession: skip self-sent message", "msg_id", msg.ID)
				// best-effort: 失敗は致命的でなく、次回 polling で再試行される
				_ = client.MarkAsRead(ctx, msg.ID)
				continue
			}

			if router.Handle(ctx, client, msg) {
				continue
			}

			gapTracker.onMessageReceived(msg.ID)

			if cursor.seen(msg) {
				slog.Info("runHubSession: skipping already-seen message",
					"msg_id", msg.ID, "ts", msg.Timestamp, "cursor", cursor.TS)
				// best-effort: 失敗は致命的でなく、次回 polling で再試行される
				_ = client.MarkAsRead(ctx, msg.ID)
				continue
			}

			// issue #336 #4: 内側 codex が先回りで返信済みの inbound は流さない (二重返信防止)
			if runner.innerReplied.has(msg.ID) {
				slog.Warn("runHubSession: skipping already-replied-by-inner-codex message (issue #336)",
					"msg_id", msg.ID, "from", msg.Sender)
				_ = client.MarkAsRead(ctx, msg.ID)
				continue
			}

			// issue #176: MarkAsRead を handleOne 前に呼ぶ
			if err := client.MarkAsRead(ctx, msg.ID); err != nil {
				slog.Warn("runHubSession: pre-process mark_as_read failed; cursor will guard on retry",
					"msg_id", msg.ID, "err", err)
			}

			handleErr := handleOne(ctx, client, runner, msg, cfg, tracker, journal)
			if handleErr != nil {
				slog.Error("runHubSession: handleOne error", "msg_id", msg.ID, "err", handleErr)
			}

			cursor = cursor.advance(msg)
			saveCursor(cfg.User, cursor)
		}

		// push を受信すれば即時ループ、なければ PollInterval 待機 (issue #198)
		pollTimer := time.NewTimer(cfg.PollInterval)
		select {
		case <-ctx.Done():
			pollTimer.Stop()
			// 次ループ先頭の ctx.Done() check で drain が走る
		case <-pushCh:
			pollTimer.Stop()
			slog.Debug("runHubSession: inbox push — immediate GetMessages")
		case <-pollTimer.C:
			// timer fired; already drained — Stop() unnecessary
		}
	}
}

// startupCatchup は bridge 起動時に未読メッセージを処理する。
func startupCatchup(
	ctx context.Context,
	cfg *config,
	client *agenthub.Client,
	runner *codexRunner,
	cursor cursorPos,
	tracker *activityTracker,
	gapTracker *messageGapTracker,
	journal *Journal,
) (cursorPos, error) {
	msgs, err := client.GetMessages(ctx)
	if err != nil {
		slog.Warn("[startup-catchup] get_messages failed; skipping", "err", err)
		return cursor, nil
	}

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

	selfHandle := "@" + cfg.User

	for _, msg := range nlMsgs {
		if msg.Sender == selfHandle {
			// best-effort: 失敗は致命的でなく、次回 polling で再試行される
			_ = client.MarkAsRead(ctx, msg.ID)
			continue
		}

		if cursor.seen(msg) {
			slog.Info("[startup-catchup] skipping seen message",
				"msg_id", msg.ID, "ts", msg.Timestamp, "cursor", cursor.TS)
			// best-effort: 失敗は致命的でなく、次回 polling で再試行される
			_ = client.MarkAsRead(ctx, msg.ID)
			continue
		}

		// issue #336 #4: 内側 codex が先回りで返信済みの inbound は流さない
		if runner.innerReplied.has(msg.ID) {
			slog.Warn("[startup-catchup] skipping already-replied-by-inner-codex message (issue #336)",
				"msg_id", msg.ID, "from", msg.Sender)
			_ = client.MarkAsRead(ctx, msg.ID)
			continue
		}

		gapTracker.onMessageReceived(msg.ID)

		if err := client.MarkAsRead(ctx, msg.ID); err != nil {
			slog.Warn("[startup-catchup] pre-process mark_as_read failed; cursor will guard on retry",
				"msg_id", msg.ID, "err", err)
		}

		handleErr := handleOne(ctx, client, runner, msg, cfg, tracker, journal)
		if handleErr != nil {
			slog.Error("[startup-catchup] handleOne error", "msg_id", msg.ID, "err", handleErr)
		}

		cursor = cursor.advance(msg)
		saveCursor(cfg.User, cursor)
	}

	return cursor, nil
}

// handleOne は message 1 件を codex に流して応答を待つ。
//
// 失敗時の扱い (issue #336、約束 C1・C4):
//   - SubprocessTimeout 到達 (errSubprocessTimeout): リトライせず、送信元へエラーを 1 回返す。
//     リトライすると実際に作業できる時間が上限の数倍になり、「何分まで作業できるか」が決まらなくなる
//   - 内側 codex が処理中の inbound へ返信済み (caused_by=msg.ID が成功): エラーは返さない
//     (返すと 1 依頼に 2 返信になる)
//   - SIGTERM 等で ctx がキャンセル済み: エラーは返さない。キャンセル済み ctx で送ると送信に
//     失敗して journal に残り、次回起動時に replay されて偽エラーになる (bridge-claude2 #240 / #250)
func handleOne(
	ctx context.Context,
	client *agenthub.Client,
	runner *codexRunner,
	msg agenthub.Message,
	cfg *config,
	tracker *activityTracker,
	journal *Journal,
) error {
	if _, err := os.Stat(cfg.Workdir); err != nil {
		slog.Error("handleOne: workdir gone",
			"workdir", cfg.Workdir, "msg_id", msg.ID)
		sendAutoErrorReply(ctx, client, runner, journal, msg,
			fmt.Sprintf("bridge の workdir が存在しません: %s", cfg.Workdir))
		return nil
	}

	slog.Info("← message",
		"msg_id", msg.ID, "from", msg.Sender, "body_preview", truncate(msg.Body, 120))

	prompt := formatPrompt("@"+cfg.User, msg, cfg.GitHubFooter)
	usage, err := runner.query(ctx, prompt, msg.Sender, tracker)
	emitSpan(msg.ID, cfg.Model, usage)
	// issue #336 #4: 内側 codex が先回りで返信した他の inbound を記録する (query の成否に関わらず、
	// 成功を確認できた送信は hub に届いている)
	recordInnerReplied(runner, msg.ID, usage.RepliedCausedBy)
	if err == nil {
		slog.Info("→ message processed", "msg_id", msg.ID, "from", msg.Sender)
		return nil
	}

	if usage.repliedTo(msg.ID) {
		slog.Warn("handleOne: codex failed after replying to this message — suppressing error report "+
			"to avoid a second reply (issue #336)",
			"msg_id", msg.ID, "err", err)
		return err
	}
	if ctx.Err() != nil {
		slog.Info("handleOne: query cancelled by shutdown — suppressing error report (issue #336)",
			"msg_id", msg.ID, "err", err)
		return err
	}

	slog.Error("handleOne: codex query error", "msg_id", msg.ID, "err", err)
	sendAutoErrorReply(ctx, client, runner, journal, msg, fmt.Sprint(err))
	return err
}

// sendAutoErrorReply は送信元へ `autoErrorPrefix + " " + detail` の auto 返信を送る。
// auto 返信の全経路 (codex 失敗 / workdir 不在) はここを通し、echo 抑止と送信元別 cooldown を
// 共通に掛ける (issue #336 #3、autoreply.go 参照)。
func sendAutoErrorReply(
	ctx context.Context,
	client *agenthub.Client,
	runner *codexRunner,
	journal *Journal,
	msg agenthub.Message,
	detail string,
) {
	if isAutoErrorEcho(msg.Body) {
		slog.Warn("sendAutoErrorReply: inbound is an auto reply — suppressing auto error reply (issue #336)",
			"msg_id", msg.ID, "from", msg.Sender)
		return
	}
	if ok, wait := runner.autoReply.allow(msg.Sender, time.Now()); !ok {
		slog.Warn("sendAutoErrorReply: auto error reply suppressed by per-sender cooldown (issue #336)",
			"msg_id", msg.ID, "from", msg.Sender,
			"cooldown_s", fmt.Sprintf("%.0f", autoReplyCooldown.Seconds()),
			"retry_after_s", fmt.Sprintf("%.0f", wait.Seconds()))
		return
	}
	errMsg := fmt.Sprintf("%s %s", autoErrorPrefix, detail)
	_ = journalledSend(ctx, client, journal, msg.Sender, errMsg, msg.ID)
}

// recordInnerReplied は内側 codex が返信した inbound ID を runner.innerReplied に積む。
// 処理中の inbound (currentID) への返信は正規の応答なので除外する。
func recordInnerReplied(runner *codexRunner, currentID string, ids []string) {
	if runner == nil || runner.innerReplied == nil {
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
	runner.innerReplied.add(others...)
	slog.Warn("recordInnerReplied: inner codex replied to other inbound messages — "+
		"they will be skipped on next get_messages (issue #336)",
		"current_msg_id", currentID, "inner_replied_ids", others)
}

// journalledSend は journal write → hub.SendMessage → journal delete の順で送信を永続化する。
func journalledSend(
	ctx context.Context,
	client *agenthub.Client,
	journal *Journal,
	to, message, causedBy string,
) error {
	entry := journal.makeEntry(to, message, causedBy)
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

// replayJournal は起動時に pending journal entries を replay する。
func replayJournal(ctx context.Context, client *agenthub.Client, journal *Journal) {
	entries := journal.loadAll()
	if len(entries) == 0 {
		return
	}
	slog.Warn("replayJournal: pending entries found — replaying", "count", len(entries))
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

// runGracefulDrain は SIGTERM 受信後の graceful drain を実行する。
//
// bridge-claude2 の runGracefulDrain と異なり compact ステップを省略する。
// codex CLI には /compact 相当の機能がないため。
//
// フロー:
//  1. GetMessages で未処理メッセージを確認
//  2. メッセージがあれば処理してから exit
//  3. メッセージがなければそのまま exit
func runGracefulDrain(
	client *agenthub.Client,
	runner *codexRunner,
	cfg *config,
	cursor cursorPos,
	tracker *activityTracker,
	journal *Journal,
	selfHandle string,
) {
	drainTimeout := cfg.SubprocessTimeout + time.Minute
	if drainTimeout <= time.Minute {
		drainTimeout = 6 * time.Minute // 最小バッファ
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	slog.Info("[drain] graceful drain started (no compact — codex bridge)",
		"drain_timeout_s", drainTimeout.Seconds(),
	)

	msgs, err := client.GetMessages(drainCtx)
	if err != nil {
		slog.Warn("[drain] final get_messages failed", "err", err)
		return
	}

	var pending []agenthub.Message
	for _, m := range msgs {
		if m.Sender == selfHandle {
			// best-effort: 失敗は致命的でなく、次回 polling で再試行される
			_ = client.MarkAsRead(drainCtx, m.ID)
			continue
		}
		if cursor.seen(m) {
			// best-effort: 失敗は致命的でなく、次回 polling で再試行される
			_ = client.MarkAsRead(drainCtx, m.ID)
			continue
		}
		// issue #336 #4: 内側 codex が先回りで返信済みの inbound は流さない
		if runner.innerReplied.has(m.ID) {
			slog.Warn("[drain] skipping already-replied-by-inner-codex message (issue #336)",
				"msg_id", m.ID, "from", m.Sender)
			_ = client.MarkAsRead(drainCtx, m.ID)
			continue
		}
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

	slog.Info("[drain] processing pending messages", "count", len(pending))
	for _, msg := range pending {
		if err := client.MarkAsRead(drainCtx, msg.ID); err != nil {
			slog.Warn("[drain] mark_as_read failed", "msg_id", msg.ID, "err", err)
		}
		if err := handleOne(drainCtx, client, runner, msg, cfg, tracker, journal); err != nil {
			slog.Error("[drain] handleOne error", "msg_id", msg.ID, "err", err)
		}
		cursor = cursor.advance(msg)
		saveCursor(cfg.User, cursor)
	}
	slog.Info("[drain] graceful drain completed")
}
