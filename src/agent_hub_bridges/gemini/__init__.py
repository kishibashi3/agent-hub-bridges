"""agent_hub_bridges.gemini: Stateful Gemini bridge.

`@<user>` の peer として agent-hub に住み、 `gemini` CLI (npm:
`@google/gemini-cli`) を engine として 動く独立 process。 input は
agent-hub の inbox push (SSE) + 補助 polling、 output は gemini 自身が
`mcp__agent-hub__send_message` tool を呼ぶ (= isolated HOME に書いた
settings.json 経由)。

`pip install "agent-hub-bridges[gemini]"` で 追加 Python deps は無いが、
別途 `npm i -g @google/gemini-cli` で CLI を install する必要がある。
console script は `agent-hub-bridge-gemini` (= 旧 repo と同名、 後方互換)。

M3 で 旧 repo の自前 `HubClient` (= 旧 `hub.py`) を 削除し、
`agent_hub_sdk.AgentHub` + `hub.inbox()` に切替済。 claude / slack と
同じ SDK 経由 pattern。
"""

from agent_hub_bridges import __version__

__all__ = ["__version__"]
