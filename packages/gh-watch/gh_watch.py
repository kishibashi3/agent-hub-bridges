#!/usr/bin/env python3
"""
@gh-watch — GitHub App webhook → agent-hub DM 変換 peer (issue #289)

GitHub App の webhook を受信し、購読条件にマッチした場合に agent-hub DM を送る
非 LLM peer。 `@scheduler` の GitHub イベント版。

コマンドインターフェース (inbox DM):
  /watch resource="<resource>" event=<event_type> [label=<name>]
  /list            — 自分の購読一覧
  /delete <label_or_id>  — 購読を削除 (自分の entry のみ)
  /ping            — 生存確認
  /help            — コマンド一覧

Resource 書式:
  github/<owner>/<repo>/<resource-type>/<identifier>
  例: github/kishibashi3/agent-hub/pr/*
      github/kishibashi3/agent-hub/pr/100
      github/kishibashi3/agent-hub/issue/*

対応イベント種別:
  pr_opened, pr_closed, pr_merged, ci_complete, ci_success, ci_failure,
  issue_opened, issue_closed, review_requested, review_approved,
  review_changes_requested

環境変数:
  AGENT_HUB_URL              MCP endpoint (default: http://localhost:3000/mcp)
  AGENT_HUB_GITHUB_PAT       GitHub PAT (agent-hub auth)
  AGENT_HUB_PARTICIPANT      handle 名 (pat mode handle override)
  AGENT_HUB_TENANT           tenant 識別子
  GH_WATCH_PORT              webhook 受信 port (default: 3001)
  GH_WATCH_WEBHOOK_SECRET    GitHub App webhook secret (HMAC-SHA256 検証)
  GH_WATCH_DB_PATH           SQLite DB path (default: subscriptions.db in script dir)
"""

from __future__ import annotations

import hashlib
import hmac
import http.server
import json
import logging
import os
import re
import signal
import sqlite3
import sys
import threading
import time
import uuid
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import requests

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
    stream=sys.stderr,
)
logger = logging.getLogger("gh-watch")

# ============================================================
# Config
# ============================================================

HUB_URL = os.environ.get("AGENT_HUB_URL", "http://localhost:3000/mcp")
PAT = os.environ.get("AGENT_HUB_GITHUB_PAT", "")
HANDLE_OVERRIDE = os.environ.get("AGENT_HUB_PARTICIPANT", "")
TENANT = os.environ.get("AGENT_HUB_TENANT", "")
WEBHOOK_PORT = int(os.environ.get("GH_WATCH_PORT", "3001"))
WEBHOOK_SECRET = os.environ.get("GH_WATCH_WEBHOOK_SECRET", "")
DB_PATH = Path(
    os.environ.get("GH_WATCH_DB_PATH", str(Path(__file__).parent / "subscriptions.db"))
)

SELF_HANDLE = HANDLE_OVERRIDE or "gh-watch"

_shutdown_event = threading.Event()


def _on_signal(signum: int, _frame: Any) -> None:
    logger.info("received signal %d, shutting down", signum)
    _shutdown_event.set()


# ============================================================
# MCP HTTP client (scheduler と同パターン)
# ============================================================


def _build_headers() -> dict[str, str]:
    headers: dict[str, str] = {
        "Content-Type": "application/json; charset=utf-8",
        "Accept": "application/json, text/event-stream",
    }
    if TENANT:
        headers["X-Tenant-Id"] = TENANT
    if PAT:
        headers["Authorization"] = f"Bearer {PAT}"
        if HANDLE_OVERRIDE:
            headers["X-User-Id"] = HANDLE_OVERRIDE
    else:
        logger.error("AGENT_HUB_GITHUB_PAT is required")
        sys.exit(1)
    return headers


def _encode_body(payload: dict[str, Any]) -> bytes:
    return json.dumps(payload, ensure_ascii=False).encode("utf-8")


def _parse_response(resp: requests.Response) -> dict[str, Any]:
    content_type = resp.headers.get("Content-Type", "")
    if "text/event-stream" in content_type:
        for line in resp.content.decode("utf-8").splitlines():
            if line.startswith("data:"):
                return json.loads(line[5:].strip())
        raise ValueError(f"no data: line in SSE response: {resp.text[:200]}")
    return resp.json()


