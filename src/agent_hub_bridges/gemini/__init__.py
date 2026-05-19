"""agent_hub_bridges.gemini: Stateful Gemini bridge (placeholder for M3).

実コードは M3 で `agent-hub-bridge-gemini` repo から移植する (issue 別途)。
移植と同時に 旧 `hub.py` (= 自前 HubClient) を捨てて agent-hub-sdk へ
切り替える (operator DM で合意済 — 5 質問 C)。 M0 の段階では stub のみ。
"""

from agent_hub_bridges import __version__

__all__ = ["__version__"]
