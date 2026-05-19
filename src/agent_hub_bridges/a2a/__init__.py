"""agent_hub_bridges.a2a: A2A client bridge (placeholder for M4).

設計仕様は `kishibashi3/agent-hub#94` を参照。 外部 A2A agent への client
(= 呼びに行く側、 server ではない)。 LLM を持たない pure protocol
translator として実装する (scheduler と同じ構造)。

M0 の段階では stub のみ。 M4 で実装着手。
"""

from agent_hub_bridges import __version__

__all__ = ["__version__"]
