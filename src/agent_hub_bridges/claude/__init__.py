"""agent_hub_bridges.claude: Stateful Claude bridge (placeholder for M1).

実コードは M1 で `agent-hub-bridge-claude` repo から移植する (issue 別途)。
M0 の段階では `cli.main` が「未実装」を表示して exit 1 する stub のみ。

`pip install "agent-hub-bridges[claude]"` で `claude-agent-sdk` も pull
される。 M0 では import はせず stub だけ提供する。
"""

from agent_hub_bridges import __version__

__all__ = ["__version__"]