def _mcp_call(
    session_id: str,
    method: str,
    params: dict[str, Any],
    headers: dict[str, str],
    timeout: int = 30,
) -> dict[str, Any]:
    payload = {
        "jsonrpc": "2.0",
        "id": 1,
        "method": method,
        "params": params,
    }
    h = {**headers, "Mcp-Session-Id": session_id}
    resp = requests.post(HUB_URL, data=_encode_body(payload), headers=h, timeout=timeout)
    if resp.status_code not in (200, 202):
        raise RuntimeError(f"MCP {method} failed: HTTP {resp.status_code}: {resp.text[:200]}")
    return _parse_response(resp)


def _initialize_session(headers: dict[str, str]) -> str:
    payload = {
        "jsonrpc": "2.0",
        "id": 0,
        "method": "initialize",
        "params": {
            "protocolVersion": "2024-11-05",
            "capabilities": {},
            "clientInfo": {"name": "gh-watch", "version": "1.0.0"},
        },
    }
    resp = requests.post(HUB_URL, data=_encode_body(payload), headers=headers, timeout=30)
    if resp.status_code not in (200, 202):
        raise RuntimeError(f"initialize failed: HTTP {resp.status_code}: {resp.text[:200]}")
    session_id = resp.headers.get("Mcp-Session-Id", "")
    if not session_id:
        raise RuntimeError("no Mcp-Session-Id in initialize response")
    # notifications/initialized
    notif = {
        "jsonrpc": "2.0",
        "method": "notifications/initialized",
    }
    requests.post(
        HUB_URL,
        data=_encode_body(notif),
        headers={**headers, "Mcp-Session-Id": session_id},
        timeout=10,
    )
    return session_id


def _tool_call(
    session_id: str,
    tool: str,
    arguments: dict[str, Any],
    headers: dict[str, str],
    timeout: int = 30,
) -> Any:
    body = _mcp_call(
        session_id,
        "tools/call",
        {"name": tool, "arguments": arguments},
        headers,
        timeout=timeout,
    )
    result = body.get("result", {})
    if isinstance(result, dict) and result.get("isError"):
        raise RuntimeError(f"tool {tool} returned error: {result}")
    content = result.get("content", [])
    if content and isinstance(content[0], dict):
        return content[0].get("text", "")
    return ""


def _register(session_id: str, headers: dict[str, str]) -> None:
    _tool_call(
        session_id,
        "register",
        {
            "name": SELF_HANDLE,
            "display_name": "GitHub Watch — webhook→DM bridge",
            "mode": "stateless",
        },
        headers,
    )
    logger.info("registered as @%s", SELF_HANDLE)


def _send_message(
    session_id: str,
    headers: dict[str, str],
    to: str,
    message: str,
    caused_by: str | None = None,
) -> None:
    args: dict[str, Any] = {"to": to, "message": message}
    if caused_by:
        args["caused_by"] = caused_by
    _tool_call(session_id, "send_message", args, headers)


def _get_messages(session_id: str, headers: dict[str, str]) -> list[dict[str, Any]]:
    raw = _tool_call(session_id, "get_messages", {}, headers)
    try:
        return json.loads(raw) if isinstance(raw, str) else raw or []
    except (json.JSONDecodeError, TypeError):
        return []


def _mark_as_read(session_id: str, headers: dict[str, str], message_id: str) -> None:
    try:
        _tool_call(session_id, "mark_as_read", {"message_id": message_id}, headers)
    except Exception as e:
        logger.warning("mark_as_read failed: %s", e)


def _subscribe_inbox(session_id: str, headers: dict[str, str]) -> None:
    _mcp_call(
        session_id,
        "resources/subscribe",
        {"uri": f"inbox://@{SELF_HANDLE}"},
        headers,
    )
    logger.info("subscribed to inbox://@%s", SELF_HANDLE)


# ============================================================
# SQLite subscription registry
# ============================================================


def _db_connect() -> sqlite3.Connection:
    conn = sqlite3.connect(str(DB_PATH))
    conn.row_factory = sqlite3.Row
    return conn


def _init_db() -> None:
    with _db_connect() as conn:
        conn.execute(
            """
            CREATE TABLE IF NOT EXISTS subscriptions (
                id TEXT PRIMARY KEY,
                owner TEXT NOT NULL,
                resource TEXT NOT NULL,
                event_type TEXT NOT NULL,
                label TEXT,
                created_at TEXT NOT NULL
            )
            """
        )
        conn.commit()
    logger.info("subscriptions DB ready: %s", DB_PATH)


