# bridge-claude2: on-demand モード warm idle 設計ドキュメント (issue #252)

- 作成: 2026-07-06
- 作者: @bridges-impl
- ステータス: Draft — operator L1 GO 取得前の設計リサーチ（実装はレビュー後）
- 依存: なし（既存 on-demand runner.go / worker.go の拡張）
- 関連: [stateful-mode-design.md](./stateful-mode-design.md)（session_id ハンドリングの前提知識）

---

## 1. 現状分析

### 1.1 現在の動作（issue #252 が問題視する挙動）

`runner.query()`（runner.go L133-201）は呼び出しごとに:

1. `spawnSubprocess` で `claude` CLI を新規 spawn
2. `control_request(initialize)` → `user` message を stdin に書き込み
3. `result` イベントまで stdout を読む（`readUntilResult`）
4. `stdinPipe.Close()` で EOF を送り、`cmd.Wait()` で subprocess 終了を待つ

= **1 メッセージ = 1 subprocess のライフサイクル**。`handleOne` が呼ばれるたびに claude CLI の起動コスト（プロセス起動・モデル/MCP 初期化等）が毎回発生する。

### 1.2 呼び出し元の並行性

`runHubSession`（worker.go L235-299）の push 駆動ループは **単一 goroutine の for-select**。同時に処理中の `handleOne` は常に高々 1 件であり、複数メッセージが来ても順番に処理される。つまり「同時実行の並列度」を上げる話ではなく、「**逐次実行される呼び出しの間の cold start を消す**」話である。

### 1.3 session_id の役割（再掲）

`session_id`（= `msg.Sender`、例 `"@planner"`）を stdin JSON プロトコルに渡すことで、Claude 側のセッションストレージ（`~/.claude/projects/.../*.jsonl`）から前回文脈を復元している。stateful モードの継続性はこの仕組みで実現済み（[stateful-mode-design.md](./stateful-mode-design.md) 参照）。

---

## 2. 提案: warm idle

タスク（`result` イベント受信）完了後、subprocess を即座に `Close`/`Wait` せず、一定時間（idle timeout）**stdin を開いたまま待機**させる。タイムアウト内に次のメッセージが来れば、新規 spawn せず同一 subprocess に次の `user` message を書き込んで即応答する。タイムアウトを過ぎたら通常どおり `stdinPipe.Close()` + `cmd.Wait()` で正常終了する。

### 2.1 スコープ判断: 単一 warm スロット（sender 単位ではない）

`runHubSession` が単一 goroutine で逐次処理する以上、**同時に生きている warm subprocess は高々 1 つ**で十分（issue の「複数リクエストの同時受付可否」への回答）。並列受付そのものは目的ではなく、連続メッセージのコールドスタート削減が目的のため、warm スロットは `claudeRunner` に 1 つだけ持たせる。

### 2.2 sender 不一致時の扱い（重要な設計判断）

warm subprocess は最初の `user` message で使った `session_id`（= sender）に紐づいている。**次のメッセージの sender が異なる場合、同一 subprocess に別の session_id を書き込んで安全に動作するかは未検証**（[stateful-mode-design.md](./stateful-mode-design.md) 6章の未解決事項と同種の懸念）。

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
    sessionID  string        // 紐づいている sender
    idleTimer  *time.Timer   // 満了で自動 close
    exited     chan struct{} // cmd.Wait() 完了通知（idle 中の crash 検知用）
}

