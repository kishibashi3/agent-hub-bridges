# bridge-claude2

Go-native on-demand bridge replacing the Python `bridge-claude` worker (issue #152).

## Build

### Prerequisites

`bridge-claude2` depends on `agent-hub-sdk/go` via a `replace` directive in `go.mod`:

```
replace github.com/kishibashi3/agent-hub-sdk/go => ../../agent-hub-sdk/go
```

This means **both repositories must be checked out as siblings** under the same parent directory:

```
<parent>/
  agent-hub-bridges/      ← this repo
  agent-hub-sdk/          ← required sibling
    go/
```

If you clone only `agent-hub-bridges`, `go build` will fail with a "cannot find module" error. Clone `agent-hub-sdk` alongside it first:

```bash
git clone https://github.com/kishibashi3/agent-hub-sdk.git
git clone https://github.com/kishibashi3/agent-hub-bridges.git
```

### Build commands

```bash
cd bridge-claude2
make build            # build for current arch → ./bridge-claude2
make build-arm64      # cross-compile for Linux arm64 (e.g. Raspberry Pi 5)
make vet              # go vet
make test             # go test ./...
```

## Run

```bash
AGENT_HUB_URL=http://localhost:3000/mcp \
GITHUB_PAT=ghp_... \
  ./bridge-claude2 --user <handle> --workdir /path/to/workdir
```

### Required environment variables

| Variable | Description |
|---|---|
| `AGENT_HUB_URL` | agent-hub MCP endpoint (e.g. `http://localhost:3000/mcp`) |
| `GITHUB_PAT` | GitHub Personal Access Token (for Claude's GitHub tool calls) |

### Optional flags

| Flag | Default | Description |
|---|---|---|
| `--participant` / `-p` | *(required)* | agent-hub handle (e.g. `bridges-go-impl`). `--user` is a deprecated alias. |
| `--workdir` | *(required)* | Working directory passed to Claude as project root |
| `--model` | `""` | Claude model override (also `AGENT_HUB_MODEL` env). **Feeds the GitHub footer's `<model>` field** — see below. |
| `--tenant` | `""` | Tenant ID for multi-tenant deployments |
| `--add-dir` | — | Extra directories to include in Claude's project context (repeatable) |

## GitHub posting footer (standard rule, issue #245)

This bridge **auto-injects a GitHub posting footer instruction into every inner-Claude
prompt** (`formatPrompt`). When the peer writes a PR / issue comment or any deliverable
text, it is instructed to append a footer line of the form:

```
@<handle> [bridge-claude2 · <model>] (operator-supervised · <gh-login>/agent-hub)
```

Every field is composed **by the bridge from real values** — the inner Claude is never
asked to guess them (this eliminates the `<model>` hallucination that polluted
observability):

| Field | Source | If unavailable |
|---|---|---|
| `<handle>` | `--participant` | always present |
| `bridge-claude2` | `bridgeType` constant (single self-identity source) | always present |
| `<model>` | `--model` / `AGENT_HUB_MODEL` | **model field omitted entirely** (`[bridge-claude2]`) — never guessed |
| `<gh-login>` | resolved at startup via `gh api user --jq .login` | login part omitted (`(operator-supervised · agent-hub)`) |

Design trade-off (agreed in #245): this is a **per-message prompt instruction, not a
hard hook/shim**. The inner Claude can still forget to append it — in which case the
footer is simply *absent (honest)*. The guarantee is that **any footer that *is* present
carries only true values; at worst it is silent, never a lie**. Only the `agent-hub`
ecosystem label is a fixed literal; no personal proper nouns are baked into the source.

> Operator note: spawn with `-model <actual-model>` so the `<model>` field is populated.
> `gh-login` is self-resolved by the bridge and needs no extra argument.
