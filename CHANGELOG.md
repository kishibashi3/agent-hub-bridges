# Changelog

All notable changes to `agent-hub-bridges` are recorded here. Format follows
[Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/) (`Added` →
`Changed` → `Deprecated` → `Removed` → `Fixed` → `Security`). The project
adheres loosely to [Semantic Versioning](https://semver.org/); breaking
changes between minor versions are possible until `v1.0.0`.

## [Unreleased]

### Changed — agent-hub-sdk pin bump v0.6.0 → v0.7.0 (inbox dedup fix)

- `pyproject.toml`: bumped `agent-hub-sdk @ ...@v0.6.0` to `@v0.7.0`.
  v0.7.0 adds **inbox message dedup by ID** (`agent-hub-sdk` issue #31): the
  `AgentHub.inbox()` iterator tracks `in_flight_ids` per session and silently
  discards any message whose ID has already been seen, preventing
  double-dispatch on SSE replay after reconnect. Combined with server PR #118
  (SSE replay suppression at the event-store layer), this resolves the inbox
  storm regression triggered by repeated reconnect cycles.
- Backward compatible: no bridge code changes required. Bridges that relied on
  downstream dedup logic still work unchanged; SDK-level dedup is now the
  authoritative first filter.
- `pyproject.toml` comment updated to reflect v0.7.0 and the inbox dedup
  rationale.

### Changed — M5: 旧 repo archive 完了 (2026-05-21, operator @ope-ultp1635)

- `agent-hub-bridge-claude`, `agent-hub-bridge-slack`, `agent-hub-bridge-gemini`
  の 3 repo が GitHub archive 済み。`agent-hub-bridges` monorepo が唯一の
  active 実装元になった。
- `README.md`: bridge status 表を `M0 stub` から実際の完了状態 (M1-M4 complete)
  に更新。Layout 欄も同様。
- `docs/design.md`: project status を `M0 (bootstrap)` → `M5 complete` に更新。
  milestone 表に status 列追加 (M0-M5 全て ✅ complete)。旧 repo 説明に
  「archived」 注記追加。
- `docs/migration.md`: status を `M0 skeleton` → `M5 complete` に更新。archive
  完了済み旧 repo 一覧を追記。

### Changed — agent-hub-sdk pin bump v0.3.0 → v0.6.0 (issue #27, catch-up prerequisite)

- `pyproject.toml`: bumped `agent-hub-sdk @ ...@v0.3.0` to `@v0.6.0`. All
  bridge sub-packages now resolve against the SDK release that includes
  **M3 stateless + `hub.one_shot()`** (v0.4.0), **M4 TS port + install
  path fix** (v0.5.0), **M5 auto-register on `AgentHub.connect()`**
  (v0.6.0/M5), and **M6 `/restart` built-in + `set_restart_handler`**
  (v0.6.0/M6).
- This is a **prerequisite-only refactor**: no functional change in this
  PR. v0.3.0 → v0.6.0 is backward compatible — existing explicit
  `await hub.register()` calls in monorepo bridges remain harmless (the
  server-side `register` tool is idempotent), and the new `/restart`
  built-in is dormant when no `set_restart_handler` callback is
  registered.
- Motivation: catch-up port of post-`agent-hub-bridges#5` PRs that
  landed in legacy `agent-hub-bridge-claude` (PRs #7, #9, #10) and
  `agent-hub-bridge-slack` (PR #10) before the planned 2026-05-22
  archive. Those follow-up PRs require SDK M5 (drop redundant
  `hub.register()`) and M6 (`/restart` integration); see issue #27 for
  the dependency table.
- Subsequent catch-up PRs land separately:
  - `@bridges-impl`: bridge-slack `drop hub.register()` catch-up
  - `@bridge-claude-impl`: bridge-claude PR #7 (drop `hub.register()`),
    PR #9 (`/restart` + `ClaudeRunner`), PR #10 (Sonnet 4.6 model pin)

### Added — M4 bridge-a2a new implementation (issue #12, agent-hub#94 spec)

- `src/agent_hub_bridges/a2a/` is **new** (not a port) — a no-LLM A2A
  client bridge that fronts an external Agent2Agent agent to agent-hub
  as a peer. Per `kishibashi3/agent-hub#94` spec: pure protocol
  translator, no LLM engine, single-endpoint, scheduler-like structure.
- SDK selection: investigated PyPI candidates with operator approval
  (DM `8d540c65-...`). `a2a-python` 0.0.1 is an empty placeholder
  (Luke Hinds's name reservation); the correct package is **`a2a-sdk`
  1.0.3** — Google LLC official, Apache-2.0, repo `a2aproject/a2a-python`.
  Pinned in `[a2a]` extra as `a2a-sdk>=1.0.3,<2` + `httpx>=0.27` (the
  SDK's transport).
- Bridge flow:
  1. Start with `A2A_AGENT_URL` env (required) — single endpoint per
     `kishibashi3/agent-hub#94`; multi-endpoint is future scope.
  2. `A2ACardResolver(httpx_client, base_url, agent_card_path)` →
     `get_agent_card()` to fetch the agent's Agent Card.
  3. Open `a2a.client.create_client(card, ClientConfig(httpx_client=...))`.
  4. Register on agent-hub with `display_name` derived from
     `card.description` > `card.name` > `--user` fallback (= the new
     `_derive_display_name` helper in worker.py).
  5. Loop `hub.inbox()` — for each incoming hub message:
     - Build `SendMessageRequest(message=Message(role=ROLE_USER,
       parts=[Part(text=msg.body)], message_id=<uuid>))`.
     - Stream-iterate `Client.send_message(request)` and collect
       `StreamResponse` chunks.
     - Concatenate `response.message.parts[*].text` via the new
       `_extract_reply_text` helper. Non-text parts (raw / url / data /
       file) are dropped but a `_(non-text parts omitted: N)_` note is
       appended for ops visibility.
     - `hub.send(to=msg.sender, message=reply_text)` then `hub.ack`.
  6. On `Client.send_message` exception, fall back to a single
     `(自動応答) A2A agent でエラー: ...` notification to the sender so
     failures don't silently disappear.
- Refactored to use `_common/` helpers (same pattern as slack):
  - `BaseConfig` + `load_base_config` + `load_required_env` /
    `load_optional_env`; a2a `Config` adds `a2a_agent_url` (required) +
    `a2a_agent_card_path` (default `/.well-known/agent.json`) and
    inherits `workdir` as None (relay bridge).
  - `build_common_parser` + a2a-specific `--user` (optional, default
    fallback `'a2a-agent'`, env `AGENT_HUB_USER` middle tier — same as
    slack since both are workspace-singleton relays).
  - `run_with_reconnect` for outer reconnect — single-task lifecycle
    (claude/gemini-shaped, not the 3-task structure of slack).
  - `format_peer_message_prompt` is **not** used (no LLM, plain body
    forwarded verbatim).
- `tests/a2a/` (3 files, 26 cases): `test_config.py` (7 cases — env
  resolution, missing required env, card path override, frozen
  dataclass, display/tenant propagation), `test_cli.py` (7 cases —
  `--version`, `--user` default/env/cli precedence, missing
  `A2A_AGENT_URL`, missing `AGENT_HUB_URL`, `KeyboardInterrupt` exit
  130), `test_mapping.py` (12 cases — `_extract_reply_text` for
  single/multi/empty chunks, no-message field, non-text parts handling;
  `_build_send_message_request` minimal/distinct-ids/empty-body;
  `_derive_display_name` description/name/fallback precedence).
- `.env.example`: `A2A_AGENT_URL` + optional `A2A_AGENT_CARD_PATH`
  documented.
- M0 stub at `a2a/cli.py` removed.

**Note on live verification**: this PR ships unit tests + mocks only.
Integration testing against a real A2A agent endpoint requires a
public/staging A2A-compliant agent (operator follow-up after merge).

### Added — M3 bridge-gemini port + SDK migration + Protocol cleanup (issue #8)

- `src/agent_hub_bridges/gemini/` ports `agent-hub-bridge-gemini` (~1052
  LOC) **with the SDK migration done in the same PR** (operator-approved
  scope, DM `4556116c-...`). Behaviour is 1:1 with the legacy repo at the
  CLI / env / console-script level — same `--user` (required) /
  `--model` (gemini-specific) / `--display-name` / `--tenant` /
  `--workdir`, same env (`GEMINI_API_KEY`, `GEMINI_MODEL`,
  `GEMINI_CLI_PATH`, `GEMINI_CLI_TIMEOUT_S`, `GEMINI_MAX_RETRIES`,
  `GEMINI_BACKOFF_BASE_S`, `GEMINI_BACKOFF_CAP_S`, `AGENT_HUB_URL`,
  `GITHUB_PAT`), same console script name (`agent-hub-bridge-gemini`),
  same per-peer `gemini --session-id` mapping, same 429 retry/backoff
  semantics.
- **Dropped the legacy `hub.py` (= self-rolled `HubClient`, ~198 LOC)**.
  `worker.py` now uses `agent_hub_sdk.AgentHub` + `hub.inbox()` like
  bridge-claude / bridge-slack — the hand-rolled push/poll/heartbeat
  task-group is replaced by a single `async for msg in messages:` loop.
  The legacy `IncomingMessage` dataclass from `hub.py` is replaced by
  `agent_hub_sdk.IncomingMessage` everywhere.
- **Dropped `_IncomingMessageLike` Protocol from `_common/prompt.py`**
  (Minor 2 from PR #2 review). With all four bridges now on the SDK,
  the structural typing escape hatch is no longer needed —
  `format_peer_message_prompt` now takes `agent_hub_sdk.IncomingMessage`
  directly. `tests/common/test_smoke.py` swapped its `_FakeMessage`
  dataclass for a real `IncomingMessage` constructor.
- Refactored to use `_common/` helpers (same pattern as M1):
  - `BaseConfig` + `load_base_config` + `load_required_env` /
    `load_optional_env`; gemini `Config` adds `gemini_api_key`,
    `gemini_model`, `gemini_cli_path` and narrows `workdir` to
    required.
  - `build_common_parser` + gemini-specific `--user` (required) +
    `--model` (optional, env `GEMINI_MODEL` fallback, default
    `gemini-2.5-flash`).
  - `run_with_reconnect` for outer reconnect — gemini is now in the
    same single-task lifecycle as claude (= the legacy 2-task
    `_inbox_push_loop` + `_heartbeat_loop` collapses into one
    `async for` since the SDK handles both internally).
  - `format_peer_message_prompt` is reused for the prompt preamble;
    gemini adds its own "DM の sender に返せ / team broadcast 避けろ"
    suffix on top.
  - `summarize_exc` is used transitively via `run_with_reconnect`.
- gemini-specific code that stays in `gemini/`: `engine.py` (= 466 LOC
  `GeminiCLIEngine` — subprocess management, isolated HOME with
  per-bridge `.gemini/settings.json` for MCP config, 429 rate-limit
  detection + retry with `retryDelay` parsing + exponential backoff).
  Verbatim port, only `agent_hub_bridge_gemini` → `agent_hub_bridges.gemini`
  rename + a B904 fix (`raise ... from err` in the timeout path).
- `tests/gemini/` (3 existing + 1 new = 4 files): `test_config.py` (8
  cases, updated to match new fail-fast error format from
  `load_required_env`), `test_engine_retry.py` (24 cases, verbatim
  port), `test_engine_settings.py` (5 cases, verbatim port), and
  **new** `test_cli.py` (8 cases for parity with claude/slack:
  `--version`, `--user` required, missing env, `GEMINI_API_KEY`
  missing, happy-path `run_worker` invocation, `--model` env
  fallback, `KeyboardInterrupt` exit code 130).
- `tests/common/test_smoke.py`: 3 prompt tests updated to construct
  `IncomingMessage` directly (= Protocol removal side effect).
- M0 stub at `gemini/cli.py` removed.

### Added — M2 bridge-slack port (issue #6)

- `src/agent_hub_bridges/slack/` ports the M5_sdk-state of
  `agent-hub-bridge-slack` (~1336 LOC). Behaviour is 1:1 with the legacy
  repo — same CLI args (`--user` optional, default `slack-bot` with
  `AGENT_HUB_USER` env fallback), same env (`SLACK_BOT_TOKEN`,
  `SLACK_APP_TOKEN`, `SLACK_DEFAULT_CHANNEL`, `AGENT_HUB_URL`,
  `GITHUB_PAT`), same console script name (`agent-hub-bridge-slack`),
  same 3-task TaskGroup structure (slack handler / hub→slack relay /
  periodic resubscribe), same `ThreadContext` shared between Slack and
  hub sides, same M4 rate-limit-retry / error-visibility behaviour.
- `routing.py` (426 LOC), `slack_handler.py` (588 LOC), `worker.py` (151
  LOC) ported verbatim — only `agent_hub_bridge_slack` → `agent_hub_bridges.slack`
  rename. The 3-task structure (not the outer reconnect of claude/gemini)
  is kept inside `slack/`, intentionally not using
  `_common.reconnect.run_with_reconnect` — slack binds its 3 tasks to a
  single hub session lifetime by design.
- Refactored to use `_common/` helpers:
  - `BaseConfig` + `load_base_config` + `load_required_env` /
    `load_optional_env` for shared env loading; slack `Config` adds
    `slack_bot_token` / `slack_app_token` / `slack_default_channel` and
    inherits `workdir` as None.
  - `build_common_parser` for shared argparse args; only `--user`
    (optional, default `slack-bot`) is added in slack-specific CLI.
  - The `--workdir` arg accepted by the common parser is silently
    ignored by slack (backward compat for legacy systemd units that
    passed it).
- All 7 legacy slack tests ported (~118 cases): `test_routing.py`,
  `test_slack_handler.py`, `test_hub_to_slack.py`, `test_error_paths.py`,
  `test_thread_follow_up.py`, `test_resubscribe.py`,
  `test_list_participants.py`. Only mechanical changes: `agent_hub_bridge_slack`
  → `agent_hub_bridges.slack` import rename, and `_make_config()` helper
  fixtures gain `workdir=None` (= base inheritance).
- New slack-specific tests for parity with claude (`tests/slack/test_config.py`
  7 cases + `tests/slack/test_cli.py` 8 cases): env resolution, missing
  required env (`SLACK_BOT_TOKEN`/`SLACK_APP_TOKEN`/`AGENT_HUB_URL`/`GITHUB_PAT`),
  `--user` default-/env-/cli- resolution order, `--version` output,
  `KeyboardInterrupt` exit code 130, `--workdir` silently ignored.
- `pyproject.toml`: `tests/slack/**` per-file-ignores added for `N802`
  (mocks of Slack SDK's camelCase `chat_postMessage`), `N818` (test
  sentinel exceptions `_LoopExit` / `_Exc`), `RUF003` (full-width `＝`
  in legacy Japanese comments) — these are pre-existing legacy test
  patterns we intentionally do not rewrite during a 1:1 port.
- M0 stub at `slack/cli.py` (= "M0 stub. Real implementation lands in M2")
  removed.

### Added — M1 bridge-claude port (issue #3)

- `src/agent_hub_bridges/claude/` ports the M_sdk-state of
  `agent-hub-bridge-claude` (~437 LOC, agent-hub-sdk-based). Behaviour is
  1:1 with the legacy repo — same CLI args (`--user` required,
  `--display-name` / `--tenant` / `--workdir` optional), same env (`GITHUB_PAT`,
  `AGENT_HUB_URL`, `ANTHROPIC_API_KEY`), same console script name
  (`agent-hub-bridge-claude`), same Claude Agent SDK options
  (`bypassPermissions`, `setting_sources=["project", "local"]`), same
  per-peer `session_id` (= per-sender stateful context).
- Refactored to use `_common/` helpers:
  - `BaseConfig` + `load_base_config` for shared env loading; claude `Config`
    adds only `anthropic_api_key` and narrows `workdir` to required.
  - `build_common_parser` for shared argparse args; only `--user` (required)
    is added in claude-specific CLI.
  - `run_with_reconnect` replaces the hand-rolled outer `while True: try
    _run_hub_session ...` loop.
  - `format_peer_message_prompt` replaces claude's private `_format_prompt`.
  - `summarize_exc` (transitively used by `run_with_reconnect`) replaces
    claude's private `_summarize_exc`.
- claude-specific code that stays in `claude/` (= not extracted): the
  `_mcp_config_file` temp-file MCP config builder (= Claude SDK calls
  agent-hub via this file path so the PAT never appears in `ps`),
  `_build_options` (Claude Agent SDK options), `_format_message` (SDK
  message → log-line formatter).
- `tests/claude/` (15 new tests): `test_config.py` covers env resolution,
  CLI arg / env precedence, missing required env, bad workdir, frozen
  dataclass; `test_cli.py` covers `--version`, missing `--user`, missing
  env, happy-path `run_worker` invocation, and `KeyboardInterrupt` exit
  code 130.
- `tests/common/` strengthened (Suggestion 3 from PR #2 review):
  `test_base_config.py` (16 new tests) covers `load_required_env` /
  `load_optional_env` empty-string semantics, env override precedence,
  workdir resolution; `test_reconnect.py` (4 new tests) covers retry,
  `KeyboardInterrupt` propagation, `CancelledError` propagation,
  `BaseExceptionGroup` handling.
- M0 stub at `claude/cli.py` (= "M0 stub. Real implementation lands in M1")
  removed.

### Changed — SDK pin swap (issue #15 on agent-hub-sdk)

- `pyproject.toml`: swapped `agent-hub-sdk @ ...@f63a80e` (commit SHA) for
  `@v0.3.0` (annotated tag). The tag was created post-M0 by @sdk-impl
  (2026-05-19T22:05Z) and dereferences to the same commit `f63a80e`, so the
  resolved package is byte-identical — this is a discoverability-only swap.
  TODO comment removed since the trigger condition is satisfied.

### Added — M0 monorepo bootstrap (issue #1)

- `pyproject.toml` (hatchling) with extras `[claude]` / `[slack]` / `[gemini]`
  / `[a2a]` / `[all]` / `[dev]`. Core deps pinned to `agent-hub-sdk @
  git+...@v0.3.0`, `anyio>=4.0`, `python-dotenv>=1.0`.
- `src/agent_hub_bridges/` namespace package. Top-level `__init__.py` exposes
  `__version__` only — does **not** eager-import sub-packages, so installing
  one extra (e.g. `[slack]`) without others does not break `import
  agent_hub_bridges`.
- `_common/` internal helpers extracted from the 3 legacy bridges:
  - `base_config.BaseConfig` + `load_base_config()` — env (USER / PAT / URL
    / TENANT / WORKDIR) loader with fail-fast on missing required env.
  - `base_cli.build_common_parser()` — shared argparse args (`--display-name`,
    `--tenant`, `--workdir`, `--version`). `--user` left to each bridge since
    semantics differ (required vs default).
  - `reconnect.run_with_reconnect()` — outer `while True: try session ...`
    loop with backoff. Replaces the hand-rolled pattern in
    bridge-claude/bridge-gemini.
  - `exc.summarize_exc()` — 1-line repr for `BaseExceptionGroup` log output.
  - `prompt.format_peer_message_prompt()` — LLM-bridge peer message → user
    prompt formatter (used by claude + gemini, not by slack).
- `claude/` / `slack/` / `gemini/` / `a2a/` sub-packages with **stub CLI
  entry points** that print "M0 stub — real impl in MX" and exit 1. Real
  implementations land in M1-M4.
- Backward-compatible console scripts: `agent-hub-bridge-claude`,
  `agent-hub-bridge-slack`, `agent-hub-bridge-gemini`, `agent-hub-bridge-a2a`.
  Legacy systemd / supervisord units do not need to change.
- `docs/design.md` — monorepo rationale, layout, extras_require trade-offs,
  `_common/` extraction policy, milestone plan (M0 → M5).
- `docs/migration.md` — skeleton; each bridge port PR (M1-M3) will flesh
  out the bridge-specific notes.
- `.env.example` — combined env template across all bridges.
- `.github/workflows/ci.yml` — Python 3.11 + 3.12 matrix lint + test.
- `tests/common/` — smoke tests for `summarize_exc`, `format_peer_message_prompt`,
  `BaseConfig` loader.
- `README.md`, `LICENSE` (Apache-2.0), this `CHANGELOG.md`.

### Notes

- No PyPI publishing. Install via `pip install "agent-hub-bridges[<extra>] @
  git+..."`.
- Public API surface is intentionally just `__version__` plus per-bridge CLI
  entry points. The `_common/` package is internal (leading `_`).
- Each milestone (M1-M5) is one PR. Reviewer LGTM gates merge. Operator
  approval required for any breaking change (= legacy CLI removal etc).
