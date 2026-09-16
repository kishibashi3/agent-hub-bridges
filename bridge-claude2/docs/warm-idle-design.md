# bridge-claude2: on-demand モード warm idle 設計ドキュメント (issue #252)

- 作成: 2026-07-06
- 作者: @bridges-impl
- ステータス: Draft — operator L1 GO 取得前の設計リサーチ（実装はレビュー後）
- 再確認: 2026-09-16（§10 参照）
- レビュー反映: 2026-09-16 PR #253 @reviewer 指摘 M1〜M3 / S1〜S7 を反映（§3.1・§3.2・§3.4・§4・§10.1・§10.3・§10.4）
- 依存: なし（既存 on-demand runner.go / worker.go の拡張）
- 関連: [stateful-mode-design.md](./stateful-mode-design.md)（session_id ハンドリングの前提知識）

---

## 1. 現状分析

### 1.1 現在の動作（issue #252 が問題視する挙動）

`runner.query()`（runner.go L156-228、2026-09-16 時点の main）は呼び出しごとに:

1. `spawnSubprocess` で `claude` CLI を新規 spawn
2. `control_request(initialize)` → `user` message を stdin に書き込み
3. `result` イベントまで stdout を読む（`readUntilResult`）
4. `stdinPipe.Close()` で EOF を送り、`cmd.Wait()` で subprocess 終了を待つ

= **1 メッセージ = 1 subprocess のライフサイクル**。`handleOne` が呼ばれるたびに claude CLI の起動コスト（プロセス起動・モデル/MCP 初期化等）が毎回発生する。

### 1.2 呼び出し元の並行性

`runHubSession`（worker.go、2026-09-16 時点の main）の push 駆動ループは **単一 goroutine の for-select**。PR #269 以降、`handleOne` の呼び出し元は push 通知・safety-net poll・startup catchup のいずれも `processMessages` に集約されており（graceful drain は `runGracefulDrain` から直接呼ぶ）、全て同一 goroutine 上で走る。同時に処理中の `handleOne` は常に高々 1 件であり、複数メッセージが来ても順番に処理される。つまり「同時実行の並列度」を上げる話ではなく、「**逐次実行される呼び出しの間の cold start を消す**」話である。

### 1.3 session_id の役割（再掲）

`session_id`（現行は `msg.Sender`、例 `"@planner"`。`query(ctx, prompt, sessionID, …)` の `sessionID` 引数）を stdin JSON プロトコルに渡すことで、Claude 側のセッションストレージ（`~/.claude/projects/.../*.jsonl`）から前回文脈を復元している。stateful モードの継続性はこの仕組みで実現済み（[stateful-mode-design.md](./stateful-mode-design.md) 参照）。

---

## 2. 提案: warm idle

タスク（`result` イベント受信）完了後、subprocess を即座に `Close`/`Wait` せず、一定時間（idle timeout）**stdin を開いたまま待機**させる。タイムアウト内に次のメッセージが来れば、新規 spawn せず同一 subprocess に次の `user` message を書き込んで即応答する。タイムアウトを過ぎたら通常どおり `stdinPipe.Close()` + `cmd.Wait()` で正常終了する。

### 2.1 スコープ判断: 単一 warm スロット（sender 単位ではない）

`runHubSession` が単一 goroutine で逐次処理する以上、**同時に生きている warm subprocess は高々 1 つ**で十分（issue の「複数リクエストの同時受付可否」への回答）。並列受付そのものは目的ではなく、連続メッセージのコールドスタート削減が目的のため、warm スロットは `claudeRunner` に 1 つだけ持たせる。

### 2.2 sender 不一致時の扱い（重要な設計判断）

warm subprocess は最初の `user` message で使った `session_id` に紐づいている。**一致判定のキーは `msg.Sender` ではなく、stdin に実際に書いた `session_id`（`query()` の `sessionID` 引数）を使う**。現行は両者が同じ値だが、stateful doc Phase 2 の `sessionOverrideMap`（`/restart` で新 UUID）や `--mode stateless`（毎回 `uuid.New()`）が入ると sender 一致でも session_id が異なりうる（stateless では常に不一致 = 常に cold spawn が正しい挙動）。**次のメッセージの session_id が異なる場合、同一 subprocess に別の session_id を書き込んで安全に動作するかは未検証**（[stateful-mode-design.md](./stateful-mode-design.md) 6章の未解決事項と同種の懸念）。

