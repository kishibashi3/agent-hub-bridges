"""agent_hub_bridges.slack: Slack relay bridge.

Slack workspace を peer として agent-hub に つなぐ relay bridge (= LLM
engine は持たない)。 旧 repo `agent-hub-bridge-slack` (M5_sdk 完了状態) を
1:1 移植したもの。

`pip install "agent-hub-bridges[slack]"` で `slack-bolt` / `slack-sdk` /
`aiohttp` も install される。 console script は `agent-hub-bridge-slack`
(= 旧 repo と同名、 後方互換)。
"""

from agent_hub_bridges import __version__

__all__ = ["__version__"]
