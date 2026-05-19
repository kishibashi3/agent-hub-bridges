"""M4 unit test: error 可視化系の helper / _post_one rate-limit retry.

責務:
  - `format_send_failure_message` (PeerNotFoundError / HubTransientError /
    generic Exception を Slack thread に出す text に変換)
  - `_post_one` の Slack rate limit (429) inline retry path
"""

from __future__ import annotations

import pytest
from agent_hub_sdk import HubTransientError, IncomingMessage, PeerNotFoundError

from agent_hub_bridges.slack.config import Config
from agent_hub_bridges.slack.slack_handler import (
    _post_one,
    format_send_failure_message,
)

# ----- format_send_failure_message -------------------------------------


class TestFormatSendFailureMessage:
    def test_peer_not_found_mentions_offline_and_get_participants(self) -> None:
        e = PeerNotFoundError(peer="@gemma", detail="peer @gemma not found")
        out = format_send_failure_message("@gemma", e)
        # 人間が次のアクションを取れる文面
        assert ":bust_in_silhouette:" in out
        assert "@gemma" in out
        assert "offline" in out.lower() or "オフライン" in out
        assert "get_participants" in out

    def test_hub_transient_mentions_retry_hint(self) -> None:
        e = HubTransientError("503 Service Unavailable")
        out = format_send_failure_message("@gemma", e)
        assert ":hourglass:" in out
        # ユーザに「後で再試行」を促す文言
        assert "再試行" in out or "後で" in out or "時間を置" in out
        # 元 error の identity (= debug 用) も残す
        assert "503" in out

    def test_unknown_error_falls_back_to_generic_warning(self) -> None:
        e = ValueError("schema invalid")
        out = format_send_failure_message("@gemma", e)
        assert ":warning:" in out
        assert "@gemma" in out
        assert "schema invalid" in out

    def test_peer_handle_normalized_in_retry_hint(self) -> None:
        # "@gemma" でも "gemma" でも、HubTransientError 時の使い方ヒントには
        # `@bot gemma ...` 形で出す (先頭 `@` を 1 個に絞る)
        e = HubTransientError("timeout")
        out = format_send_failure_message("@gemma", e)
        assert "@bot gemma" in out


# ----- _post_one: rate-limit retry path --------------------------------


def _make_config(*, default_channel: str | None = "C0123") -> Config:
    return Config(
        slack_bot_token="xoxb-test",
        slack_app_token="xapp-test",
        slack_default_channel=default_channel,
        user="slack-bot",
        display_name=None,
        tenant=None,
        agent_hub_url="http://localhost/mcp",
        github_pat="ghp_test",
        workdir=None,
    )


def _make_msg() -> IncomingMessage:
    return IncomingMessage(
        id="msg-1",
        sender="@gemma",
        to="@slack-bot",
        body="hello",
        timestamp="2026-05-13T13:00:00Z",
    )


class _RateLimitedResp:
    """SlackResponse 風の duck typing object.

    `parse_slack_retry_after` が見るのは:
      - `.get("error") == "ratelimited"`
      - `.headers["Retry-After"]`
    """

    def __init__(self, retry_after: int = 1) -> None:
        self.headers = {"Retry-After": str(retry_after)}

    def get(self, key, default=None):
        if key == "error":
            return "ratelimited"
        return default


class _SlackApiError(Exception):
    def __init__(self, retry_after: int = 1) -> None:
        super().__init__("ratelimited")
        self.response = _RateLimitedResp(retry_after=retry_after)


class _SequencedClient:
    """`chat_postMessage` の連続呼出で 異なる挙動を返す client stub."""

    def __init__(self, behaviors: list) -> None:
        # behaviors: 各要素は "ok" または Exception 実体
        self.behaviors = list(behaviors)
        self.calls: list[dict] = []

    async def chat_postMessage(self, *, channel: str, text: str, thread_ts: str | None) -> dict:
        self.calls.append({"channel": channel, "text": text, "thread_ts": thread_ts})
        b = self.behaviors.pop(0)
        if isinstance(b, Exception):
            raise b
        return {"ok": True}


class TestPostOneRateLimit:
    @pytest.mark.asyncio
    async def test_rate_limited_then_retry_succeeds(self) -> None:
        client = _SequencedClient([_SlackApiError(retry_after=2), "ok"])
        sleep_calls: list[float] = []

        async def fake_sleep(s: float) -> None:
            sleep_calls.append(s)

        ok = await _post_one(client, _make_config(), _make_msg(), sleep_fn=fake_sleep)
        assert ok is True  # retry 後成功 → mark_as_read してよい
        assert sleep_calls == [2]  # Retry-After 秒を尊重
        assert len(client.calls) == 2  # 1 回目 (rate limited) + retry

    @pytest.mark.asyncio
    async def test_rate_limited_retry_also_fails_returns_false(self) -> None:
        client = _SequencedClient([
            _SlackApiError(retry_after=1),
            RuntimeError("still down"),
        ])
        sleep_calls: list[float] = []

        async def fake_sleep(s: float) -> None:
            sleep_calls.append(s)

        ok = await _post_one(client, _make_config(), _make_msg(), sleep_fn=fake_sleep)
        assert ok is False  # 2 度目失敗 → mark_as_read せず 次の push に retry 委譲
        assert sleep_calls == [1]
        assert len(client.calls) == 2

    @pytest.mark.asyncio
    async def test_non_rate_limit_error_no_sleep_returns_false(self) -> None:
        # rate limit でない error は inline retry せず False で抜ける
        client = _SequencedClient([RuntimeError("channel_not_found")])
        sleep_calls: list[float] = []

        async def fake_sleep(s: float) -> None:
            sleep_calls.append(s)

        ok = await _post_one(client, _make_config(), _make_msg(), sleep_fn=fake_sleep)
        assert ok is False
        assert sleep_calls == []
        assert len(client.calls) == 1


if __name__ == "__main__":
    pytest.main([__file__, "-v"])
