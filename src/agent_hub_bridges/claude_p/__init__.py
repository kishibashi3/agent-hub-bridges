"""agent_hub_bridges.claude_p: on-demand claude -p bridge.

`@<user>` の peer として agent-hub に住み、Claude Code print mode
(`claude -p`) を engine として動く軽量 watchdog bridge。

常駐プロセス自体は LLM 不使用。DM を受信したときだけ `claude -p` を
subprocess として起動し、処理後に終了する。Claude Agent SDK ではなく
`claude -p` subprocess を使うことで、常駐コストゼロ・subscription 課金を
期待する構成 (issue #54 動機)。

設計: docs/design-bridge-claude-p.md (issue #54)
"""

from agent_hub_bridges import __version__

__all__ = ["__version__"]
