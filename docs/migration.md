# Migration: legacy bridge repos → agent-hub-bridges monorepo

> Status: **skeleton (M0)**. 各 bridge 移植 PR (M1-M3) で 本 doc を 詳しく
> 埋めていく。

## TL;DR (ユーザ向け)

旧:
```bash
pip install git+https://github.com/kishibashi3/agent-hub-bridge-claude.git
agent-hub-bridge-claude --user claude-impl --tenant my-tenant
```

新 (M1 以降):
```bash
pip install "agent-hub-bridges[claude] @ git+https://github.com/kishibashi3/agent-hub-bridges.git"
agent-hub-bridge-claude --user claude-impl --tenant my-tenant
```

CLI 名 / 引数 / env vars は **変えない**。 ユーザ視点では install 元の URL
を 差し替えるだけで動く想定。

## 影響範囲 (operator / deployer)

| 項目 | 変更 | 対応 |
|---|---|---|
| 旧 repo URL | 残すが M5 で archive | M1-M4 安定確認後 archive |
| pip install 行 | repo URL + extra 名 が変わる | deployment script 更新 |
| CLI 名 (`agent-hub-bridge-<name>`) | 不変 | なし |
| CLI 引数 (`--user` etc) | 不変 | なし |
| env vars (`AGENT_HUB_*`) | 不変 | なし |
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

- 旧 repo は **まだ自前 `hub.py` (HubClient) を 使っている**。 monorepo 移植
  と 同時に SDK へ 切り替える (operator DM 質問 C で合意)。
- 移行ステップ (M3 issue で 詳細化):
  1. SDK ベースに 書き換えた `worker.py` を monorepo 側で 作る。
  2. 旧 repo の `hub.py` (HubClient) は 移植しない。
  3. integration test で 旧版と挙動同等を 確認 (peer に DM → 返信が来る、
     /ping → /pong)。

### bridge-a2a → `agent_hub_bridges.a2a` (新規)

- 旧 repo は 存在しない (新規実装)。
- 仕様は `kishibashi3/agent-hub#94` を 出発点に M4 で 設計を 詰める。

## 旧 repo の archive 手順 (M5)

operator (@ope-ultp1635) 判断で 実行する:

1. 旧 repo の README 冒頭に 「⚠️ This repo is archived. Use
   [agent-hub-bridges](https://github.com/kishibashi3/agent-hub-bridges)
   instead.」 を追記。
2. GitHub UI から repo を archive (= read-only)。
3. 関連 issue / PR の 移動が 必要なら 個別判断 (= label `archived` を 付ける
   などの軽量運用で 済ます方針)。
