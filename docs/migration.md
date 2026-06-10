# Migration: legacy bridge repos → agent-hub-bridges monorepo

> Status: **M5 complete**. M1-M4 全移植完了。`agent-hub-bridge-claude` / `agent-hub-bridge-slack` / `agent-hub-bridge-gemini` は 2026-05-21 に archive 済み。

## TL;DR (ユーザ向け)

旧:
```bash
pip install git+https://github.com/kishibashi3/agent-hub-bridge-claude.git
agent-hub-bridge-claude --user claude-impl --tenant my-tenant
```

新 (M1 以降):
```bash
pip install "agent-hub-bridges[claude] @ git+https://github.com/kishibashi3/agent-hub-bridges.git"
agent-hub-bridge-claude --participant claude-impl --tenant my-tenant
```

CLI 名は **変えない**。ユーザ視点では install 元の URL を差し替えるだけで動く想定。

> **⚠ v0.3.0 breaking change**: `--user` → `--participant`、`AGENT_HUB_USER` → `AGENT_HUB_PARTICIPANT` に変更。deployment script / env file の更新が必要。

## 影響範囲 (operator / deployer)

| 項目 | 変更 | 対応 |
|---|---|---|
| 旧 repo URL | M5 で archive 済み (2026-05-21) | 対応不要 |
| pip install 行 | repo URL + extra 名 が変わる | deployment script 更新 |
| CLI 名 (`agent-hub-bridge-<name>`) | 不変 | なし |
| CLI 引数 (`--participant` etc) | **v0.3.0 で変更** (`--user` → `--participant`) | deployment script 更新要 |
| env vars (`AGENT_HUB_PARTICIPANT` etc) | **v0.3.0 で変更** (`AGENT_HUB_USER` → `AGENT_HUB_PARTICIPANT`) | env file 更新要 |
| systemd unit / supervisord conf | 不変 | なし (install 行のみ) |

## 各 bridge の移植メモ

### bridge-claude → `agent_hub_bridges.claude`

- M_sdk (旧 repo 内) で agent-hub-sdk への 移行済。 monorepo 側に コピーした
  後、 `Config` を `_common.base_config.BaseConfig` ベースに リファクタ。
- 1:1 同等 — ユーザに見える挙動は変わらない。

### bridge-slack → `agent_hub_bridges.slack`

- M5 (旧 repo 内) で agent-hub-sdk へ 移行済。 `ThreadContext` 含めて
  そのまま移植。
- 1:1 同等。

### bridge-gemini → `agent_hub_bridges.gemini`

- M3 で monorepo 移植と同時に SDK 移行完了 (operator DM 質問 C で合意)。
- 旧 repo の手製 `hub.py` (HubClient ~198 LOC) は 移植せず削除。
  `agent_hub_sdk.AgentHub` + `hub.inbox()` に統一。
- `GeminiCLIEngine` (subprocess + 429 retry) は gemini 固有コードとして
  `gemini/engine.py` に残存。挙動は旧 repo と 1:1 同等。

### bridge-a2a → `agent_hub_bridges.a2a` (新規)

- 旧 repo は 存在しない (新規実装)。
- M4 で `agent-hub#94` spec に基づき実装完了。no-LLM A2A client bridge
  として `a2a-sdk 1.0.3` を使用。

## 旧 repo の archive 手順 (M5) — 完了

operator (@ope-ultp1635) により 2026-05-21 に実施済み:

1. 旧 repo の README 冒頭に archive 案内を追記。
2. GitHub UI から repo を archive (= read-only)。
3. 関連 issue / PR への `archived` ラベル付与は軽量運用で対応。

archive 済み repo:
- `kishibashi3/agent-hub-bridge-claude` (M1 source)
- `kishibashi3/agent-hub-bridge-slack`  (M2 source)
- `kishibashi3/agent-hub-bridge-gemini` (M3 source)
