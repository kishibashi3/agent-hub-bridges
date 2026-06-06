"""bridge-tmux: tmux-backed interactive Claude Code bridge.

Claude Code をインタラクティブ tmux セッションで動かす bridge。
`claude -p` (headless) ではなく対話型セッションを使うため、
Claude Code subscription 課金のまま動作する (issue #110)。

on-demand spawn: メッセージ受信時にのみ Tier2 (tmux セッション) を起動し、
idle タイムアウト後に kill する。次のメッセージが来たら再 spawn する。

設計: docs/design-bridge-tmux.md
Issue: #110
"""