type claudeRunner struct {
    cfg           *config
    mcpConfigPath string
    iatMgr        *githubclient.IATManager

    mu   sync.Mutex   // warm へのアクセスは呼び出し元が単一 goroutine のため主に防御的
    warm *warmProcess // nil = warm subprocess なし
}
```

### 3.2 `query()` の変更

```
query(ctx, prompt, sessionID, tracker):
    if r.warm != nil:
        stop r.warm.idleTimer  // 再利用するので idle タイマーは止める
        if r.warm.sessionID == sessionID && r.warm still alive (exited channel が閉じてない):
            reuse r.warm.stdinPipe / scanner  # spawn・initialize をスキップ
        else:
            closeWarm(r.warm)  # 別 sender or 既に死んでいた → 破棄
            r.warm = nil

    if r.warm == nil:
        spawn + initialize as today

    write user message
    usage, err := readUntilResult(...)

    if err == nil && idleTimeoutEnabled:
        # stdin を閉じず warm 状態に遷移
        r.warm = &warmProcess{ cmd, stdinPipe, scanner, sessionID, ... }
        r.warm.idleTimer = time.AfterFunc(idleTimeout, func() { closeWarm 経由で自動終了 })
    else:
        stdinPipe.Close(); cmd.Wait()  # 現状どおり

    return usage, err
```

`initialize control_request` は **subprocess の最初の user message でのみ送信**（warm 再利用時は送らない）。stream-json プロトコルが 1 subprocess = 1 initialize を前提にしているかは Phase 1 実装時に要確認（claude CLI 実装依存）。

### 3.3 shutdown 時の扱い

- `runGracefulDrain`（issue #178）: drain 開始時点で warm subprocess が生きていれば、**idle timeout を待たず即座に close** する。drain 中の `/compact` は別 subprocess を spawn するため、warm subprocess を握ったままにする理由がない。
- bridge プロセス自体の終了（`main()` return 前）: warm subprocess が残っていれば close する defer を追加。孤児プロセス化を防ぐ。
- `/restart` コマンド: 現状 no-op（on-demand のため）だが、warm 化後は「warm subprocess を破棄して次回 spawn を強制する」動作にする方が意味が通る（sender の文脈リセット要求と warm 再利用が衝突しないように）。

### 3.4 crash 検知

idle 中に subprocess が（OOM kill 等で）予期せず終了した場合に気づかず次回再利用しようとして書き込みエラーになるケースに備え、`cmd.Wait()` を待つ goroutine を warm 化と同時に起動し、`exited` channel で終了を通知する。再利用前に `exited` が閉じていないか確認し、閉じていれば warm を破棄して新規 spawn にフォールバックする。

---

## 4. 設定

| 環境変数 | 説明 | デフォルト |
|---|---|---|
| `AGENT_HUB_WARM_IDLE_S` | warm idle タイムアウト秒数。`0` で無効化（=現行の即終了動作） | `0`（デフォルト無効。opt-in） |

デフォルト無効にする理由: 既存の bridge fleet 全体に挙動変更を強制しないため（CLAUDE.md の「変更着手前の依存性確認」原則）。有効化は環境変数で明示的に選択する運用とする。

`--subprocess-timeout` との関係: 既存の `SubprocessTimeout` は **1 query の実行時間上限**（クエリ中にキャンセルする）であり、warm idle timeout は **query 完了後の待機時間上限**。両者は独立した時間軸で、混同しないよう命名を分離した（`AGENT_HUB_SUBPROCESS_TIMEOUT` vs `AGENT_HUB_WARM_IDLE_S`）。

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
3. idle 中に stdout 側で何らかのイベント（想定外の出力）が届いた場合の扱い（無視してよいか、ログに残すべきか）
4. warm subprocess が握ったままの MCP config 一時ファイル（`mcpConfigPath`）のライフサイクルに影響がないか（既存は bridge プロセス全体で 1 つ共有のため恐らく無影響だが確認）

---

## 8. 推奨実装方針（段階的アプローチ）

### Phase 1（MVP・operator L1 GO 必須）
- `AGENT_HUB_WARM_IDLE_S`（デフォルト `0` = 無効）を追加
- §3.2 の sender 一致時のみ再利用する単一 warm スロットを実装
- `runGracefulDrain` / bridge shutdown 時に warm subprocess を即座に close する経路を実装
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

*@bridges-impl [bridge-claude2] (operator-supervised · kishibashi3/agent-hub-bridges)*
