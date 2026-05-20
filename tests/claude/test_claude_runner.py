"""Unit tests for :class:`ClaudeRunner` — the M6 ``/restart`` integration
wrapper around ``ClaudeSDKClient``.

These tests don't reach the real ``claude-agent-sdk`` transport: they
``patch`` ``ClaudeSDKClient`` at the module-lookup point so we can drive
``__aenter__`` / ``__aexit__`` / ``connect`` outcomes via
``AsyncMock`` and verify the runner's lifecycle + in-place-restart
semantics without needing a live Claude session.

Coverage is the 4 paths the M6 L1 PR review (`Suggestion 1`) called
out, plus 2 edge cases for the ``client`` accessor:

  1. Normal ``__aenter__`` + ``__aexit__`` (happy lifecycle).
  2. ``__aenter__`` with a ``connect()`` failure (= the inner partial-
     -open recovery path must release the underlying client).
  3. ``restart()`` success — old client closes, new client opens,
     ``runner.client`` returns the new instance, the outer ``__aexit__``
     then tears down only the new instance.
  4. ``restart()`` failure when the new client's ``connect()`` raises —
     ``_client`` is dropped to ``None``, subsequent ``client`` access
     raises a clear ``RuntimeError``, and the outer ``__aexit__`` is a
     safe no-op (= no double-close on a partially-opened instance).
  5. ``client`` accessed outside the ``async with`` block raises.
  6. ``restart()`` called outside the ``async with`` block raises.
"""

from __future__ import annotations

from unittest.mock import AsyncMock, MagicMock

import pytest

from agent_hub_bridges.claude.claude_runner import ClaudeRunner


@pytest.fixture
def options() -> MagicMock:
    """Sentinel options object — the runner just passes it through to
    ``ClaudeSDKClient(options=...)``. The contents are never inspected
    in these tests because ``ClaudeSDKClient`` is patched."""
    return MagicMock(name="ClaudeAgentOptions")


def _make_async_client_instance() -> AsyncMock:
    """Build a fresh ``ClaudeSDKClient`` stand-in.

    ``AsyncMock()`` already returns awaitable attrs by default; the
    methods ``__aenter__`` / ``__aexit__`` / ``connect`` are all
    awaited by ``ClaudeRunner``. ``__aenter__`` returns ``self`` by
    convention (though the runner doesn't rely on it).
    """
    instance = AsyncMock(name="ClaudeSDKClient-instance")
    instance.__aenter__.return_value = instance
    return instance


