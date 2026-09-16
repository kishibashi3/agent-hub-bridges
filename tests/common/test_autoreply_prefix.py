"""issue #275: Python bridge の auto 返信 prefix を bridge-claude2 と同じ形にそろえる。

bridge-claude2 は受信した本文が ``(auto) `` で始まるとき echo と判定して auto 返信しない。
各 worker の ``_AUTO_ERROR_PREFIX`` がこの形から外れると、echo 判定がエラーを出さずに
効かなくなるため、形式をここで固定する。
"""

from __future__ import annotations

import importlib

import pytest

_EXPECTED = {
    "claude": "(auto) bridge-claude error:",
    "claude_p": "(auto) bridge-claude-p error:",
    "gemini": "(auto) bridge-gemini error:",
    "codex": "(auto) bridge-codex error:",
    "client_codex": "(auto) client-codex error:",
    "a2a": "(auto) bridge-a2a error:",
}


@pytest.mark.parametrize(("module", "expected"), sorted(_EXPECTED.items()))
def test_auto_error_prefix(module: str, expected: str) -> None:
    worker = importlib.import_module(f"agent_hub_bridges.{module}.worker")
    assert worker._AUTO_ERROR_PREFIX == expected
    # bridge-claude2 の isAutoErrorEcho は共通 prefix `(auto) ` の前方一致で判定する
    assert worker._AUTO_ERROR_PREFIX.startswith("(auto) ")