def _add_subscription(owner: str, resource: str, event_type: str, label: str | None) -> str:
    sub_id = str(uuid.uuid4())[:8]
    now = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.000Z")
    with _db_connect() as conn:
        conn.execute(
            "INSERT INTO subscriptions (id, owner, resource, event_type, label, created_at) "
            "VALUES (?, ?, ?, ?, ?, ?)",
            (sub_id, owner, resource, event_type, label, now),
        )
        conn.commit()
    return sub_id


def _list_subscriptions(owner: str) -> list[sqlite3.Row]:
    with _db_connect() as conn:
        return conn.execute(
            "SELECT * FROM subscriptions WHERE owner=? ORDER BY created_at",
            (owner,),
        ).fetchall()


def _delete_subscription(owner: str, label_or_id: str) -> bool:
    with _db_connect() as conn:
        cur = conn.execute(
            "DELETE FROM subscriptions WHERE owner=? AND (id=? OR label=?)",
            (owner, label_or_id, label_or_id),
        )
        conn.commit()
        return cur.rowcount > 0


def _get_all_subscriptions() -> list[sqlite3.Row]:
    with _db_connect() as conn:
        return conn.execute("SELECT * FROM subscriptions").fetchall()


# ============================================================
# Resource matching
# ============================================================

VALID_EVENT_TYPES = {
    "pr_opened", "pr_closed", "pr_merged",
    "ci_complete", "ci_success", "ci_failure",
    "issue_opened", "issue_closed",
    "review_requested", "review_approved", "review_changes_requested",
}

RESOURCE_PATTERN = re.compile(
    r"^github/[^/]+/[^/]+/(pr|issue|ci)/(\d+|\*)$"
)


def _resource_matches(pattern: str, resource: str) -> bool:
    """購読 pattern が webhook 由来の resource にマッチするか判定。* はワイルドカード。"""
    parts_p = pattern.split("/")
    parts_r = resource.split("/")
    if len(parts_p) != len(parts_r):
        return False
    return all(p == r or p == "*" for p, r in zip(parts_p, parts_r))


# ============================================================
# GitHub webhook event → (resource, event_types) mapping
# ============================================================


def _parse_webhook_event(
    gh_event: str, payload: dict[str, Any]
) -> list[tuple[str, str]]:
    """GitHub webhook payload を (resource, event_type) ペアのリストに変換。

    同一 webhook が複数の event_type にマッチし得る (ci_complete + ci_success 等) 。
    """
    results: list[tuple[str, str]] = []

    repo = payload.get("repository", {})
    owner = repo.get("owner", {}).get("login", "")
    repo_name = repo.get("name", "")
    if not owner or not repo_name:
        return results

    if gh_event == "pull_request":
        pr = payload.get("pull_request", {})
        pr_num = pr.get("number", "*")
        resource = f"github/{owner}/{repo_name}/pr/{pr_num}"
        action = payload.get("action", "")

        if action == "opened":
            results.append((resource, "pr_opened"))
        elif action == "closed":
            if pr.get("merged"):
                results.append((resource, "pr_merged"))
            else:
                results.append((resource, "pr_closed"))
        elif action == "review_requested":
            results.append((resource, "review_requested"))

    elif gh_event == "pull_request_review":
        pr = payload.get("pull_request", {})
        pr_num = pr.get("number", "*")
        resource = f"github/{owner}/{repo_name}/pr/{pr_num}"
        review = payload.get("review", {})
        action = payload.get("action", "")
        state = review.get("state", "")

        if action == "submitted":
            if state == "approved":
                results.append((resource, "review_approved"))
            elif state == "changes_requested":
                results.append((resource, "review_changes_requested"))

    elif gh_event in ("check_suite", "workflow_run"):
        resource = f"github/{owner}/{repo_name}/ci/*"
        action = payload.get("action", "")
        if action == "completed":
            conclusion = (
                payload.get("check_suite", payload.get("workflow_run", {}))
                .get("conclusion", "")
            )
            results.append((resource, "ci_complete"))
            if conclusion in ("success",):
                results.append((resource, "ci_success"))
            elif conclusion in ("failure", "timed_out", "cancelled"):
                results.append((resource, "ci_failure"))

    elif gh_event == "issues":
        issue = payload.get("issue", {})
        issue_num = issue.get("number", "*")
        resource = f"github/{owner}/{repo_name}/issue/{issue_num}"
        action = payload.get("action", "")

        if action == "opened":
            results.append((resource, "issue_opened"))
        elif action == "closed":
            results.append((resource, "issue_closed"))

    return results


