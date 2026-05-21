# agent-hub-bridges

Monorepo of [agent-hub](https://github.com/kishibashi3/agent-hub) bridge workers.

| extra | bridge | status |
|---|---|---|
| `[claude]` | Stateful Claude bridge (uses Claude Agent SDK) | **M1 ✅** — ported from `agent-hub-bridge-claude` (archived) |
| `[slack]`  | Slack relay bridge (Socket Mode + thread routing)  | **M2 ✅** — ported from `agent-hub-bridge-slack` (archived) |
| `[gemini]` | Stateful Gemini bridge (uses `gemini` CLI)         | **M3 ✅** — ported from `agent-hub-bridge-gemini` (archived) |
| `[a2a]`    | A2A client bridge (no-LLM protocol translator)     | **M4 ✅** — new implementation (spec: [agent-hub#94](https://github.com/kishibashi3/agent-hub/issues/94)) |
| `[all]`    | Install everything                                  | — |
| `[dev]`    | Test + lint toolchain (pytest, ruff)                | — |

## Install

```bash
# install one bridge
pip install "agent-hub-bridges[claude] @ git+https://github.com/kishibashi3/agent-hub-bridges.git"

# install multiple
pip install "agent-hub-bridges[claude,slack] @ git+https://github.com/kishibashi3/agent-hub-bridges.git"

# install all bridges
pip install "agent-hub-bridges[all] @ git+https://github.com/kishibashi3/agent-hub-bridges.git"
```

Each extra brings only the deps that specific bridge needs. The core `agent-hub-sdk`
client + `anyio` + `python-dotenv` is always installed.

## Run

Each bridge ships its own console script. The legacy script names (`agent-hub-bridge-claude`,
`agent-hub-bridge-slack`, `agent-hub-bridge-gemini`, `agent-hub-bridge-a2a`) are kept for
backward compatibility — existing systemd / supervisord units do **not** need to change.

```bash
agent-hub-bridge-claude --user claude-impl --tenant my-tenant --workdir /path/to/project
agent-hub-bridge-slack
agent-hub-bridge-gemini --user gemini-impl --tenant my-tenant --workdir /path/to/project
agent-hub-bridge-a2a --user external-agent
```

Required env (shared by all bridges):

```
AGENT_HUB_URL=http://localhost:3000/mcp
GITHUB_PAT=<your-pat>
```

See `.env.example` for the full list of env vars (including bridge-specific ones).

## Layout

```
src/agent_hub_bridges/
├── __init__.py        # __version__ only — does NOT eager-import sub-packages
├── _common/           # internal helpers shared by all bridges
├── claude/            # M1 port target
├── slack/             # M2 port target
├── gemini/            # M3 port target (+ SDK migration)
└── a2a/               # M4 new bridge
```

Design: [`docs/design.md`](docs/design.md).
Migration guide: [`docs/migration.md`](docs/migration.md).

## Development

```bash
pip install -e ".[dev,all]"
pytest
ruff check src/
```

Issues live at [`kishibashi3/agent-hub-bridges/issues`](https://github.com/kishibashi3/agent-hub-bridges/issues).
Use labels `bridge:<name>` + `type:<kind>` (= `feat` / `bug` / `doc` / `refactor`).

## License

[Apache-2.0](LICENSE).
