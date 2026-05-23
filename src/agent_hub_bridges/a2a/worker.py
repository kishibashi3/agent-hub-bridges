"""Bridge worker main loop (a2a, no-LLM protocol translator).

概要 (= `kishibashi3/agent-hub#94` spec):
  - 起動時に 外部 A2A agent endpoint から **Agent Card** を fetch
  - Agent Card の `name` / `description` を 流用して agent-hub に register
  - hub.inbox() の 各 message を A2A `send_message` に forward
  - A2A side の `StreamResponse` を text に collect して `hub.send` で
    sender に返す
  - LLM を 経由しないので prompt 整形は しない (= body を そのまま転送)

reconnect は `_common.reconnect.run_with_reconnect` で claude/gemini と
同 pattern。 a2a-sdk の Client は 1 hub session 中 1 度だけ open する
(= TaskGroup 構造は 単純な 1-task)。

NOTE: a2a-sdk 1.0.3 は protobuf-based の type を 公開しているため、
`SendMessageRequest` / `Message` / `Part` は `a2a.types.a2a_pb2` から
import する。 stream response の `message.parts` を 順に巡って `text`
フィールドを 連結する。
"""

from __future__ import annotations

import logging
import sys
import uuid

import httpx
from a2a.client import A2ACardResolver, Client, ClientConfig, create_client
from a2a.types.a2a_pb2 import (
    ROLE_USER,
    AgentCard,
    Message,
    Part,
    SendMessageRequest,
    StreamResponse,
)
from agent_hub_sdk import AgentHub, HubSession, IncomingMessage

from agent_hub_bridges._common.reconnect import run_with_reconnect
from agent_hub_bridges.a2a.config import Config

logger = logging.getLogger(__name__)


def _extract_reply_text_from_response(response: StreamResponse) -> tuple[str, int]:
    """1 つの StreamResponse から text と non-text skip 数を取り出す.

    issue #14 item 1: streaming aggregation の per-chunk helper。
    ``_handle_one`` は ``list[StreamResponse]`` を heap に保持せず、
    chunk が届くたびにこの関数を呼んで text を即時蓄積する。

    Returns:
        ``(text, skipped)`` — text は chunk 内 text part を ``\\n`` 連結した文字列。
        message field が unset の場合は ``("", 0)``。
    """
    if not response.HasField("message"):
        return "", 0
    parts: list[str] = []
    skipped = 0
    for part in response.message.parts:
        if part.text:
            parts.append(part.text)
        else:
            skipped += 1
    return "\n".join(parts), skipped


def _assemble_reply(
    text_parts: list[str],
    skipped: int,
    interrupted: str | None = None,
) -> str:
    """集めた text parts・skip 数・中断理由から最終メッセージ文字列を組み立てる.

    issue #14 item 1 & 2: streaming aggregation と partial-chunk 結合の共通ロジック。

    注記の形式:
      - non-text parts: ``_(non-text parts omitted: N)_``
      - stream 中断: ``_(stream interrupted: ExcType: msg)_``
    注記が複数ある場合は改行で連結する。text が空でも注記は残す。

    Args:
        text_parts: chunk ごとに抽出した text の list。
        skipped: 全 chunk を通じた non-text part の総スキップ数。
        interrupted: stream が途中で途切れた場合の理由文字列 (``type(exc).__name__: str(exc)``)。
            完走した場合は ``None``。

    Returns:
        agent-hub に ``hub.send`` で流せる文字列。text も注記もない場合は空文字。
    """
    text = "\n".join(text_parts)
    notes: list[str] = []
    if skipped:
        notes.append(f"_(non-text parts omitted: {skipped})_")
    if interrupted:
        notes.append(f"_(stream interrupted: {interrupted})_")
    if not notes:
        return text
    note_block = "\n".join(notes)
    return (text + "\n" + note_block) if text else note_block


