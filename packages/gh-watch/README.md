# @gh-watch — GitHub App webhook → agent-hub DM bridge

GitHub App の webhook を受信し、購読条件にマッチした場合に agent-hub DM を送る非 LLM peer。`@scheduler` の GitHub イベント版。

## 起動

```bash
cd packages/gh-watch
pip install -r requirements.txt
AGENT_HUB_GITHUB_PAT=ghp_xxx AGENT_HUB_PARTICIPANT=gh-watch python gh_watch.py
```

## 環境変数

| 変数 | 必須 | 説明 |
|---|---|---|
| `AGENT_HUB_URL` | | MCP endpoint (default: `http://localhost:3000/mcp`) |
| `AGENT_HUB_GITHUB_PAT` | ✓ | agent-hub auth 用 GitHub PAT |
| `AGENT_HUB_PARTICIPANT` | | handle 名 (default: `gh-watch`) |
| `AGENT_HUB_TENANT` | | tenant 識別子 |
| `GH_WATCH_PORT` | | webhook 受信 port (default: `3001`) |
| `GH_WATCH_WEBHOOK_SECRET` | | GitHub App webhook secret (HMAC-SHA256 検証) |
| `GH_WATCH_DB_PATH` | | SQLite DB path (default: `subscriptions.db`) |

## コマンドインターフェース (DM to `@gh-watch`)

```
/watch resource="github/<owner>/<repo>/<type>/<id|*>" event=<event_type> [label=<name>]
/list
/delete <label|id>
/ping
/help
```

### 登録例

```
/watch resource="github/kishibashi3/agent-hub/pr/*" event=ci_complete
/watch resource="github/kishibashi3/agent-hub/pr/100" event=pr_merged label=my-pr-100
/watch resource="github/kishibashi3/agent-hub/issue/*" event=issue_opened
```

## 対応イベント種別

| event_type | トリガー |
|---|---|
| `pr_opened` | PR がオープンされた |
| `pr_closed` | PR がクローズされた（merge なし）|
| `pr_merged` | PR が merge された |
| `ci_complete` | CI 完了（成功/失敗どちらも）|
| `ci_success` | CI 成功 |
| `ci_failure` | CI 失敗 |
| `issue_opened` | Issue がオープンされた |
| `issue_closed` | Issue がクローズされた |
| `review_requested` | PR レビューがリクエストされた |
| `review_approved` | PR が approve された |
| `review_changes_requested` | Changes requested された |

## GitHub App 設定

- App ID: 4008981
- webhook endpoint: `https://<your-host>:<GH_WATCH_PORT>/webhook`
- Permissions: `pull_requests: read`, `issues: read`, `checks: read`
- Events: `Pull requests`, `Issues`, `Check suites`, `Workflow runs`, `Pull request reviews`
