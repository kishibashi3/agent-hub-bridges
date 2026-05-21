"""rate-limit (429) retry / backoff ロジックのテスト.

`gemini` CLI 本体は呼び出さない:
  - rate-limit 検出 / retryDelay parse / backoff 計算の純関数を直接テスト
  - `GeminiCLIEngine.run` の retry loop は `_invoke_once` を monkeypatch して
    subprocess 起動を抑止しつつ挙動を検証する
"""

from __future__ import annotations

from pathlib import Path

import pytest

from agent_hub_bridges.gemini.config import Config
from agent_hub_bridges.gemini.engine import (
    EngineResult,
    GeminiCLIEngine,
    _compute_backoff_s,
    _parse_retry_delay_s,
    is_rate_limit_error,
)

# ---------- 純関数: rate-limit detection ----------


@pytest.mark.parametrize(
    "stderr",
    [
        (
            "Quota exceeded for metric: "
            "generativelanguage.googleapis.com/generate_content_free_tier_requests"
        ),
        "Error: 429 Too Many Requests",
        "google.api_core.exceptions.ResourceExhausted: 429 ...",
        "RESOURCE_EXHAUSTED: rate limit hit",
        "Please retry: rate limit reached",
    ],
)
def testis_rate_limit_error_detects(stderr: str) -> None:
    assert is_rate_limit_error(stderr) is True


@pytest.mark.parametrize(
    "stderr",
    [
        "",
        "command not found: gemini",
        "Error: invalid API key",
        "Some normal stderr without quota markers",
    ],
)
def testis_rate_limit_error_ignores_unrelated(stderr: str) -> None:
    assert is_rate_limit_error(stderr) is False


# ---------- 純関数: retryDelay parse ----------


@pytest.mark.parametrize(
    "stderr, expected",
    [
        ('{"retryDelay": "13s"}', 13.0),
        ('"retry_delay": "30s"', 30.0),
        ("Please retry in 12.5s", 12.5),
        ("retry after 5 seconds", 5.0),
        ("retry after 7 sec", 7.0),
        ("retry in 1s", 1.0),
    ],
)
def test_parse_retry_delay_s_extracts(stderr: str, expected: float) -> None:
    assert _parse_retry_delay_s(stderr) == pytest.approx(expected)


@pytest.mark.parametrize(
    "stderr",
    [
        "",
        "Quota exceeded but no delay info",
        "retry in soon",  # 数字無し
        "retryDelay: 0s",  # 0 は noise 扱いで無視
    ],
)
def test_parse_retry_delay_s_returns_none(stderr: str) -> None:
    assert _parse_retry_delay_s(stderr) is None


# ---------- 純関数: backoff 計算 ----------


def test_compute_backoff_prefers_parsed_value() -> None:
    # stderr に明示秒数があれば exp backoff より優先される
    wait = _compute_backoff_s(
        "retryDelay: 7s", attempt=3, base_s=2.0, cap_s=60.0
    )
    assert wait == pytest.approx(7.0)


def test_compute_backoff_falls_back_to_exponential() -> None:
    # 1: 2*1=2, 2: 2*2=4, 3: 2*4=8
    assert _compute_backoff_s("Quota exceeded", 1, base_s=2.0, cap_s=60.0) == 2.0
    assert _compute_backoff_s("Quota exceeded", 2, base_s=2.0, cap_s=60.0) == 4.0
    assert _compute_backoff_s("Quota exceeded", 3, base_s=2.0, cap_s=60.0) == 8.0


def test_compute_backoff_caps_parsed_value() -> None:
    # API が「3600 秒待て」と言ってきても cap で頭打ち
    wait = _compute_backoff_s(
        "retryDelay: 3600s", attempt=1, base_s=2.0, cap_s=60.0
    )
    assert wait == 60.0


def test_compute_backoff_caps_exponential() -> None:
    # 2 ** 20 は当然 cap を超える
    wait = _compute_backoff_s("Quota exceeded", 20, base_s=2.0, cap_s=60.0)
    assert wait == 60.0


# ---------- GeminiCLIEngine.run の retry loop ----------


def _make_engine(
    *,
    tmp_workdir: Path,
    max_retries: int = 3,
    backoff_base_s: float = 2.0,
    backoff_cap_s: float = 60.0,
) -> GeminiCLIEngine:
    """`create()` を経由せず engine を直接組み立てる (CLI / HOME を弄らない)."""
    cfg = Config(
        user="bridge-gemini-test",
        display_name=None,
        tenant=None,
        agent_hub_url="http://example.invalid/mcp",
        github_pat="ghp_test",
        gemini_api_key="key",
        gemini_model="gemini-2.5-flash",
        gemini_cli_path="gemini",
        workdir=tmp_workdir,
    )
    return GeminiCLIEngine(
        config=cfg,
        home_dir=tmp_workdir,  # 実際には触らない
        cli_path="/bin/true",
        timeout_s=10.0,
        max_retries=max_retries,
        backoff_base_s=backoff_base_s,
        backoff_cap_s=backoff_cap_s,
    )