@pytest.mark.asyncio
async def test_path1_normal_lifecycle(
    options: MagicMock, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Path 1: ``async with ClaudeRunner(...) as runner:`` opens the
    client (``__aenter__`` + ``connect``), exposes it via the
    ``client`` property, and closes it (``__aexit__``) on exit."""
    instance = _make_async_client_instance()
    fake_ctor = MagicMock(name="ClaudeSDKClient-ctor", return_value=instance)
    monkeypatch.setattr(
        "agent_hub_bridges.claude.claude_runner.ClaudeSDKClient", fake_ctor
    )

    async with ClaudeRunner(options) as runner:
        # Constructor was called with the options we passed.
        fake_ctor.assert_called_once_with(options=options)
        # __aenter__ + connect were awaited in order.
        instance.__aenter__.assert_awaited_once()
        instance.connect.assert_awaited_once()
        # client property returns the live instance.
        assert runner.client is instance
        # __aexit__ has not been called yet (we're still inside the block).
        instance.__aexit__.assert_not_awaited()

    # On exit, __aexit__ is awaited exactly once.
    instance.__aexit__.assert_awaited_once()


@pytest.mark.asyncio
async def test_path2_aenter_connect_failure_releases_client(
    options: MagicMock, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Path 2: if ``connect()`` raises inside ``__aenter__``, the
    runner must call ``__aexit__`` on the already-opened underlying
    client (= release resources) and propagate the exception.

    Without this recovery, the partially-opened client would leak — no
    outer ``async with`` block exists to clean it up because the
    runner's own ``__aenter__`` is the one that's raising."""
    instance = _make_async_client_instance()
    instance.connect.side_effect = RuntimeError("connect bombed")
    fake_ctor = MagicMock(return_value=instance)
    monkeypatch.setattr(
        "agent_hub_bridges.claude.claude_runner.ClaudeSDKClient", fake_ctor
    )

    with pytest.raises(RuntimeError, match="connect bombed"):
        async with ClaudeRunner(options):
            pass  # pragma: no cover — the body should never run

    # __aenter__ succeeded, then connect raised, then the runner's
    # recovery called __aexit__ on the underlying client.
    instance.__aenter__.assert_awaited_once()
    instance.connect.assert_awaited_once()
    instance.__aexit__.assert_awaited_once()


@pytest.mark.asyncio
async def test_path3_restart_swaps_client(
    options: MagicMock, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Path 3: ``restart()`` closes the old client and opens a new
    one. ``runner.client`` returns the new instance; the outer
    ``async with`` exit tears down only the new instance (= one
    ``__aexit__`` call per *current* client at each lifecycle point,
    not on stale references)."""
    instances: list[AsyncMock] = []

    def make_instance(**_kwargs: object) -> AsyncMock:
        m = _make_async_client_instance()
        instances.append(m)
        return m

    fake_ctor = MagicMock(side_effect=make_instance)
    monkeypatch.setattr(
        "agent_hub_bridges.claude.claude_runner.ClaudeSDKClient", fake_ctor
    )

    async with ClaudeRunner(options) as runner:
        # Path-3-A: initial open uses the first instance.
        assert len(instances) == 1
        old = runner.client
        assert old is instances[0]

        # Path-3-B: restart triggers close of old + open of new.
        await runner.restart()

        assert len(instances) == 2
        new = runner.client
        assert new is instances[1]
        assert new is not old

        # The old instance was closed during restart.
        instances[0].__aexit__.assert_awaited_once()
        # The new instance was opened (entered + connect).
        instances[1].__aenter__.assert_awaited_once()
        instances[1].connect.assert_awaited_once()
        # The new instance has not been exited yet.
        instances[1].__aexit__.assert_not_awaited()

    # Outer __aexit__ closes only the new (current) client.
    # The old client's __aexit__ count stays at 1 (= the restart-time
    # close); the runner does not double-close it on outer exit.
    assert instances[0].__aexit__.await_count == 1
    instances[1].__aexit__.assert_awaited_once()


@pytest.mark.asyncio
async def test_path4_restart_new_client_open_failure(
    options: MagicMock, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Path 4: when the new client's ``connect()`` raises inside
    ``restart()``, the runner releases the new client's resources,
    drops ``_client`` to ``None``, and re-raises. Subsequent ``client``
    accesses raise ``RuntimeError`` with the documented diagnostic.
    The outer ``async with`` exit is a safe no-op (= no double-close)."""
    instances: list[AsyncMock] = []
    call_count = 0

    def make_instance(**_kwargs: object) -> AsyncMock:
        nonlocal call_count
        call_count += 1
        m = _make_async_client_instance()
        if call_count == 2:
            # The replacement client fails on connect.
            m.connect.side_effect = RuntimeError("respawn bombed")
        instances.append(m)
        return m

    fake_ctor = MagicMock(side_effect=make_instance)
    monkeypatch.setattr(
        "agent_hub_bridges.claude.claude_runner.ClaudeSDKClient", fake_ctor
    )

    async with ClaudeRunner(options) as runner:
        assert runner.client is instances[0]

        # restart() raises because the new client's connect bombed.
        with pytest.raises(RuntimeError, match="respawn bombed"):
            await runner.restart()

        # Old instance was closed (best-effort during restart).
        instances[0].__aexit__.assert_awaited_once()
        # New instance entered + connect attempted, then released after
        # the connect failure (= partial-open recovery).
        instances[1].__aenter__.assert_awaited_once()
        instances[1].connect.assert_awaited_once()
        instances[1].__aexit__.assert_awaited_once()

        # The runner's ``_client`` is now ``None`` — subsequent
        # ``client`` access raises with the documented diagnostic.
        with pytest.raises(RuntimeError, match="after a failed restart"):
            _ = runner.client

    # Outer __aexit__ is a no-op (= _client is None). No additional
    # __aexit__ on either instance beyond what restart() already did.
    assert instances[0].__aexit__.await_count == 1
    assert instances[1].__aexit__.await_count == 1


@pytest.mark.asyncio
async def test_client_before_aenter_raises(options: MagicMock) -> None:
    """Edge case A: accessing ``client`` before ``__aenter__`` raises
    a clear diagnostic (= the runner is not yet open)."""
    runner = ClaudeRunner(options)
    with pytest.raises(RuntimeError, match="outside the async with"):
        _ = runner.client


@pytest.mark.asyncio
async def test_restart_before_aenter_raises(options: MagicMock) -> None:
    """Edge case B: calling ``restart()`` before ``__aenter__`` raises
    a clear diagnostic. The L1 PR's CommandRouter wiring always
    registers ``runner.restart`` after the runner is opened, but this
    guard catches misuse where the wiring order is reversed."""
    runner = ClaudeRunner(options)
    with pytest.raises(RuntimeError, match="outside the async with"):
        await runner.restart()
