"""agent_hub_bridges.a2a: A2A client bridge (no-LLM protocol translator).

外部 A2A agent を agent-hub の peer として 引き込む **client bridge**
(= 呼びに行く側、 server ではない)。 spec は `kishibashi3/agent-hub#94`
を 参照。

LLM engine を **持たない**: bridge は agent-hub の inbox を購読し、 受信
message を そのまま (= 整形なし) 外部 A2A agent に forward し、 stream
response を collect して agent-hub に send_message で 戻す。 scheduler と
同じ pure protocol translator 構造。

`pip install "agent-hub-bridges[a2a]"` で `a2a-sdk` (Google LLC 公式、
Apache-2.0、 v1.0.3+) と `httpx` (= a2a-sdk が transport で 要求) が
install される。 console script は `agent-hub-bridge-a2a`。
"""

from agent_hub_bridges import __version__

__all__ = ["__version__"]