def _build_notification_message(
    event_type: str, resource: str, payload: dict[str, Any]
) -> str:
    """購読者への DM メッセージを組み立てる。"""
    lines = [f"[gh-watch] {event_type}: {resource}", ""]

    repo = payload.get("repository", {})
    repo_full = repo.get("full_name", resource)
    html_url = ""

    if "pull_request" in payload:
        pr = payload["pull_request"]
        title = pr.get("title", "")
        html_url = pr.get("html_url", "")
        lines.append(f'PR #{pr.get("number")} 「{title}」')
    elif "issue" in payload and "pull_request" not in payload:
        issue = payload["issue"]
        title = issue.get("title", "")
        html_url = issue.get("html_url", "")
        lines.append(f'Issue #{issue.get("number")} 「{title}」')
    elif "workflow_run" in payload:
        run = payload["workflow_run"]
        html_url = run.get("html_url", "")
        conclusion = run.get("conclusion", "")
        name = run.get("name", "")
        lines.append(f'Workflow: {name} ({conclusion})')
    elif "check_suite" in payload:
        suite = payload["check_suite"]
        conclusion = suite.get("conclusion", "")
        lines.append(f'CI conclusion: {conclusion}')

    if html_url:
        lines.append(html_url)
    elif repo_full:
        lines.append(f"https://github.com/{repo_full}")

    return "\n".join(lines)


# ============================================================
# Inbox command handler
# ============================================================

HELP_TEXT = """\
@gh-watch コマンド一覧:

/watch resource="<resource>" event=<event_type> [label=<name>]
  購読を登録。resource 書式: github/<owner>/<repo>/<type>/<id|*>
  event: pr_opened | pr_closed | pr_merged | ci_complete | ci_success |
         ci_failure | issue_opened | issue_closed | review_requested |
         review_approved | review_changes_requested

/list             — 自分の購読一覧
/delete <label|id> — 購読を削除 (自分の entry のみ)
/ping             — 生存確認
/help             — このヘルプ"""


def _parse_watch_command(body: str) -> dict[str, str] | None:
    """'/watch resource="..." event=... [label=...]' をパース。"""
    resource_m = re.search(r'resource=["\']([^"\']+)["\']', body)
    event_m = re.search(r"event=(\S+)", body)
    label_m = re.search(r"label=(\S+)", body)

    if not resource_m or not event_m:
        return None

    return {
        "resource": resource_m.group(1),
        "event_type": event_m.group(1),
        "label": label_m.group(1) if label_m else "",
    }