def _extract_reply_text(stream: list[StreamResponse]) -> str:
    """A2A stream response list から 人間向け text を 取り出す (後方互換 API).

    `StreamResponse.message.parts[*].text` を 順に 連結する。 text 以外の
    part (= raw / url / data / file) は 当面 無視し、 後ろに 「(non-text
    parts omitted)」 と 注記を 付ける (= 取りこぼしを ops から 検知できる
    ように)。

    NOTE: ``_handle_one`` では issue #14 item 1 の streaming aggregation を使うため
    この関数は直接呼ばない。テスト・外部用途のために API を残す。
    内部実装は ``_extract_reply_text_from_response`` + ``_assemble_reply`` に委譲。

    Args:
        stream: A2A ``Client.send_message`` から集めた StreamResponse list。

    Returns:
        agent-hub にそのまま ``hub.send`` で流せる text 文字列。全 part が
        空でも空文字を返す (= 呼出側で落とす判断はしない)。
    """
    text_parts: list[str] = []
    skipped = 0
    for response in stream:
        chunk_text, chunk_skipped = _extract_reply_text_from_response(response)
        if chunk_text:
            text_parts.append(chunk_text)
        skipped += chunk_skipped
    return _assemble_reply(text_parts, skipped)


def _build_send_message_request(body: str) -> SendMessageRequest:
    """hub から 受け取った body を A2A `SendMessageRequest` に詰める.

    role は `ROLE_USER` (= bridge は 人間/上流 agent の代理として 投げる、
    protobuf enum、 a2a-sdk v1.0 spec)、 parts は text 1 つだけの最小構成。
    message_id は UUID で 生成 (= A2A side で context 追跡を したい場合の
    手がかり)。 tenant / configuration は 当面 default のまま (= future
    scope)。
    """
    message = Message(
        message_id=str(uuid.uuid4()),
        role=ROLE_USER,
        parts=[Part(text=body)],
    )
    return SendMessageRequest(message=message)


def _derive_display_name(card: AgentCard, fallback: str) -> str:
    """Agent Card から display_name に 使える文字列を取り出す.

    優先順: `card.description` > `card.name` > fallback (= bridge `--user`)。
    描画上 `description` の方が情報量があるが、 空のことも多いので
    name → fallback と倒す。
    """
    if card.description:
        return card.description
    if card.name:
        return card.name
    return fallback


async def run_worker(config: Config) -> None:
    """Bridge worker メインループ.

    `httpx.AsyncClient` と `a2a.client.Client` は 外側で 1 度だけ open し、
    hub 再接続には 巻き込まない (= `_common.run_with_reconnect` の outer
    loop の外で 保持)。 hub session ごとに register + inbox loop を 回す。
    """
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
        stream=sys.stderr,
    )

    logger.info(
        "Starting agent-hub-bridge-a2a as @%s (tenant=%s, a2a_url=%s)",
        config.user,
        config.tenant or "default",
        config.a2a_agent_url,
    )

    async with httpx.AsyncClient() as http_client:
        # 1. Agent Card を resolve (= 起動疎通の確認も兼ねる)
        resolver = A2ACardResolver(
            httpx_client=http_client,
            base_url=config.a2a_agent_url,
            agent_card_path=config.a2a_agent_card_path,
        )
        card = await resolver.get_agent_card()
        logger.info(
            "Resolved A2A agent card: name=%r version=%r",
            card.name,
            card.version,
        )

        # 2. A2A Client を作成 (= card を渡す形、 内部で 適切な transport を
        #    選択する)。
        a2a_client = await create_client(
            card,
            ClientConfig(httpx_client=http_client),
        )

        try:

            async def _one_session() -> None:
                await _run_hub_session(config, card, a2a_client)

            await run_with_reconnect(_one_session, name="hub session (a2a)")
        finally:
            await a2a_client.close()


