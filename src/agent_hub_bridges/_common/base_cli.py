"""全 bridge で 共通する argparse 引数のビルダー.

旧 bridge-claude / bridge-slack / bridge-gemini が ほぼ同じ argparse 定義
(`--user` / `--display-name` / `--tenant` / `--workdir`) を 個別に書いて
いたので 1 つに集約。 bridge 固有の引数 (例: gemini の `--model`、
slack の `--default-channel`) は 各 bridge 側で `add_argument` を 追加する。

`--user` の semantics は bridge ごとに微妙に違うので 本ヘルパでは扱わない:
  - bridge-claude / bridge-gemini : required
  - bridge-slack : optional (default `"slack-bot"`、 env AGENT_HUB_USER fallback)
  - bridge-a2a : optional (default は a2a agent name の slugify)
"""

from __future__ import annotations

import argparse


def build_common_parser(
    *,
    prog: str,
    description: str,
    version: str,
) -> argparse.ArgumentParser:
    """`--display-name` / `--tenant` / `--workdir` / `--version` だけ
    付けた parser を返す.

    `--user` は 各 bridge で semantics が違う (required / default 有り等)
    ので呼出側で `parser.add_argument("--user", ...)` を 別途追加する。

    Args:
        prog: argparse の prog (= `agent-hub-bridge-claude` 等)。
        description: --help の冒頭に出る説明文。
        version: 各 bridge の `__version__` (現状は monorepo 全体で同一)。

    Returns:
        共通引数だけ追加済みの `ArgumentParser`。 呼出側で `add_argument`
        を 続けてよい。
    """
    parser = argparse.ArgumentParser(prog=prog, description=description)
    parser.add_argument(
        "--version", action="version", version=f"%(prog)s {version}"
    )
    parser.add_argument(
        "--display-name",
        default=None,
        help="表示名 (任意)。 未指定なら env AGENT_HUB_DISPLAY_NAME を使う。",
    )
    parser.add_argument(
        "--tenant",
        default=None,
        help="tenant 名。 未指定なら default tenant (雑談室) に入る。",
    )
    parser.add_argument(
        "--workdir",
        default=None,
        help="作業対象 project root。 未指定なら現在の cwd。 relay 系 bridge は無視。",
    )
    return parser
