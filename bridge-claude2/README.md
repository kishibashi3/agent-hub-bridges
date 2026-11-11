# bridge-claude2

Go-native on-demand bridge replacing the Python `bridge-claude` worker (issue #152).

## Build

### Prerequisites

`bridge-claude2` depends on `agent-hub-sdk/go` via a pseudo-version in `go.mod` (no `replace`, no sibling clone needed).
`go build` fetches the pinned version through the Go module proxy.
To bump the SDK, follow the procedure in the root [`README.md`](../README.md#go-bridges-agent-hub-sdk-の取り込みと-bump-手順-issue-328) (all three Go bridges are bumped together).

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

## Spend / session limit handling (issue #268)

When the inner `claude` fails with a usage limit (`session limit` / `spend limit` /
`resets <time>`), the bridge **does not auto-reply** to the sender. Instead it:

1. parses the reset time (e.g. `resets 6:50pm (Asia/Tokyo)`) and **sleeps until then
   (+1 min)**; unparseable limit errors sleep a fixed 30 minutes,
2. stops calling `get_messages` while sleeping (unread messages stay queued on the hub;
   SSE stays connected so `is_online` remains `true` and the fleet watchdog does not
   respawn it),
3. re-registers with `display_name = "<name> (sleeping until 18:50 JST: spend limit)"`
   so the state is visible in `get_participants`, and restores it on wake,
4. logs `[limit] entering sleep` / `[limit] woke up` once each.

The message that hit the limit (already marked read) is kept in memory and re-processed
after wake, before the hub backlog. Inbound messages starting with
`(auto) bridge-claude2 error:` (or `(自動応答)`, still used by the Python bridges) never
receive an auto error reply, even for non-limit failures, so two bridges cannot bounce
errors at each other.

### Auto error reply cooldown (issue #267)

Every auto reply path — non-limit claude failures and a missing workdir (issue #272) —
sends `(auto) bridge-claude2 error: …` to the sender, but **at most once per sender per
10 minutes** (`autoReplyLimiter`), and never in reply to an auto reply echo. A ping-pong
only continues if the bridge replies again, so the second and later replies to the same
sender within the cooldown are suppressed with a WARN log
(`auto error reply suppressed by per-sender cooldown`). The first auto reply to a human
or a normal peer is unchanged. The cooldown state lives in the runner and survives hub
reconnects.

## Work that outlives a turn: `detach` (issue #395)

The bridge ends the inner `claude` after each request, so anything running *inside*
claude (a `run_in_background` task, a foreground Bash moved to the background when it
hit its timeout, a background Agent) stops with it. The bridge therefore:

1. starts claude with `CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1`, so a command that hits
   its timeout comes back to claude as a timeout instead of silently moving to the
   background;
2. offers `"$AGENT_HUB_BRIDGE_BIN" detach [-name <label>] -- '<command>'` (the prompt
   tells the peer about it). It starts a supervisor with `setsid` outside claude's process
   tree and returns at once. A single string runs with `bash -c`; two or more words run
   as argv;
3. checks the spool (`$AGENT_HUB_JOURNAL_DIR/detached/<tenant>__<participant>/`, default
   `~/.agent-hub/journals/...`) every 5 s. For each job that ended — **any exit code,
   a signal, a start failure, or a supervisor that disappeared (`LOST`)** — it wakes claude
   once in the session of the requester the job was started for, with the exit code,
   elapsed time, output path and the tail of the output. The hub does not accept a DM to
   yourself, so this notice is injected inside the bridge, not sent through the hub. It is
   kept on disk, so a bridge restart does not lose it (`<id>.notified` marks it as sent);
4. if claude still does not exit within 10 s after its result, stops it as before
   (issue #374) and now also sends `(auto) bridge-claude2 notice: …` to the requester,
   so the stopped work does not vanish silently.

## GitHub posting footer (standard rule, issue #245)

This bridge **auto-injects a GitHub posting footer instruction into every inner-Claude
prompt** (`formatPrompt`). When the peer writes a PR / issue comment or any deliverable
text, it is instructed to append a footer line of the form:

```
@<handle> [bridge-claude2 · <model>] (operator-supervised)
```

Every field is composed **by the bridge from real values** — the inner Claude is never
asked to guess them (this eliminates the `<model>` hallucination that polluted
observability):

| Field | Source | If unavailable |
|---|---|---|
| `<handle>` | `--participant` | always present |
| `bridge-claude2` | `bridgeType` constant (single self-identity source) | always present |
| `<model>` | `--model` / `AGENT_HUB_MODEL` | **model field omitted entirely** (`[bridge-claude2]`) — never guessed |

Design trade-off (agreed in #245): this is a **per-message prompt instruction, not a
hard hook/shim**. The inner Claude can still forget to append it — in which case the
footer is simply *absent (honest)*. The guarantee is that **any footer that *is* present
carries only true values; at worst it is silent, never a lie**. The GitHub account name
is not included (#385), so a post to a public repo does not expose which account the host uses.

> Operator note: spawn with `-model <actual-model>` so the `<model>` field is populated.
> `gh-login` is self-resolved by the bridge and needs no extra argument.
