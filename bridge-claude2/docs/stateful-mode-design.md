# bridge-claude2: Stateful Mode 設計ドキュメント (issue #170)

- 作成: 2026-06-07
- 作者: @bridges-go-impl
- ステータス: Draft — operator L1 GO 取得前の設計リサーチ
- 依存: issue #163 (CommandRouter /restart) ✅ 実装済み

---

## 1. 現状分析

### 1.1 現在の動作

bridge-claude2 は「on-demand アーキテクチャ」を採用しており、メッセージごとに `claude` subprocess を spawn して応答後に終了させる。

session の連続性は **JSON stdin プロトコルの `session_id` フィールド** で実現している:

```go
// runner.go L117-121
if err := writeJSON(stdinPipe, map[string]any{
    "type":       "user",
    "session_id": sessionID,   // = msg.Sender (例: "@planner")
    "message":    map[string]any{"role": "user", "content": prompt},
    // ...
}); err != nil { ... }
```

Claude は `session_id` でセッションストレージ（`~/.claude/projects/<workdir>/` 以下の `.jsonl`）を引き当て、**前回の会話コンテキストを復元** する。

#### 重要な発見

現在の Go bridge は **実質的に stateful 動作** している。`session_id = msg.Sender` を固定で渡すことで、同一 sender からの複数メッセージに渡って会話文脈が維持される。これは Python bridge-claude の stateful モードと機能的に同等。

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

**重要**: `--session-id` は UUID 形式を要求する。現在の `session_id = msg.Sender`（例: `"@planner"`）は UUID ではなく、JSON プロトコル経由で渡しているため CLI フラグとは異なるパスで処理される。

---

## 2. 設計上の課題

### 課題 A: stateless モードがセッションをリセットしない

`--mode stateless` を指定しても、session_id が固定のため Claude が前回の会話コンテキストを復元してしまう。真の stateless は「毎回新規セッション」であるべき。

### 課題 B: global モードが未実装

`--mode global` は「全 sender が同一コンテキストを共有」を意図するが、現在は sender ごとに異なる session_id を渡すため global にならない。

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
| `stateful`（デフォルト） | `msg.Sender` 固定 | per-sender で会話継続（現状と同じ） |
| `stateless` | `uuid.New()` 毎回生成 | 毎回新規セッション（前回文脈なし） |
| `global` | `"_global_"` 固定 | 全 sender が同一セッションを共有 |

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
2. `session_id` に UUID でない文字列（例: `"@planner"`）を渡した場合の Claude の正確な挙動（現状動いているが公式仕様か不明）
3. compact 時の session_id ハンドリング（compact は `"_compact_"` を使っているが、stateful モードとの整合性）

---

## 7. operator L1 GO が必要な変更

Phase 1 以降は `runner.go` の中核ロジックを変更するため、**operator L1 GO が必要**。本ドキュメントはその前段の設計リサーチとして位置づける。

---

*@bridges-go-impl [bridge-claude · sonnet-4.6] (operator-supervised · kishibashi3/agent-hub)*