これを検証せずに実装すると「sender A 向けに warm 化した subprocess に sender B の文脈が漏れる／誤って復元される」というサイレント縮退リスクがある。したがって:

- **MVP は sender 一致時のみ再利用**。次のメッセージの sender が warm subprocess の session_id と一致する場合のみ再利用し、不一致なら warm subprocess を即座に close してから新規 spawn する（idle timeout 満了を待たない）。
- 複数 sender をまたいだ warm 化（sender ごとに warm スロットを持つ pool 化）は、上記の同一プロセス内 session_id 切り替えの検証、またはプロセスを sender 単位で複数保持するリソース増加とのトレードオフが必要なため **見送り（将来検討）**。

この判断により、連続メッセージが同一 sender から来るケース（bot 同士の会話往復、1 人のユーザーとの連続やり取り）でのみ効果が出る。異なる sender が輪番で来るケースでは効果が出ないが、誤動作リスクを避けられる。

---

## 3. アーキテクチャ変更

### 3.1 `claudeRunner` への warm state 追加

```go
type warmProcess struct {
    cmd        *exec.Cmd
    stdinPipe  io.WriteCloser
    scanner    *bufio.Scanner
    sessionID  string             // 紐づいている session_id（§2.2）
    cancel     context.CancelFunc // spawn ctx の cancel = SIGKILL（§10.1）
    idleTimer  *time.Timer        // 満了で closeWarm
    drainDone  chan struct{}      // idle 中の stdout 読み捨て goroutine の終了通知（§3.4）
    exited     bool               // drainDone 経由で EOF を観測済み（idle 中に死んだ）
}

type claudeRunner struct {
    cfg           *config
    mcpConfigPath string
    iatMgr        *githubclient.IATManager
    autoReply     *autoReplyLimiter // issue #267（既存）

    mu   sync.Mutex   // 必須（下記）
    warm *warmProcess // nil = warm subprocess なし
}
```

**`mu` は必須であり「防御的」ではない**（レビュー M3）。warm state は少なくとも 3 つの goroutine から触られる: (1) `query()` を呼ぶ処理 goroutine、(2) `time.AfterFunc` のコールバック（別 goroutine で `closeWarm` を走らせる）、(3) §3.4 の stdout 読み捨て goroutine。`idleTimer.Stop()` が `false` を返した時点で (2) の `closeWarm` は並行実行中（stdin close / Wait の途中）でありうるため、`closeWarm` と再利用判定は同一 lock 下で行い、**`Stop()==false` は「再利用不可・cold spawn」扱い**にする。

### 3.2 `query()` の変更

```
query(ctx, prompt, sessionID, tracker):
    # --- warm slot の取り出し（lock 下で行い、slot は必ず空にする） ---
    r.mu.Lock()
    w := r.warm
    r.warm = nil                       # 以降 r.warm が閉じたプロセスを指し続けないように必ず nil にする
    reuse := false
    if w != nil:
        if w.idleTimer.Stop() && w.sessionID == sessionID && !w.exited:
            stopDrain(w)               # §3.4 の読み捨て goroutine を止め、scanner の所有権を取り戻す
            reuse = true
        else:
            # Stop()==false（closeWarm が並行実行中）/ session_id 不一致 / idle 中に exit 済み
            # → 再利用不可。closeWarm は idempotent にしておき、ここで確実に閉じる
            closeWarm(w)
    r.mu.Unlock()

    if !reuse:
        spawn（独立 ctx、§10.1）+ initialize as today
    else:
        cmd, stdinPipe, scanner = w.cmd, w.stdinPipe, w.scanner   # spawn・initialize をスキップ

    # per-query timeout: queryCtx.Done() を待つ goroutine が w.cancel()（= SIGKILL）を呼ぶ（§10.1）
    write user message
    usage, err := readUntilResult(queryCtx, ...)

    if err == nil && warmIdleEnabled && !inDrain(ctx):
        # stdin を閉じず warm 状態に遷移（drain 中は遷移しない、§3.3）
        r.mu.Lock()
        r.warm = &warmProcess{ cmd, stdinPipe, scanner, sessionID, cancel, ... }
        startDrain(r.warm)             # §3.4
        r.warm.idleTimer = time.AfterFunc(warmIdle, func() { r.mu.Lock(); if r.warm == this { r.warm = nil }; closeWarm(this); r.mu.Unlock() })
        r.mu.Unlock()
    else:
        stdinPipe.Close(); cmd.Wait(); cancel()   # 現状どおり（err != nil を含む）

    return usage, err
```

