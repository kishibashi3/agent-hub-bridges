"""Internal helpers shared by all bridge implementations.

外向き API ではない (= top-level `_` で 内部 module であることを明示)。
本 module の公開シンボルは 各 bridge package の内部実装からのみ参照される。
互換性 contract は monorepo 内に限定し、 user-facing semantic versioning の
対象外。

抽出方針:
  - **共通する** boilerplate (Config の env loader、 argparse 引数、 outer
    reconnect loop、 ExceptionGroup の log 整形、 LLM 系 bridge の peer
    prompt 整形) のみここに置く。
  - bridge ごとに差分が出る部分 (task group 構造、 engine 抽象、 thread
    routing 等) は ここに置かず 各 bridge 内で実装する。

詳細は `docs/design.md` § "_common 抽出ポリシー" を参照。
"""

from agent_hub_bridges._common.exc import summarize_exc
from agent_hub_bridges._common.prompt import format_peer_message_prompt

__all__ = [
    "format_peer_message_prompt",
    "summarize_exc",
]