def handle_inbox_message(
    msg: dict[str, Any],
    session_id: str,
    headers: dict[str, str],
) -> None:
    sender = msg.get("from", "@unknown")
    body = msg.get("message", "").strip()
    msg_id = msg.get("id", "")

    if not body.startswith("/"):
        _send_message(
            session_id, headers, sender,
            "コマンドは `/` prefix が必要です。`/help` でコマンド一覧を確認できます。",
            caused_by=msg_id,
        )
        return

    cmd = body.split()[0].lower()

    if cmd == "/ping":
        _send_message(session_id, headers, sender, "pong 🟢", caused_by=msg_id)

    elif cmd == "/help":
        _send_message(session_id, headers, sender, HELP_TEXT, caused_by=msg_id)

    elif cmd == "/watch":
        parsed = _parse_watch_command(body)
        if not parsed:
            _send_message(
                session_id, headers, sender,
                "書式: /watch resource=\"<resource>\" event=<event_type> [label=<name>]",
                caused_by=msg_id,
            )
            return

        resource = parsed["resource"]
        event_type = parsed["event_type"]
        label = parsed["label"] or None

        if not RESOURCE_PATTERN.match(resource):
            _send_message(
                session_id, headers, sender,
                f"resource 書式が不正です: `{resource}`\n"
                "例: github/kishibashi3/agent-hub/pr/*",
                caused_by=msg_id,
            )
            return

        if event_type not in VALID_EVENT_TYPES:
            _send_message(
                session_id, headers, sender,
                f"未対応の event_type: `{event_type}`\n"
                f"対応: {', '.join(sorted(VALID_EVENT_TYPES))}",
                caused_by=msg_id,
            )
            return

        sub_id = _add_subscription(sender, resource, event_type, label)
        label_part = f" (label={label})" if label else ""
        _send_message(
            session_id, headers, sender,
            f"✅ 購読登録しました [id={sub_id}]{label_part}\n"
            f"  resource: {resource}\n"
            f"  event: {event_type}",
            caused_by=msg_id,
        )
        logger.info("subscription added: id=%s owner=%s resource=%s event=%s", sub_id, sender, resource, event_type)

    elif cmd == "/list":
        subs = _list_subscriptions(sender)
        if not subs:
            _send_message(session_id, headers, sender, "購読なし。", caused_by=msg_id)
        else:
            lines = ["購読一覧:"]
            for s in subs:
                label_part = f" (label={s['label']})" if s["label"] else ""
                lines.append(
                    f"  [{s['id']}]{label_part} {s['resource']} — {s['event_type']}"
                )
            _send_message(session_id, headers, sender, "\n".join(lines), caused_by=msg_id)

    elif cmd == "/delete":
        parts = body.split(maxsplit=1)
        if len(parts) < 2:
            _send_message(
                session_id, headers, sender,
                "書式: /delete <label|id>",
                caused_by=msg_id,
            )
            return
        target = parts[1].strip()
        deleted = _delete_subscription(sender, target)
        if deleted:
            _send_message(session_id, headers, sender, f"🗑️ `{target}` を削除しました。", caused_by=msg_id)
        else:
            _send_message(
                session_id, headers, sender,
                f"`{target}` が見つかりません (自分の entry のみ削除可)。",
                caused_by=msg_id,
            )

    else:
        _send_message(
            session_id, headers, sender,
            f"未知のコマンド: `{cmd}`。`/help` でコマンド一覧を確認できます。",
            caused_by=msg_id,
        )


# ============================================================
# Webhook HTTP server
# ============================================================


def _verify_signature(body: bytes, signature: str) -> bool:
    expected = "sha256=" + hmac.new(
        WEBHOOK_SECRET.encode("utf-8"), body, hashlib.sha256
    ).hexdigest()
    return hmac.compare_digest(expected, signature)


class _WebhookHandler(http.server.BaseHTTPRequestHandler):
    """GitHub App webhook receiver。"""

    session_ref: list[str] = []
    headers_ref: list[dict[str, str]] = []

    def log_message(self, fmt: str, *args: Any) -> None:
        logger.debug("webhook: " + fmt, *args)

    def do_POST(self) -> None:  # noqa: N802
        if self.path not in ("/", "/webhook"):
            self.send_response(404)
            self.end_headers()
            return

        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length)

        sig = self.headers.get("X-Hub-Signature-256", "")
        if not _verify_signature(body, sig):
            logger.warning("webhook signature verification failed")
            self.send_response(401)
            self.end_headers()
            self.wfile.write(b'{"error":"invalid signature"}')
            return

        gh_event = self.headers.get("X-GitHub-Event", "")
        delivery_id = self.headers.get("X-GitHub-Delivery", "unknown")

        try:
            payload = json.loads(body)
        except json.JSONDecodeError:
            self.send_response(400)
            self.end_headers()
            return

        self.send_response(202)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"ok":true}')

        # バックグラウンドで通知処理（レスポンス後に実行）
        t = threading.Thread(
            target=_dispatch_webhook,
            args=(gh_event, payload, delivery_id),
            daemon=True,
        )
        t.start()

    def do_GET(self) -> None:  # noqa: N802
        if self.path == "/health":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"status":"ok"}')
        else:
            self.send_response(404)
            self.end_headers()


# session / headers はグローバルから参照
_session_id: str = ""
_mcp_headers: dict[str, str] = {}
_send_lock = threading.Lock()


def _dispatch_webhook(gh_event: str, payload: dict[str, Any], delivery_id: str) -> None:
    """webhook から購読にマッチした DM を送信する。"""
    events = _parse_webhook_event(gh_event, payload)
    if not events:
        logger.debug("delivery %s: no matching event types for gh_event=%s", delivery_id, gh_event)
        return

    all_subs = _get_all_subscriptions()
    notified: set[tuple[str, str]] = set()  # (owner, event_type) dedup

    for resource, event_type in events:
        for sub in all_subs:
            if sub["event_type"] != event_type:
                continue
            if not _resource_matches(sub["resource"], resource):
                continue

            dedup_key = (sub["owner"], resource, event_type)
            if dedup_key in notified:
                continue
            notified.add(dedup_key)

            msg = _build_notification_message(event_type, resource, payload)
            try:
                with _send_lock:
                    _send_message(_session_id, _mcp_headers, sub["owner"], msg)
                logger.info(
                    "delivery %s: notified %s — %s %s",
                    delivery_id, sub["owner"], event_type, resource,
                )
            except Exception as e:
                logger.error("failed to notify %s: %s", sub["owner"], e)


