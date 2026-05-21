# agent-hub-bridges — Monorepo Design

> Status: **M5 complete** — all 4 bridges ported; `agent-hub-bridge-claude` / `agent-hub-bridge-slack` / `agent-hub-bridge-gemini` archived (2026-05-21). `agent-hub-bridges` monorepo is now the sole active implementation source.
> Authors: @bridges-impl with operator @ope-ultp1635.
> Origin: agent-hub DM thread (proposal `45223c10-...`, answers `88c56e68-...`).

## 1. Why monorepo

旧構成では bridge ごとに 別 repo を 立てていた (すべて M5 で archive 済):

- `agent-hub-bridge-claude` — stateful Claude bridge (SDK 移行済) → **archived**
- `agent-hub-bridge-slack`  — Slack relay bridge   (SDK 移行済) → **archived**
- `agent-hub-bridge-gemini` — stateful Gemini bridge (旧 HubClient のまま) → **archived**
- (bridge-a2a は旧 repo なし、 agent-hub#94 spec から 新規実装)

実装上、 3 つは ほぼ同じ CLI / Config / outer reconnect loop / ExceptionGroup
ログ整形を 持っていた。 個別 repo に分散させる利点 (= 独立 release) より、
共通基盤の二重メンテと SDK バージョン揃えの摩擦の方が大きい。

monorepo 化で:

- 共通 boilerplate を `_common/` に 1 度だけ書く。
- `agent-hub-sdk` の version bump を 1 PR で全 bridge に反映。
- 新 bridge (a2a) を 既存パターンに乗せて scaffolding 不要で立てられる。

## 2. Layout

```
agent-hub-bridges/
├── pyproject.toml
├── README.md
├── CHANGELOG.md
├── LICENSE                     (Apache-2.0)
├── .env.example                (全 bridge の env をまとめたテンプレ)
├── .gitignore
├── docs/
│   ├── design.md               (本 doc)
│   └── migration.md            (旧 repo → bridges 移行手順)
├── src/
│   └── agent_hub_bridges/
│       ├── __init__.py         (__version__ のみ、 sub-package を eager import しない)
│       ├── _common/            (内部共通 helper; 外向き API ではない)
│       │   ├── __init__.py
│       │   ├── base_config.py  (env loader + BaseConfig dataclass)
│       │   ├── base_cli.py     (共通 argparse 引数 builder)
│       │   ├── reconnect.py    (outer reconnect loop helper)
│       │   ├── exc.py          (ExceptionGroup の log 整形)
│       │   └── prompt.py       (LLM 系 bridge の peer prompt 整形)
│       ├── claude/             (M1: bridge-claude を移植)
│       ├── slack/              (M2: bridge-slack を移植)
│       ├── gemini/             (M3: bridge-gemini を SDK 移行と同時に移植)
│       └── a2a/                (M4: 新規 — agent-hub#94 spec)
├── tests/
│   ├── common/                 (_common 単体 test)
│   ├── claude/  slack/  gemini/  a2a/
└── .github/workflows/ci.yml    (Python 3.11/3.12 matrix lint + test)
```

## 3. extras_require — package を 分けない理由

最初は 「bridge ごとに独立 package + meta package で extras」 という多 package
案も検討したが、 以下の理由で **single namespace package + extras_require** に
した:

1. **同一 `_common/` を 全 bridge で 直接 import したい**。 多 package だと
   `_common` を 別 package に切り出して 各 bridge から depend させる必要が
   出るが、 名前空間が割れて 互換性 contract が monorepo の外まで漏れる。
2. **release も version も 1 つに揃える**。 「claude bridge は 0.3 で
   slack bridge は 0.1」 みたいな状態は 共通 SDK 依存と相性が悪い。
3. **hatchling の single package + multiple optional-deps は標準的な作法**で、
   pip / uv 双方で素直に動く。 multi-package monorepo は workspace 管理が
   ツール依存で複雑になる。

トレードオフ:

- ❌ ある bridge だけ 別 version で release する自由は無い (= 全部一蓮托生)。
- ✅ `pip install "agent-hub-bridges[slack]"` で 必要な bridge の deps だけ
  入る (= 不要 deps を 引きずらない)。
- ✅ `from agent_hub_bridges._common.reconnect import run_with_reconnect` が
  全 bridge から そのまま使える。

## 4. Eager import 禁止ルール

`agent_hub_bridges/__init__.py` 及び `_common/__init__.py` は **bridge sub-
package を import してはならない**。 理由:

- ユーザが `[slack]` extra だけ 入れた状態で `import agent_hub_bridges` を
  呼んだとき、 `claude` sub-package が import 時に `claude_agent_sdk` を
  要求すると `ImportError` で全停する。
- 各 bridge 内部 (`agent_hub_bridges.claude.worker` 等) は 重い deps を
  自由に import してよいが、 top-level `__init__` から触ってはならない。

console script の entry point は `agent_hub_bridges.claude.cli:main` のように
**bridge sub-package を 経由する形** にしてある。 `[claude]` extra が
入っていない環境で `agent-hub-bridge-claude` を 実行したら `ImportError` が
出るが、 これは 「使うなら必要 extra を 入れろ」 という正しい挙動。

## 5. `_common/` 抽出ポリシー

「全 bridge が 同じ pattern で書いていたもの」 だけ ここに 上げる。 1 つの
bridge にしか出ない pattern は その bridge 内に 留める。

| module | extracted from | 用途 |
|---|---|---|
| `base_config.py` | 全 3 bridge の `Config.from_env_and_args` | env (USER/PAT/URL/TENANT/WORKDIR) loader |
| `base_cli.py` | 全 3 bridge の `_build_parser` | 共通 argparse 引数の builder |
| `reconnect.py` | bridge-claude/gemini の `while True: try _run_hub_session ...` | hub session の outer reconnect loop |
| `exc.py` | 全 3 bridge の `_summarize_exc` | `BaseExceptionGroup` の log 整形 |
| `prompt.py` | bridge-claude/gemini の `_format_prompt` | LLM 系 bridge の peer prompt builder |

ここに **入れないもの**:

- bridge-slack の `ThreadContext` (slack thread bind は他 bridge に出ない)
- bridge-gemini の `GeminiCLIEngine` (subprocess engine は gemini 固有)
- bridge-claude の `_mcp_config_file` (Claude SDK 専用)
- task group 構造 (1-task / 2-task / 3-task が それぞれ違うので 抽象化しない)

## 6. 段階的移行プラン

| milestone | scope | issue | status |
|---|---|---|---|
| **M0** | bootstrap (本 doc) | #1 | ✅ complete |
| **M1** | bridge-claude 移植 + `_common` を実コードで磨く | #3 | ✅ complete |
| **M2** | bridge-slack 移植 (SDK 移行済なのでそのまま) | #6 | ✅ complete |
| **M3** | bridge-gemini 移植 + SDK 移行 (= 旧 hub.py 削除) | #8 | ✅ complete |
| **M4** | bridge-a2a 新規実装 (agent-hub#94 spec) | #12 | ✅ complete |
| **M5** | 旧 repo archive + README に 移行案内 | — (operator 判断) | ✅ complete (2026-05-21) |

各 milestone は **1 PR / 1 reviewer LGTM**。 planner / reviewer の運用は
全社共通ルール (`agent-hub/CLAUDE.md`) に従う。

## 7. Open Questions (合意済を 記録)

operator @ope-ultp1635 への 5 質問 (2026-05-19 DM):

- **A. 旧 repo の扱い**: monorepo 安定後 archive、 並行期間は短く。
- **B. a2a bridge の役割**: 外部 A2A agent への client (= 呼びに行く側)、
  `send_message` への変換が主役。 `agent-hub#94` 参照。
- **C. gemini SDK 移行**: monorepo 移植と 同時に行う (M3)。
- **D. CLI 名**: 後方互換の旧名 (`agent-hub-bridge-<name>`) だけ残す。
  統合 dispatcher は 不要。
- **E. issue 起票先**: `kishibashi3/agent-hub-bridges` repo の issues。
  label は `bridge:<name>` / `type:<kind>` を 使う (operator が事前設定済)。

## 8. Out of scope (= 本 monorepo がやらないこと)

- bridge worker 以外の component (= server / SDK / plugin) は別 repo のまま。
- bridge を JS / Go で書く案。 当面 Python のみ。
- bridge の HA / hot reload / 多重起動。 1 process = 1 bridge instance 前提。
- A2A **server** (= public endpoint を立てて 外部から呼ばれる側)。 agent-hub
  には不要 (issue #94 で議論済)。
