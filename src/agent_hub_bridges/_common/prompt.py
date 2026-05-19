"""LLM 系 bridge が peer message を LLM への user prompt に変換するヘルパ.

bridge-claude / bridge-gemini が 全く同じ形の prompt を作っていたので
共通化した。 bridge-slack のように LLM に渡さない relay bridge は
本 helper を 使わない。

設計指針:
  - 「誰から / 宛先 / 本文」を必ず明示。 LLM が context を取り違えない
    ためのプリアンブル。
  - 「`mcp__agent-hub__send_message` で sender に返信せよ」を末尾に置く。
    LLM が tool 呼出先を間違えるリスクを下げる。
  - team broadcast を避ける運用が多いので、 hint をそっと添える
    (`reply_target` で 明示上書き可能)。

NOTE: M0 では `_IncomingMessageLike` Protocol で 「SDK 移行途中の
bridge (= gemini が旧 HubClient.IncomingMessage を使っていた)」 にも
対応していたが、 M3 (= gemini SDK 移行) 完了で 全 bridge が
`agent_hub_sdk.IncomingMessage` を使うようになったため、 Protocol を
削除し 直接 import に置換した (PR #2 review Minor 2)。
"""

from __future__ import annotations

from agent_hub_sdk import IncomingMessage


def format_peer_message_prompt(
    msg: IncomingMessage,
    *,
    self_handle: str | None = None,
    reply_target: str | None = None,
) -> str:
    """受信 peer message を LLM への user prompt に整形する.

    Args:
        msg: agent-hub から届いた 1 件の message
            (`agent_hub_sdk.IncomingMessage`)。
        self_handle: 自分自身の `@handle` (例 `"@claude-impl"`)。 None なら
            プリアンブルから自己紹介行を省く。
        reply_target: 返信先 `@handle`。 None なら `msg.sender` に返す
            (= 一般的な DM の返信先)。 team 宛 message を 個別 DM で
            返したい等の例外時に上書きする。

    Returns:
        LLM の user turn にそのまま渡せる Japanese prompt 文字列。
    """
    reply_to = reply_target if reply_target is not None else msg.sender

    intro = ""
    if self_handle is not None:
        intro = f"あなたは agent-hub の peer worker `{self_handle}` として動いています。\n"

    return (
        f"{intro}"
        f"agent-hub 経由で {msg.sender} から以下の message が届きました。\n"
        f"宛先: {msg.to}\n"
        f"本文:\n{msg.body}\n\n"
        f"内容に応じて作業し、返答が必要なら "
        f"`mcp__agent-hub__send_message` で {reply_to} へ送り返してください。"
    )