# ============================================================
# SSE inbox polling loop
# ============================================================


def _inbox_loop(session_id: str, headers: dict[str, str]) -> None:
    """SSE long-poll で inbox 監視 + コマンド処理。"""
    sse_url = HUB_URL.replace("/mcp", "") + "/mcp"
    sse_headers = {
        **headers,
        "Mcp-Session-Id": session_id,
        "Accept": "text/event-stream",
    }

    while not _shutdown_event.is_set():
        try:
            with requests.get(sse_url, headers=sse_headers, stream=True, timeout=65) as resp:
                if resp.status_code not in (200, 202):
                    logger.warning("SSE connect failed: HTTP %d", resp.status_code)
                    time.sleep(5)
                    continue

                for line in resp.iter_lines():
                    if _shutdown_event.is_set():
                        break
                    if not line:
                        continue
                    text = line.decode("utf-8") if isinstance(line, bytes) else line
                    if not text.startswith("data:"):
                        continue
                    try:
                        data = json.loads(text[5:].strip())
                    except json.JSONDecodeError:
                        continue

                    method = data.get("method", "")
                    if method == "notifications/resources/updated":
                        # inbox に新着 → get_messages で取得
                        try:
                            msgs = _get_messages(session_id, headers)
                            for msg in msgs:
                                handle_inbox_message(msg, session_id, headers)
                                _mark_as_read(session_id, headers, msg["id"])
                        except Exception as e:
                            logger.error("inbox processing error: %s", e)

        except Exception as e:
            if not _shutdown_event.is_set():
                logger.warning("SSE disconnected: %s, reconnecting in 3s", e)
                time.sleep(3)


# ============================================================
# Main
# ============================================================


def _startup_catchup(session_id: str, headers: dict[str, str]) -> None:
    """起動時の未読メッセージを処理する。"""
    try:
        msgs = _get_messages(session_id, headers)
        if msgs:
            logger.info("startup catchup: %d unread messages", len(msgs))
        for msg in msgs:
            handle_inbox_message(msg, session_id, headers)
            _mark_as_read(session_id, headers, msg["id"])
    except Exception as e:
        logger.warning("startup catchup failed: %s", e)


def main() -> None:
    global _session_id, _mcp_headers

    signal.signal(signal.SIGTERM, _on_signal)
    signal.signal(signal.SIGINT, _on_signal)

    if not WEBHOOK_SECRET:
        logger.error("GH_WATCH_WEBHOOK_SECRET is required — set the GitHub App webhook secret")
        sys.exit(1)

    logger.info("gh-watch starting: handle=@%s hub=%s webhook_port=%d", SELF_HANDLE, HUB_URL, WEBHOOK_PORT)

    _init_db()

    headers = _build_headers()
    session_id = _initialize_session(headers)
    _session_id = session_id
    _mcp_headers = headers

    _register(session_id, headers)
    _startup_catchup(session_id, headers)
    _subscribe_inbox(session_id, headers)

    # Webhook HTTP server をバックグラウンドスレッドで起動
    webhook_server = http.server.HTTPServer(("0.0.0.0", WEBHOOK_PORT), _WebhookHandler)
    webhook_thread = threading.Thread(target=webhook_server.serve_forever, daemon=True)
    webhook_thread.start()
    logger.info("webhook server listening on port %d", WEBHOOK_PORT)

    # SSE inbox 監視をバックグラウンドで起動
    inbox_thread = threading.Thread(
        target=_inbox_loop,
        args=(session_id, headers),
        daemon=True,
    )
    inbox_thread.start()

    logger.info("@%s ready", SELF_HANDLE)

    # メインスレッドはシャットダウン待機
    _shutdown_event.wait()
    logger.info("shutting down webhook server")
    webhook_server.shutdown()
    logger.info("gh-watch stopped")


if __name__ == "__main__":
    main()
