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

from datetime import UTC, datetime, timedelta

from agent_hub_sdk import IncomingMessage


def format_sent_at_line(timestamp: str, now: datetime) -> str:
    """prompt に入れる「送信時刻」の行を組む (issue #405).

    受け手の LLM が「いつ送られた便か」を知らないまま古い便を今日の話として
    読まないよう、server の timestamp をそのまま載せ、prompt を組み立てた
    時点 (``now``) との差を添える。Go 版 (bridge-claude2 / bridge-codex2 /
    bridge-tmux の ``formatSentAtLine``) と同じ規則:

      - timestamp が空なら行ごと省く (空文字列を返す)
      - タイムゾーン付きの ISO 8601 として読めなければ timestamp をそのまま
        載せ、経過時間は付けない
      - 経過時間が -1 分より小さい (時計のずれ) ときも経過時間は付けない
    """
    if not timestamp:
        return ""
    try:
        sent = datetime.fromisoformat(timestamp)
    except ValueError:
        return f"送信時刻: {timestamp}\n"
    if sent.tzinfo is None:
        return f"送信時刻: {timestamp}\n"
    elapsed = now - sent
    if elapsed < -timedelta(minutes=1):
        return f"送信時刻: {timestamp}\n"
    return f"送信時刻: {timestamp} (受信処理の {_format_elapsed(elapsed)})\n"


def _format_elapsed(elapsed: timedelta) -> str:
    """経過時間を「N 分前 / N 時間前 / N 日前」に丸める (切り捨て, issue #405)."""
    seconds = elapsed.total_seconds()
    if seconds < 60:
        return "1 分以内"
    if seconds < 3600:
        return f"{int(seconds // 60)} 分前"
    if seconds < 86400:
        return f"{int(seconds // 3600)} 時間前"
    return f"{int(seconds // 86400)} 日前"


def format_peer_message_prompt(
    msg: IncomingMessage,
    *,
    self_handle: str | None = None,
    reply_target: str | None = None,
    now: datetime | None = None,
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
        now: 経過時間の基準にする時刻 (タイムゾーン付き)。None なら
            現在の UTC。test で固定するための引数 (issue #405)。

    Returns:
        LLM の user turn にそのまま渡せる Japanese prompt 文字列。
    """
    reply_to = reply_target if reply_target is not None else msg.sender
    sent_at = format_sent_at_line(msg.timestamp, now if now is not None else datetime.now(UTC))

    intro = ""
    if self_handle is not None:
        intro = f"あなたは agent-hub の peer worker `{self_handle}` として動いています。\n"

    return (
        f"{intro}"
        f"agent-hub 経由で {msg.sender} から以下の message が届きました。\n"
        f"宛先: {msg.to}\n"
        f"{sent_at}"
        f"本文:\n{msg.body}\n\n"
        f"内容に応じて作業し、返答が必要なら "
        f"`mcp__agent-hub__send_message` で {reply_to} へ送り返してください。\n"
        f"その際、`caused_by` に今回の受信メッセージ ID `{msg.id}` を設定してください"
        f" (因果チェーン追跡 — issue #162)。"
    )
