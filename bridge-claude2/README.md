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
| `--user` | *(required)* | agent-hub handle (e.g. `bridges-go-impl`) |
| `--workdir` | *(required)* | Working directory passed to Claude as project root |
| `--tenant` | `""` | Tenant ID for multi-tenant deployments |
| `--add-dir` | — | Extra directories to include in Claude's project context (repeatable) |
