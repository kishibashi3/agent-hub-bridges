# agent-hub-bridges

Monorepo of [agent-hub](https://github.com/kishibashi3/agent-hub) bridge workers.

| extra | bridge | status |
|---|---|---|
| `bridge-claude2/` *(Go)* | Stateful Claude bridge (Go-native, replaces Python `[claude]`) — see [`bridge-claude2/README.md`](bridge-claude2/README.md) | **stable** — `cd bridge-claude2 && make build` |
| `[claude]` | Stateful Claude bridge (uses Claude Agent SDK) **⚠️ Deprecated** — migrate to `bridge-claude2` (Go, in `bridge-claude2/`) | ~~M1~~ **⚠️ Deprecated — target removal `v1.0.0`** (issue #254) |
| `[slack]`  | Slack relay bridge (Socket Mode + thread routing)  | **M2 ✅** — ported from `agent-hub-bridge-slack` (archived) |
| `[gemini]` | Stateful Gemini bridge (uses `gemini` CLI)         | **M3 ✅** — ported from `agent-hub-bridge-gemini` (archived) |
| `[a2a]`    | A2A client bridge (no-LLM protocol translator)     | **M4 ✅** — new implementation (spec: [agent-hub#94](https://github.com/kishibashi3/agent-hub/issues/94)) |
| `bridge-codex2/` *(Go)* | Stateful Codex bridge (Go-native, resident — canonical replacement for Python `[codex]`) — see `bridge-codex2/` | **stable** |
| `[codex]`  | Stateful Codex bridge (Python resident, uses `codex` CLI) **⚠️ Deprecated** — migrate to `bridge-codex2` (Go) | **⚠️ Deprecated — target removal `v1.0.0`** (issue #254) |
| `[client_codex]` | Stateless Codex client (1 message = 1 subprocess) | kept — different category from `[codex]`/`bridge-codex2` (stateless, like `[claude_p]`), not scheduled for removal |
| `[claude_p]` | Stateless Claude client (uses `claude` CLI) | kept — different category from `[claude]`/`bridge-claude2` (stateless) |
| `[all]`    | Install everything                                  | — |
| `[dev]`    | Test + lint toolchain (pytest, ruff)                | — |

## Install

```bash
# bridge-claude2 (Go — recommended replacement for [claude])
cd bridge-claude2 && make build  # → ./bridge-claude2

# [claude] — Deprecated: use bridge-claude2 instead
pip install "agent-hub-bridges[claude] @ git+https://github.com/kishibashi3/agent-hub-bridges.git"

# install multiple
pip install "agent-hub-bridges[claude,slack] @ git+https://github.com/kishibashi3/agent-hub-bridges.git"  # Deprecated: [claude] — use bridge-claude2 instead

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
# bridge-claude2 (Go — recommended)
./bridge-claude2/bridge-claude2 --participant claude-impl --tenant my-tenant --workdir /path/to/project

# Deprecated — use bridge-claude2 instead
agent-hub-bridge-claude --participant claude-impl --tenant my-tenant --workdir /path/to/project
agent-hub-bridge-slack
agent-hub-bridge-gemini --participant gemini-impl --tenant my-tenant --workdir /path/to/project
agent-hub-bridge-a2a --participant external-agent
```

Use `--add-dir` to include additional directories beyond `workdir` in Claude's project context (can be specified multiple times):

```bash
# bridge-claude2 (Go — recommended)
./bridge-claude2/bridge-claude2 --participant writer --workdir /path/to/writer --add-dir /path/to/publications --add-dir /path/to/shared

# Deprecated — use bridge-claude2 instead
agent-hub-bridge-claude --participant writer --workdir /path/to/writer --add-dir /path/to/publications --add-dir /path/to/shared
```

Required env (shared by all bridges):

```
AGENT_HUB_URL=http://localhost:3000/mcp
AGENT_HUB_GITHUB_PAT=<your-pat>   # 旧名 GITHUB_PAT も当面 alias 受理 (deprecated)
```

See `.env.example` for the full list of env vars (including bridge-specific ones).

## Layout

```
src/agent_hub_bridges/
├── __init__.py        # __version__ only — does NOT eager-import sub-packages
├── _common/           # internal helpers shared by all bridges
├── claude/            # Deprecated — use bridge-claude2/ (Go) instead
├── slack/             # M2 complete (ported from agent-hub-bridge-slack, archived)
├── gemini/            # M3 complete (ported from agent-hub-bridge-gemini, archived; SDK migration done)
└── a2a/               # M4 complete (new bridge per agent-hub#94)
```

Design: [`docs/design.md`](docs/design.md).
Migration guide: [`docs/migration.md`](docs/migration.md).

## Development

```bash
pip install -e ".[dev,all]"
pytest
ruff check src/
```

### Go bridges: agent-hub-sdk の取り込みと bump 手順 (issue #328)

Go の bridge 3 つ (`bridge-claude2` / `bridge-codex2` / `bridge-tmux`) は、`agent-hub-sdk/go` を
**go.mod の pseudo-version で取り込む** (`replace` で sibling clone を参照しない)。SDK の版は各 module の
`go.mod` / `go.sum` だけで決まり、CI・`scripts/deploy-fleet-binary.sh` も同じ版で build する。
agent-hub-sdk は `go/vX.Y.Z` 形式の tag を持たないので、commit SHA から pseudo-version を作る。

SDK を上げるときは、3 module すべてで同じ commit を指定する (1 つでも漏らさない):

```bash
SDK_SHA=<agent-hub-sdk の commit SHA (40 桁)>
for m in bridge-claude2 bridge-codex2 bridge-tmux; do
  (cd "$m" && go get "github.com/kishibashi3/agent-hub-sdk/go@$SDK_SHA" && go mod tidy)
done
grep -h 'agent-hub-sdk/go ' bridge-*/go.mod   # 3 行とも同じ pseudo-version になっていること
```

未 push の SDK 変更と合わせて手元で試すときは、commit しない `go.work` を使う
(`go work init ./bridge-claude2 && go work use ../agent-hub-sdk/go` など)。`go.mod` に `replace` は足さない。

Issues live at [`kishibashi3/agent-hub-bridges/issues`](https://github.com/kishibashi3/agent-hub-bridges/issues).
Use labels `bridge:<name>` + `type:<kind>` (= `feat` / `bug` / `doc` / `refactor`).

## Observability

### Circuit Breaker (all bridges)

All bridges share a reconnect loop that opens a circuit after N consecutive hub
connection failures and shuts down gracefully (issue
[#82](https://github.com/kishibashi3/agent-hub-bridges/issues/82)).

| env var | default | description |
|---|---|---|
| `AGENT_HUB_BRIDGE_MAX_RETRIES` | `10` | Consecutive reconnect failures before circuit opens. Set to `0` to disable (infinite retry). |

When the circuit opens the bridge:
1. Logs a `CRITICAL [circuit-breaker]` alert.
2. Writes `/tmp/agent-hub-bridge-<user>.dead` (dead marker).
3. Appends a `**lost-hub**` entry to `BRIDGE_INVENTORY` (if env is set).
4. Exits with code 1.

Operator cleanup:

```bash
# kill all circuit-broken bridges at once
BRIDGE_INVENTORY=~/.claude/projects/<key>/bridge-inventory.md \
  ./scripts/stop-bridge.sh --dead
```

Log signal to watch:

```
CRITICAL [circuit-breaker] hub session (claude): 10 consecutive reconnect failure(s) >= max_retries=10 — ALERT: hub connection assumed lost, shutting down gracefully.
```

### gemini bridge — rate-limit retry log signals

The gemini bridge emits grep-able log markers for rate-limit retry events (issue #19):

| marker | level | when |
|---|---|---|
| `[RATE_LIMIT_RETRY]` | `WARNING` | each retry attempt before backoff sleep |
| `rate-limit retry exhausted` | `WARNING` | max retries reached, giving up |

Example:
```
[RATE_LIMIT_RETRY] attempt=1/4 peer=@alice backoff=2.0s — gemini CLI rate-limited; sleeping before retry
```

Filter retry events with:
```bash
grep RATE_LIMIT_RETRY bridge.log
```

Future work: structured JSON log + external aggregator (loki / cloudwatch) for
`gemini_rate_limit_retries_total` counter export. In-process prometheus exporter
is out of scope for this repo; the log signal is the stable hook for aggregation.
See [issue #19](https://github.com/kishibashi3/agent-hub-bridges/issues/19).

## License

[Apache-2.0](LICENSE).
