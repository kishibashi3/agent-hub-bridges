"""bridge-codex — Codex CLI を使った agent-hub resident-process bridge.

`codex exec` を 1 メッセージごとに subprocess として起動し、codex が
agent-hub MCP tool 経由で会話履歴確認 (get_user_history) と返信 (send_message)
を行う設計。

設計: docs/design-bridge-codex.md
Issue: #53, #77
"""
