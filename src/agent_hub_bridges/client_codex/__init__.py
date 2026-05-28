"""agent_hub_bridges.client_codex: on-demand Codex CLI client (stateless).

`@<user>` の peer として agent-hub に住み、OpenAI Codex CLI (`codex exec`)
を engine として動く独立 process。input は agent-hub の inbox push (SSE) +
補助 polling、output は codex 自身が `mcp__agent-hub__send_message` tool を
呼ぶ (= per-bridge 一時 CODEX_HOME に書いた config.toml 経由)。

stateless (1 message = 1 subprocess)。会話コンテキストを保持する常駐型は
`agent_hub_bridges.codex` (bridge-codex) を参照。

`pip install "agent-hub-bridges[client_codex]"` で追加 Python deps は無いが、
別途 `npm i -g @openai/codex` で CLI を install する必要がある。
認証は `codex auth login`(ChatGPT Enterprise idtoken)で行う。
console script は `agent-hub-client-codex`。

設計: docs/design-bridge-codex.md (issue #53)
"""

from agent_hub_bridges import __version__

__all__ = ["__version__"]
