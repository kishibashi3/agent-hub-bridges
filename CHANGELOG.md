# Changelog

All notable changes to `agent-hub-bridges` are recorded here. Format follows
[Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/) (`Added` →
`Changed` → `Deprecated` → `Removed` → `Fixed` → `Security`). The project
adheres loosely to [Semantic Versioning](https://semver.org/); breaking
changes between minor versions are possible until `v1.0.0`.

## [Unreleased]

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