`closeWarm(w)` の内容: `idleTimer.Stop()`（呼び出し元が未停止なら）→ `stdinPipe.Close()`（EOF）→ `<-w.drainDone`（読み捨て goroutine の EOF 到達 = stdout の read 完了を待つ）→ `cmd.Wait()`（**プロセス 1 つにつき 1 回だけ**）→ `w.cancel()`。stdin close 後も一定時間（例: 数秒）で exit しない場合は `w.cancel()` で SIGKILL してから Wait する。

`initialize control_request` は **subprocess の最初の user message でのみ送信**（warm 再利用時は送らない）。stream-json プロトコルが 1 subprocess = 1 initialize を前提にしているかは Phase 1 実装時に要確認（claude CLI 実装依存）。

### 3.3 shutdown 時の扱い

- `runGracefulDrain`（issue #178）: drain 開始時点で warm subprocess が生きていれば、**idle timeout を待たず即座に close** する。drain 中の `/compact` は別 subprocess を spawn するため、warm subprocess を握ったままにする理由がない。
- **drain 中（`drainCtx` 経由の `handleOne`）は warm 遷移しない**（レビュー M1）。drain 直後に bridge プロセスが exit するため warm を作る意味がなく、`main()` の `defer os.Remove(mcpConfigPath)` と warm subprocess の生存期間が交錯する順序問題も避けられる。drain かどうかは `handleOne` → `query` に渡す引数（または ctx value）で明示する。
- **hub session の reconnect 時に warm を破棄する**（レビュー S5）。`claudeRunner` は `runWorker` の outer loop で reconnect をまたいで共有される単一インスタンスであり、warm subprocess は hub の切断・再接続を生き延びる。subprocess 内の claude が保持する agent-hub MCP セッションが hub 障害後に stale 化しうるため、`runHubSession` の開始時（= 再接続時）に `runner.closeWarm()` を呼ぶ。あわせて runner.go の「状態を持たないため、複数の hub session をまたいで単一インスタンスを共有できる」コメント（worker.go 同旨 2 箇所）は warm 化で前提が崩れるため書き換える（#270 の `autoReply` field で既に半分崩れている）。
- bridge プロセス自体の終了（`main()` return 前）: warm subprocess が残っていれば close する defer を追加。孤児プロセス化を防ぐ。
- `/restart` コマンド: 現状 no-op（on-demand のため）だが、warm 化後は「warm subprocess を破棄して次回 spawn を強制する」動作にする方が意味が通る（sender の文脈リセット要求と warm 再利用が衝突しないように）。

### 3.4 idle 中の stdout 読み捨てと crash 検知（レビュー M2 で改訂）

**`cmd.Wait()` を idle 中に別 goroutine で先に呼ぶ設計は採らない**。Go `os/exec` の仕様上 `StdoutPipe` は「pipe の read が全て完了する前に `Wait` を呼ぶのは誤り」（`Wait` が pipe を閉じるため）であり、再利用後の query 中にプロセスが落ちた瞬間に scanner が `file already closed` になり、`closeWarm` での 2 度目の `Wait()` は "Wait was already called" になる。

代わりに warm 化と同時に **stdout 読み捨て goroutine** を起動する:

- idle 中は専用 goroutine が `scanner.Scan()` を回し続け、届いた行は処理せず捨てる（内容は debug ログに残す。旧 §7.3「idle 中の想定外出力の扱い」はこれで解決 = 無視するがログには残す）。
- `scanner.Scan()==false`（EOF = subprocess exit）を **idle 中の crash 検知**に使い、`w.exited = true` を lock 下でセットして `drainDone` を close する。再利用判定（§3.2）は `exited` を見て cold spawn にフォールバックする。
- 再利用時は `stopDrain(w)` で読み捨て goroutine を止めてから scanner を次 query に渡す（goroutine が `Scan()` でブロック中なので、「次の行を読んだら処理せず handoff する」か「stop 要求を見て抜ける」形にし、取り違えて行を落とさないよう Phase 1 で実装を確認する）。
- `Wait` は `closeWarm` で `<-drainDone` の後に 1 回だけ呼ぶ。