@pytest.fixture
def no_sleep(monkeypatch):
    """`asyncio.sleep` を no-op に差し替えて test を高速化."""

    async def _noop(delay: float) -> None:
        return None

    import agent_hub_bridges.gemini.engine as engine_mod

    monkeypatch.setattr(engine_mod.asyncio, "sleep", _noop)


def _result(returncode: int, stderr: str = "", attempt: int = 1) -> EngineResult:
    return EngineResult(
        returncode=returncode,
        stdout="",
        stderr=stderr,
        duration_s=0.1,
        attempts=attempt,
    )


@pytest.mark.asyncio
async def test_run_succeeds_first_try(tmp_path, no_sleep, monkeypatch) -> None:
    engine = _make_engine(tmp_workdir=tmp_path)
    calls: list[int] = []

    async def fake_invoke(*, peer: str, prompt: str, attempt: int = 1) -> EngineResult:
        calls.append(attempt)
        return _result(0, attempt=attempt)

    monkeypatch.setattr(engine, "_invoke_once", fake_invoke)

    result = await engine.run(peer="@alice", prompt="hi")
    assert result.returncode == 0
    assert result.attempts == 1
    assert calls == [1]


@pytest.mark.asyncio
async def test_run_retries_on_rate_limit_then_succeeds(
    tmp_path, no_sleep, monkeypatch
) -> None:
    engine = _make_engine(tmp_workdir=tmp_path, max_retries=3)
    calls: list[int] = []

    async def fake_invoke(*, peer: str, prompt: str, attempt: int = 1) -> EngineResult:
        calls.append(attempt)
        if attempt < 2:
            return _result(
                1,
                stderr="Quota exceeded for metric: generate_content_free_tier_requests",
                attempt=attempt,
            )
        return _result(0, attempt=attempt)

    monkeypatch.setattr(engine, "_invoke_once", fake_invoke)

    result = await engine.run(peer="@alice", prompt="hi")
    assert result.returncode == 0
    assert result.attempts == 2
    assert calls == [1, 2]


@pytest.mark.asyncio
async def test_run_gives_up_after_max_retries(tmp_path, no_sleep, monkeypatch) -> None:
    engine = _make_engine(tmp_workdir=tmp_path, max_retries=2)
    calls: list[int] = []

    async def fake_invoke(*, peer: str, prompt: str, attempt: int = 1) -> EngineResult:
        calls.append(attempt)
        return _result(1, stderr="429 Quota exceeded", attempt=attempt)

    monkeypatch.setattr(engine, "_invoke_once", fake_invoke)

    result = await engine.run(peer="@alice", prompt="hi")
    # max_retries=2 → 初回 + 2 retry = 計 3 回
    assert calls == [1, 2, 3]
    assert result.returncode == 1
    assert result.attempts == 3
    assert "Quota exceeded" in result.stderr


@pytest.mark.asyncio
async def test_run_does_not_retry_non_rate_limit_failures(
    tmp_path, no_sleep, monkeypatch
) -> None:
    engine = _make_engine(tmp_workdir=tmp_path, max_retries=5)
    calls: list[int] = []

    async def fake_invoke(*, peer: str, prompt: str, attempt: int = 1) -> EngineResult:
        calls.append(attempt)
        return _result(1, stderr="Error: invalid API key", attempt=attempt)

    monkeypatch.setattr(engine, "_invoke_once", fake_invoke)

    result = await engine.run(peer="@alice", prompt="hi")
    assert calls == [1]  # retry されない
    assert result.returncode == 1
    assert result.attempts == 1


@pytest.mark.asyncio
async def test_run_with_zero_max_retries_is_one_shot(
    tmp_path, no_sleep, monkeypatch
) -> None:
    engine = _make_engine(tmp_workdir=tmp_path, max_retries=0)
    calls: list[int] = []

    async def fake_invoke(*, peer: str, prompt: str, attempt: int = 1) -> EngineResult:
        calls.append(attempt)
        return _result(1, stderr="429 Quota exceeded", attempt=attempt)

    monkeypatch.setattr(engine, "_invoke_once", fake_invoke)

    result = await engine.run(peer="@alice", prompt="hi")
    assert calls == [1]
    assert result.returncode == 1


@pytest.mark.asyncio
async def test_run_uses_parsed_retry_delay_for_sleep(
    tmp_path, monkeypatch
) -> None:
    """stderr の retryDelay が `asyncio.sleep` に渡る秒数に反映されるか."""
    engine = _make_engine(
        tmp_workdir=tmp_path, max_retries=2, backoff_base_s=2.0, backoff_cap_s=60.0
    )
    sleep_calls: list[float] = []

    async def fake_sleep(delay: float) -> None:
        sleep_calls.append(delay)

    import agent_hub_bridges.gemini.engine as engine_mod

    monkeypatch.setattr(engine_mod.asyncio, "sleep", fake_sleep)

    async def fake_invoke(*, peer: str, prompt: str, attempt: int = 1) -> EngineResult:
        if attempt == 1:
            return _result(1, stderr='Quota exceeded. "retryDelay": "7s"', attempt=1)
        return _result(0, attempt=attempt)

    monkeypatch.setattr(engine, "_invoke_once", fake_invoke)

    result = await engine.run(peer="@alice", prompt="hi")
    assert result.returncode == 0
    assert sleep_calls == [pytest.approx(7.0)]