async def _run_hub_session(
    config: Config,
    card: AgentCard,
    a2a_client: Client,
) -> None:
    """1 回分の hub session lifecycle.

    AgentHub.connect → register (Agent Card で display_name 推定) →
    inbox iterator を 1 件ずつ A2A に forward → reply を hub に send_back。
    """
    display_name = config.display_name or _derive_display_name(card, config.user)

    async with AgentHub.connect(
        user=config.user,
        mode="stateful",
        tenant=config.tenant,
        display_name=display_name,
        url=config.agent_hub_url,
        pat=config.github_pat,
    ) as hub:
        registered = await hub.register()
        logger.info(
            "Hub session ready (%s), listening on inbox...",
            registered.splitlines()[0] if registered else "(no body)",
        )

        async with hub.inbox() as messages:
            async for msg in messages:
                await _handle_one(hub, a2a_client, msg, config)
                await hub.ack(msg.id)


async def _handle_one(
    hub: HubSession,
    a2a_client: Client,
    msg: IncomingMessage,
    config: Config,
) -> None:
    """message 1 件を A2A に forward して reply を hub に戻す.

    自分自身宛の echo (= team broadcast 自己反射) は loop の種なので skip。
    A2A から 返ってきた text が 空文字なら hub.send は 呼ばない (= server
    側で 空 message が reject される + UX 上 ノイズなので)。

    issue #14 item 1 — streaming aggregation:
      ``list[StreamResponse]`` を heap に保持せず、 chunk 到着ごとに
      ``_extract_reply_text_from_response`` で text を即時蓄積する。
      長文 RAG 系 A2A agent が数十 MB の stream を流しても bridge の
      memory 消費は O(text size) に留まる (protobuf object を保持しない)。

    issue #14 item 2 — partial-chunk 結合:
      stream 途中で例外が出た場合、それまでに蓄積した text があれば
      ``_(stream interrupted: ExcType: msg)_`` 注記を付けて送信する。
      何も届いていない場合は従来通りエラーのみ通知。
    """
    self_handle = f"@{config.user}"
    if msg.sender == self_handle:
        logger.info("Skipping self-sent message %s (avoid loop)", msg.id)
        return

    logger.info("← message %s from %s: %s", msg.id, msg.sender, msg.body[:120])

    request = _build_send_message_request(msg.body)

    # issue #14 item 1: streaming aggregation。
    # list に全件集めず、chunk ごとに text を即時抽出して蓄積する。
    text_parts: list[str] = []
    skipped_total = 0
    interrupted: str | None = None
    chunk_count = 0

    try:
        async for response in a2a_client.send_message(request):
            chunk_text, chunk_skipped = _extract_reply_text_from_response(response)
            if chunk_text:
                text_parts.append(chunk_text)
            skipped_total += chunk_skipped
            chunk_count += 1
    except Exception as exc:
        logger.exception("A2A send_message failed for message %s: %s", msg.id, exc)
        if not text_parts and not skipped_total:
            # partial も何もない → 従来通りエラーのみ通知。
            try:
                await hub.send(
                    to=msg.sender,
                    message=(
                        f"(自動応答) A2A agent でエラー: "
                        f"{type(exc).__name__}: {exc}"
                    ),
                )
            except Exception:
                logger.exception("fallback send to sender also failed")
            return
        # issue #14 item 2: partial text あり → partial + 中断注記を結合して送信。
        interrupted = f"{type(exc).__name__}: {exc}"
        logger.warning(
            "A2A stream interrupted after %d chunk(s) for message %s; "
            "sending partial reply with interruption note",
            chunk_count,
            msg.id,
        )

    reply_text = _assemble_reply(text_parts, skipped_total, interrupted)
    if not reply_text:
        logger.info(
            "✓ processed %s from %s (a2a responded with empty text, skipping hub.send)",
            msg.id,
            msg.sender,
        )
        return

    await hub.send(to=msg.sender, message=reply_text)
    logger.info(
        "✓ processed %s from %s (%d chunk(s), %d chars sent%s)",
        msg.id,
        msg.sender,
        chunk_count,
        len(reply_text),
        " [partial]" if interrupted else "",
    )