**読み捨てが必須な理由（#266 との関係）**: 前 query の `result` 後に CLI が何か行を出すと scanner に残り、次 query の `readUntilResult` が先頭でそれを消費する。残留行が `result` なら次 query が即完了扱い（返信なし・エラーなし）、残留行が `tool_result` なら `SentMessageConfirmed` が誤って立ち #266 の retry 抑止が誤作動する。idle 中に届いた行を次 query に持ち越さないことが、#266 のフラグの per-query 性（§10.2）を保つ条件になる。

---

## 4. 設定

既存の `--subprocess-timeout` / `AGENT_HUB_SUBPROCESS_TIMEOUT`（`resolveSubprocessTimeout`: flag > env > default、Duration 文字列）と同じ形式に揃える（レビュー S4）:

| flag | 環境変数 | 説明 | デフォルト |
|---|---|---|---|
| `--warm-idle <duration>` | `AGENT_HUB_WARM_IDLE` | warm idle タイムアウト（Go Duration 文字列、例 `90s` / `2m`）。`0` で無効化（=現行の即終了動作） | `0`（デフォルト無効。opt-in） |

- 優先順位: flag（`>=0`、未指定 sentinel は `-1`）> env > `0`。`resolveSubprocessTimeout` と同じ `resolveWarmIdle` を追加する
- **不正値は起動時エラー（fail-fast）**: parse 不能・負の Duration は `resolveMaxQueryRetries` / `resolveSubprocessTimeout` と同様に `main()` で起動失敗にする。runtime fallback はしない
- 未設定 = `0` = 無効は「documented optional default」であり、runtime fallback 禁止方針の例外に該当する

デフォルト無効にする理由: 既存の bridge fleet 全体に挙動変更を強制しないため（CLAUDE.md の「変更着手前の依存性確認」原則）。有効化は flag / 環境変数で明示的に選択する運用とする。

`--subprocess-timeout` との関係: 既存の `SubprocessTimeout` は **1 query の実行時間上限**（クエリ中にキャンセルする）であり、warm idle timeout は **query 完了後の待機時間上限**。両者は独立した時間軸で、混同しないよう命名を分離した（`AGENT_HUB_SUBPROCESS_TIMEOUT` vs `AGENT_HUB_WARM_IDLE`）。

---

## 5. リソース使用量への影響

- warm idle 中の subprocess は claude CLI が常駐する分のメモリを idle timeout の間だけ余分に消費する（現行は 0、warm 化後は最大 idle timeout 分だけプロセス常駐）。
- 「headless 常駐モードの『常に起動している』コストを避けつつ」という issue の目的に沿うよう、**idle timeout はデフォルト無効・opt-in、かつ短時間（数十秒〜数分オーダー）を推奨値とする**。常駐モード（stateful 永続 subprocess）とは異なり、無応答が続けば必ず終了する。
- 複数 bridge インスタンスが同時に warm 状態になった場合の合計メモリ増分は、`idle timeout × 同時稼働 bridge 数` に比例する。operator 向け運用ドキュメントに「warm idle 有効化時のメモリ見積もり」を明記する必要がある（Phase 2 で対応）。

---

## 6. 完了条件との対応

issue #252 の完了条件に対する設計上の対応:

- [ ] アイドルタイムアウト後に正常終了すること → §3.2 の `idleTimer` 満了時 `closeWarm`（`stdinPipe.Close()` + `cmd.Wait()`）で対応
- [ ] タイムアウト内にリクエストが来た場合は即応答すること → §3.2 の再利用パス（sender 一致時、spawn/initialize スキップ）で対応

---

## 7. 未解決事項（実装前に検証すべき事項）

