"""``ClaudeSDKClient`` lifecycle manager with in-place restart support.

The bare ``ClaudeSDKClient`` can only be opened once per ``async with``
block — there's no public API to swap the underlying session in place.
That's a problem for the ``/restart`` built-in (= agent-hub-sdk M6,
issue #26), which needs to "kill and respawn" the headless Claude
session without tearing down the hub-side ``AgentHub.connect`` or the
``hub.inbox()`` iterator that's currently driving the bridge's message
loop.

``ClaudeRunner`` solves this by owning the ``ClaudeSDKClient``'s
context-manager lifecycle and exposing a ``restart()`` method that
closes the old client and opens a fresh one in place. Callers read the
current client through ``runner.client`` on every use, so the inbox
loop naturally picks up the post-restart instance without any wiring
change beyond the property access.

Usage:

    async with ClaudeRunner(options) as runner:
        await _run_hub_session(config, runner)

    async def _run_hub_session(config, runner):
        router = CommandRouter()
        router.set_restart_handler(runner.restart)
        async with AgentHub.connect(...) as hub:
            async with hub.inbox(commands=router) as messages:
                async for msg in messages:
                    await _handle_one(hub, runner.client, msg, config)
                    await hub.ack(msg.id)

When ``/restart`` arrives, the SDK's built-in dispatcher:

  1. sends ``"restarting..."`` to the sender
  2. calls ``runner.restart()`` — old ``ClaudeSDKClient`` is closed,
     a new one with the same options is opened, conversation history
     (= per-peer ``session_id`` state inside Claude) is dropped
  3. sends ``"ready"`` (because ``restart`` returned normally)
  4. acks the original ``/restart`` message

If ``restart()`` raises, the SDK sends a generic warning reply
instead of ``"ready"`` and still acks. The bridge keeps running with
whatever state the partial restart left behind — the runner's own
internal try/finally ordering is designed to be **safe** on partial
failure (= no double-close, no dangling reference, no silent dead
client). On new-open failure, ``_client`` becomes ``None`` and
subsequent access raises a clear diagnostic rather than masking the
broken state.
"""

from __future__ import annotations

import logging
from typing import Self

from claude_agent_sdk import ClaudeAgentOptions, ClaudeSDKClient

logger = logging.getLogger(__name__)

__all__ = ["ClaudeRunner"]


class ClaudeRunner:
    """Async-context-manager wrapper around ``ClaudeSDKClient`` with
    an in-place ``restart()`` method.

    The runner manages the ``__aenter__`` / ``__aexit__`` of the
    underlying ``ClaudeSDKClient`` so callers don't have to think
    about the context-manager protocol — they just use ``runner``
    where they used to use the bare client, dereferencing
    ``runner.client`` per call (= so a concurrent restart is observed
    naturally on the next message).

    The runner is NOT thread-safe (= no internal lock). It assumes a
    single async task drives the inbox loop, which is true for
    bridge-claude's structure. If a future bridge runs the inbox
    loop and ``/restart`` dispatch in separate tasks, this contract
    needs revisiting.
    """

    def __init__(self, options: ClaudeAgentOptions) -> None:
        self._options = options
        self._client: ClaudeSDKClient | None = None

    async def __aenter__(self) -> Self:
        client = ClaudeSDKClient(options=self._options)
        await client.__aenter__()
        try:
            await client.connect()
        except BaseException:
            # If ``connect`` fails after ``__aenter__`` succeeded, we
            # still need to release the underlying resources; otherwise
            # the outer ``async with`` won't see a partially-opened
            # client to close.
            await client.__aexit__(None, None, None)
            raise
        self._client = client
        logger.info("Claude session started (initial open)")
        return self

    async def __aexit__(
        self,
        exc_type: type[BaseException] | None,
        exc_val: BaseException | None,
        exc_tb: object,
    ) -> None:
        if self._client is None:
            return
        client = self._client
        self._client = None
        await client.__aexit__(exc_type, exc_val, exc_tb)

    @property
    def client(self) -> ClaudeSDKClient:
        """The current live ``ClaudeSDKClient``.

        Raises ``RuntimeError`` if accessed outside the
        ``async with`` block (= before ``__aenter__`` or after
        ``__aexit__``). Callers should read this attribute on each
        use so a concurrent ``restart()`` is observed without any
        extra wiring.
        """
        if self._client is None:
            raise RuntimeError(
                "ClaudeRunner.client accessed outside the async with "
                "block (or after a failed restart). Open the runner "
                "as an async context manager first."
            )
        return self._client

    async def restart(self) -> None:
        """Tear down the current ``ClaudeSDKClient`` and open a new one.

        Called by the SDK's ``/restart`` built-in via the callback
        registered with ``CommandRouter.set_restart_handler``. Returns
        normally on success, in which case the SDK sends the ``"ready"``
        reply to the operator.

        The new client uses the same ``ClaudeAgentOptions`` the
        original was constructed with. Conversation history that the
        Claude SDK maintains per ``session_id`` is dropped — that's
        the entire point of /restart (= fresh context).

        Failure-mode contract:

        - If the *new* client fails to open, the old client has already
          been closed by this point (= best-effort close happens
          unconditionally before the new-open attempt). ``_client`` is
          left at ``None``; subsequent ``.client`` access raises
          ``RuntimeError`` with a clear diagnostic. The bridge no
          longer has a live Claude session until a subsequent
          successful restart or worker-level reconnect.
        - If closing the old client fails, we log + continue to opening
          the new one. A best-effort close is acceptable here because
          the operator explicitly asked for a fresh session — leaving
          the bridge half-open would be worse than letting some
          leaked transport time out.
        """
        if self._client is None:
            raise RuntimeError(
                "ClaudeRunner.restart called outside the async with "
                "block — open the runner first."
            )

        logger.info("/restart: tearing down current Claude session")
        old_client = self._client
        try:
            await old_client.__aexit__(None, None, None)
        except BaseException as exc:
            # Best-effort close. The new session is what matters; the
            # old one is going away regardless.
            logger.warning(
                "/restart: error closing old Claude session "
                "(continuing to re-spawn): %s",
                exc,
            )

        # Detach from the failed-close client BEFORE the new-open
        # attempt so that, if the new open also fails, ``self._client``
        # ends up ``None`` and subsequent ``client`` accesses raise a
        # clear error rather than silently re-using a dead reference.
        self._client = None

        logger.info("/restart: opening fresh Claude session")
        new_client = ClaudeSDKClient(options=self._options)
        await new_client.__aenter__()
        try:
            await new_client.connect()
        except BaseException:
            # Match the same partial-open recovery as __aenter__:
            # release the new client's resources, leave ``_client``
            # at None, propagate so the SDK reports the failure.
            await new_client.__aexit__(None, None, None)
            raise

        self._client = new_client
        logger.info("/restart: Claude session re-spawned (ready)")
