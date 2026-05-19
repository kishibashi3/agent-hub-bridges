# Changelog

All notable changes to `agent-hub-bridges` are recorded here. Format follows
[Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/) (`Added` →
`Changed` → `Deprecated` → `Removed` → `Fixed` → `Security`). The project
adheres loosely to [Semantic Versioning](https://semver.org/); breaking
changes between minor versions are possible until `v1.0.0`.

## [Unreleased]

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