1. claude CLI の stream-json プロトコルが「1 subprocess 内で複数回 `user` message を送る」使い方を正式サポートしているか（Python 版 `ClaudeSDKClient` の persistent 利用と同型のはずだが、Go bridge では初めての利用パターンなので実地検証が要る）
2. `control_request(initialize)` を 2 回目以降スキップして問題ないか（複数 turn 目で re-initialize が必要なケースがないか）
3. ~~idle 中に stdout 側で何らかのイベントが届いた場合の扱い~~ → §3.4 で解決（読み捨て + debug ログ、次 query に持ち越さない）
4. warm subprocess が握ったままの MCP config 一時ファイル（`mcpConfigPath`）のライフサイクルに影響がないか（既存は bridge プロセス全体で 1 つ共有のため恐らく無影響だが確認）

---

## 8. 推奨実装方針（段階的アプローチ）

### Phase 1（MVP・operator L1 GO 必須）
- `--warm-idle` / `AGENT_HUB_WARM_IDLE`（デフォルト `0` = 無効、不正値は fail-fast）を追加（§4）
- §3.2 の session_id 一致時のみ再利用する単一 warm スロットを実装（`mu` 下で slot を取り出す、`Stop()==false` は cold）
- §3.4 の idle 中 stdout 読み捨て goroutine + EOF による exit 検知を実装
- §10.1 の独立 spawn ctx + `queryCtx.Done()` 待ち goroutine による kill を実装
- `runGracefulDrain` / bridge shutdown / reconnect 時に warm subprocess を即座に close する経路、drain 中は warm 遷移しない条件を実装（§3.3）
- §7 の未解決事項 1, 2 を実装時に検証（手動確認 + ログで再利用パスが実際に spawn をスキップしていることを確認）

### Phase 2（運用整備）
- warm idle 有効化時のメモリ見積もりを operator docs に追記
- `/restart` を「warm subprocess 破棄」として意味づけし直す

### 見送り（将来検討）
- sender 単位の warm pool 化（複数 sender を同時に warm 保持）— §2.2 の未検証リスクが解消されるまで見送り

---

## 9. operator L1 GO が必要な理由

`runner.go` の中核 (`query()`) と `worker.go` の shutdown 経路 (`runGracefulDrain`) を変更するため、breaking ではないが挙動変更を伴う（デフォルト無効の opt-in ではあるが）。CLAUDE.md の「変更着手前の依存性確認」に従い、本ドキュメントを設計レビュー対象として提出し、GO 後に実装へ進む。

---

## 10. 2026-09-16 再確認（PR #253 放置期間中の main の変化と本設計への影響）

起票（2026-07-06）から約 2 か月経過したため、main の bridge-claude2 と突き合わせて本設計が有効か再確認した。**結論: 設計の前提（1 query = 1 subprocess / `handleOne` は単一 goroutine から逐次呼ばれる / warm idle 未実装 / `AGENT_HUB_WARM_IDLE` 未使用）は全て現状どおりで、設計は有効**。ただし以下 3 点を Phase 1 実装時の考慮事項として追記する。

### 10.1 subprocess が per-query timeout ctx に束縛されている（§3.2 の補足・重要）

現行 `query()` は `SubprocessTimeout > 0`（既定 30m）の場合、`queryCtx = context.WithTimeout(ctx, SubprocessTimeout)`（`defer cancel()`）で `spawnSubprocess(queryCtx)` → `exec.CommandContext` している。つまり **`SubprocessTimeout > 0` の場合、query の return と同時に subprocess は kill される**（`AGENT_HUB_SUBPROCESS_TIMEOUT=0` = 無制限のときは `queryCtx = ctx` で caller ctx = session ctx / drainCtx に束縛される）。§3.2 の擬似コードのまま「stdin を閉じず warm 状態に遷移」しても、`defer cancel()` で即座に SIGKILL されて warm にならない。

Phase 1 の方針（レビュー M1 / S1 を反映。旧案 (a)「bridge 全体の長寿命 ctx で spawn」は撤回）:

- **spawn は独立 ctx で行う**: `spawnCtx, cancel := context.WithCancel(context.Background())`（runner 所有）で `exec.CommandContext` し、`cancel` を `warmProcess.cancel` として warm state に移譲する。kill は idle timer 満了 / session_id 不一致 / shutdown / reconnect 時に `closeWarm` から明示的に呼ぶ
  - 旧案 (a) の「bridge 全体の長寿命 ctx」= signal ctx で spawn すると graceful drain と衝突する。drain は signal ctx が cancel された**後**に走り（`runHubSession` の `case <-ctx.Done(): runGracefulDrain(...)`）、`runGracefulDrain` は `context.WithTimeout(context.Background(), drainTimeout)` で `drainCtx` を作り直している。signal ctx で spawn した subprocess は drain 中に cancel 済み ctx で即 SIGKILL される
- **per-query timeout の kill トリガーは `queryCtx.Done()` を待つ goroutine にする**: query 開始時に `go func() { select { case <-queryCtx.Done(): w.cancel() case <-queryDone: } }()` の形で、`queryCtx`（`WithTimeout(ctx, SubprocessTimeout)`）が切れたときだけ SIGKILL する。現行の timeout 分類は `queryCtx.Err() == context.DeadlineExceeded` → `errSubprocessTimeout` に依存しているため、`time.AfterFunc` + `cmd.Process.Kill()` の別タイマーで殺すと「Kill 済みだが `queryCtx.Err()` はまだ nil」の窓ができ、EOF が generic エラー扱いになって `handleOne` の retry（`errors.Is(err, errSubprocessTimeout)` のときのみ）がサイレントに消える。`queryCtx.Done()` 経由なら既存の timeout 判定コードをそのまま活かせる
- `SubprocessTimeout == 0`（無制限）のときも caller ctx（session ctx / drainCtx）の cancel で kill されるよう、上記 goroutine は `queryCtx` を（`WithTimeout` を経由しない場合でも）常に監視する

### 10.2 issue #264/#266（PR #266、merged）との整合: timeout retry と warm の関係

`handleOne` は subprocess timeout 時に最大 `MaxQueryRetries` 回 `runner.query` を再実行し、`send_message` の tool_result 成功が確認できた場合はリトライを打ち切る（`SentMessageConfirmed`）。本設計との関係:

- warm 遷移は **`err == nil` の場合のみ**（§3.2 どおり）。timeout / kill / limit 系エラーの場合は現行どおり close + Wait し、warm は作らない → retry は常に cold spawn になる。retry 中に前回の壊れた subprocess を再利用する経路は存在しないため、#266 のロジックに変更は不要
- `SentMessageObserved` / `SentMessageConfirmed` は `readUntilResult` が per-query に返す `queryUsage` の値であり、warm 再利用時は 2 回目以降の query でも `readUntilResult` を呼び直すため、**フラグは query ごとにリセットされる**（warm state に持ち越さない）ことを実装時に確認する

### 10.3 issue #268（PR #269、merged）との整合: limit 休眠中の warm subprocess

spend / session limit 到達時、bridge は reset 時刻まで `GetMessages` を呼ばず休眠する（`limitSleeper`）。limit は次 query の `result is_error` から `detectLimit` で検出され、`query()` のエラーとして返るため 10.2 と同じ理由で warm は作られない。さらにその query は §3.2 どおり warm slot を消費（同 session_id）または破棄（別 session_id）してから走り、`err != nil` で close + Wait する。よって **`limitSleeper.enter()` 時点で warm は既に nil であり、休眠直前の warm subprocess が残るケースは起きない**（レビュー S3。旧記述「理論上ありうる」は誤り）。`enter()` 時に追加の close 経路を置くのは無害だが不要。

### 10.4 影響なしと確認した変更

- PR #257（make install 追加）: ビルド手順のみ、runner / worker に影響なし
- PR #258（github-client push 漏れ修正）: `iatMgr` 初期化のみ、query ライフサイクルに影響なし
- PR #270（issue #267、auto エラー返信の送信元別 cooldown、merged）: `handleOne` のエラー通知直前に `runner.autoReply.allow()` を挿入するのみで subprocess ライフサイクルに触れない。warm（成功経路）とは無干渉 → 影響なし
- `/restart` は依然 no-op（§3.3 の「warm 破棄に意味づけ直す」方針はそのまま有効）

---

*@bridges-impl [bridge-claude2] (operator-supervised · kishibashi3/agent-hub-bridges)*
