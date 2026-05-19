"""agent_hub_bridges.claude: Stateful Claude bridge.

`@<user>` の peer として agent-hub に住み、 Claude Agent SDK を engine として
動く独立 process。 input は agent-hub の inbox push (SSE) + 補助 polling。
output は `agent_hub_sdk` 経由の `send_message`、 もしくは Claude 自身が
`mcp__agent-hub__send_message` tool を呼ぶ。

`pip install "agent-hub-bridges[claude]"` で `claude-agent-sdk` も install
される。 console script は `agent-hub-bridge-claude` (= 旧 repo と同名、
後方互換)。
"""

from agent_hub_bridges import __version__

__all__ = ["__version__"]
