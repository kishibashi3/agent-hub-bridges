# bridge-claude2: Stateful Mode 設計ドキュメント (issue #170)

- 作成: 2026-06-07
- 作者: @bridges-go-impl
- ステータス: Draft — operator L1 GO 取得前の設計リサーチ
- 依存: issue #163 (CommandRouter /restart) ✅ 実装済み

---

## 1. 現状分析

> **訂正 (2026-11-24, #414)**: 初版は「stdin の `session_id` で前回の会話コンテキストが復元され、Go bridge は実質 stateful に動いている」と書いていたが、誤りだった。実測 (#406 issuecomment-7130019308) では、同じ `session_id` で 2 回起動しても 2 回目は 1 回目の内容を覚えておらず、CLI は起動ごとに別の session を作った。`--resume` を付けた対照では内容が返った。§1・§2・§3 の表・§6 の該当箇所はこの結果に合わせて直した。§3 以降の設計案は初版の前提のまま書かれており、文脈を継続させる設計は #338 で扱う。

### 1.1 現在の動作

bridge-claude2 は「on-demand アーキテクチャ」を採用しており、メッセージごとに `claude` subprocess を spawn して応答後に終了させる。

各 query は **JSON stdin プロトコルの `session_id` フィールド** に sender を入れて送っている:

```go
// runner.go L117-121
if err := writeJSON(stdinPipe, map[string]any{
    "type":       "user",
    "session_id": sessionID,   // = msg.Sender (例: "@planner")
    "message":    map[string]any{"role": "user", "content": prompt},
    // ...
}); err != nil { ... }
```

CLI はこの `session_id` でセッションストレージ（`~/.claude/projects/<workdir>/` 以下の `.jsonl`）を引き当てない。起動ごとに新しい UUID の session を作り（`.jsonl` も起動ごとに別ファイルになる）、stdin で渡した `session_id` の値は `.jsonl` に記録されない。このため **前回の会話コンテキストは復元されない**（#406 で実測）。復元には `--resume <session-id>` が要るが、現在の `buildArgs` は渡していない。

#### 重要な発見（訂正済み）

現在の Go bridge は **1 通ごとに文脈ゼロ**（実質 stateless）で動いている。`session_id = msg.Sender` を固定で渡しても、同一 sender の前のメッセージの文脈は持ち越されない。Python bridge-claude の stateful モードとは同等ではない。

### 1.2 `--mode` フラグの現状

`--mode stateful|stateless|global` フラグは **パースされるが動作に影響しない**:

```go
// worker.go L141
if _, err := client.Register(ctx, cfg.DisplayName, cfg.Mode); err != nil { ... }
```

`cfg.Mode` は `Register()` の mode 引数に渡される（デフォルト: `"stateful"`）。しかし claude subprocess の起動方法は mode によって変わらない。

### 1.3 Claude CLI のセッション管理機能

`claude --help` で確認した関連フラグ:

| フラグ | 説明 |
|---|---|
| `-r, --resume [session-id]` | セッション ID でセッションを再開 |
| `--session-id <uuid>` | セッション ID を明示指定（**UUID 形式必須**） |
| `-c, --continue` | カレントディレクトリの直近セッションを継続 |
| `--no-session-persistence` | セッション永続化を無効化（`--print` 専用） |
| `--fork-session` | resume 時に新しいセッション ID を生成 |

**重要**: `--session-id` は UUID 形式を要求する。現在の `session_id = msg.Sender`（例: `"@planner"`）は UUID ではなく、JSON プロトコル経由で渡しても CLI のセッション管理には使われない（§1.1、#406）。

---

## 2. 設計上の課題

### 課題 A: stateful モードが実現されていない（初版: stateless モードがセッションをリセットしない）

初版は「`--mode stateless` でも session_id が固定のため前回の会話コンテキストが復元されてしまう」としていたが、実測 (#406) では復元されない。現状は mode によらず毎回新規セッションなので、`stateless` の動作は結果として満たされており、逆に `stateful`（デフォルト）の会話継続が実現されていない。

### 課題 B: global モードが未実装

`--mode global` は「全 sender が同一コンテキストを共有」を意図するが、現在は全 sender とも 1 通ごとに文脈ゼロなので global にならない。

### 課題 C: /restart が no-op

現在の `runner.restart()` は on-demand モードを理由に何もしない。しかし stateful モードでは「当該 sender のセッションコンテキストをリセットする」実装が必要。

### 課題 D: UUID 変換

`--session-id <uuid>` CLI フラグを使う場合、sender 文字列 (`"@planner"`) を deterministic UUID に変換する必要がある（UUID v5 + 固定 namespace）。ただし現在は JSON プロトコル経由で string のまま渡せているため、この課題は CLI フラグ使用時のみ発生。

---

## 3. 設計案

### 案 A: mode-aware session_id 制御（推奨）

**session_id の渡し方を `--mode` に応じて変える**。Claude CLI の JSON プロトコル経由なので UUID 変換不要。

| mode | session_id に渡す値 | 動作 |
|---|---|---|
| `stateful`（デフォルト） | `msg.Sender` 固定 | per-sender で会話継続 |
| `stateless` | `uuid.New()` 毎回生成 | 毎回新規セッション（前回文脈なし） |
| `global` | `"_global_"` 固定 | 全 sender が同一セッションを共有 |

> **注 (#414)**: 実測 (#406) では stdin の `session_id` は CLI のセッション管理に使われないため、この表の `stateful` / `global` 行は session_id の値を変えるだけでは実現しない（3 行とも実際の動作は「毎回新規セッション」になる）。継続させるには案 B の `--resume` のような CLI 側の指定が要る。

**実装規模**: 小規模。runner.go の `query()` に `cfg.Mode` を参照する分岐を追加するだけ。

```go
// runner.go (案 A の実装イメージ)
func (r *claudeRunner) sessionIDFor(sender string) string {
    switch r.cfg.Mode {
    case "stateless":
        return uuid.New().String() // 毎回新規
    case "global":
        return "_global_"          // 全 sender 共有
    default: // "stateful"
        return sender              // per-sender 継続
    }
}
```

**メリット**: 実装が単純、UUID 変換不要、Python bridge と概念的に同等。  
**デメリット**: `_global_` が有効な session_id かどうかは Claude の内部実装次第（要検証）。

### 案 B: --resume CLI フラグ活用

`session_id` を JSON プロトコルではなく CLI フラグ `--resume <session-id>` で渡す。sender → UUID v5 変換が必要。

```go
// UUID v5 変換: sender "@planner" → 安定した UUID
import "github.com/google/uuid"
var ns = uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8") // DNS namespace
func senderToUUID(sender string) string {
    return uuid.NewSHA1(ns, []byte(sender)).String()
}
```

**メリット**: Claude の公式 API を使う。  
**デメリット**: UUID 変換ライブラリの追加依存、既存の JSON プロトコル session_id との混在が複雑。

### 案 C: Python bridge との役割分担（最小変更）

stateful/global は Python bridge-claude（Claude Agent SDK 使用）が担当し、Go bridge は stateless に特化する分業案。

**メリット**: Go bridge の変更が最小。  
**デメリット**: 6/15 移行後に Python bridge を廃止する方向と矛盾する。移行完了後の bridge fleet が Go のみになる場合は選択不可。

---

## 4. /restart のより良い実装

案 A を採用した場合の `/restart` 実装:

```go
// runner.go
func (r *claudeRunner) restart(_ context.Context) error {
    switch r.cfg.Mode {
    case "stateful":
        // TODO: sender の session_id を変える方法が必要
        // → restartMap[sender] = uuid.New() でセッション切り替え
        slog.Info("runner: /restart — clearing session context for sender")
    default:
        slog.Info("runner: /restart — no persistent session (stateless/global)")
    }
    return nil
}
```

stateful モードでの `/restart` は「次のメッセージから新しい session_id を使う」ことでコンテキストを切断できる。`claudeRunner` に `sessionOverride map[string]string` フィールドを追加し、`/restart` 時に sender の override を設定する。

---

## 5. 推奨実装方針

**段階的アプローチ**:

### Phase 1（小規模・L0 相当）
- `--mode stateless` 時のみ session_id をランダム UUID に変更
- `--mode global` は `"_global_"` を渡してみて Claude 側の挙動を確認

### Phase 2（/restart 対応）
- `claudeRunner` に `sessionOverrideMap map[string]string` を追加
- `/restart` 受信時に該当 sender の override に新規 UUID をセット
- 次回 query で override が存在すれば override の UUID を使う

### 見送り（将来検討）
- `--resume` CLI フラグへの移行（UUID v5 変換が必要で複雑さが増す）
- 案 C Python bridge 分業（6/15 移行後の方針による）

---

## 6. 未解決事項

1. `--mode global` で `session_id = "_global_"` を渡した場合に Claude が有効なセッションとして扱うかどうか未検証
2. `session_id` に UUID でない文字列（例: `"@planner"`）を渡した場合の Claude の正確な挙動（#406 で確認: エラーにはならないが、CLI はこの値を使わず起動ごとに新しい session を作る。前回文脈は復元されない）
3. compact 時の session_id ハンドリング（compact は `"_compact_"` を使っているが、stateful モードとの整合性）

---

## 7. operator L1 GO が必要な変更

Phase 1 以降は `runner.go` の中核ロジックを変更するため、**operator L1 GO が必要**。本ドキュメントはその前段の設計リサーチとして位置づける。

---

*@bridges-go-impl [bridge-claude · sonnet-4.6] (operator-supervised · kishibashi3/agent-hub)*
