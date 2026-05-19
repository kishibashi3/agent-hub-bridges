"""M0 unit test: mention 除去 helper のみ. routing.py の本格 test は M1 で追加."""

from __future__ import annotations

from agent_hub_bridges.slack.slack_handler import _strip_bot_mention


def test_strip_bot_mention_removes_leading_mention() -> None:
    assert _strip_bot_mention("<@U01234567> hello") == "hello"


def test_strip_bot_mention_trims_whitespace() -> None:
    assert _strip_bot_mention("<@U01234567>   ping   ") == "ping"


def test_strip_bot_mention_only_first_mention() -> None:
    # 2 つ目以降の mention は本文の一部として残す
    assert (
        _strip_bot_mention("<@U01234567> tell <@U89999999> hi")
        == "tell <@U89999999> hi"
    )


def test_strip_bot_mention_empty_returns_empty() -> None:
    assert _strip_bot_mention("") == ""
    assert _strip_bot_mention(None) == ""  # type: ignore[arg-type]
