"""Stub CLI entry for `agent-hub-bridge-gemini` (M0; real impl in M3)."""

from __future__ import annotations

import sys


def main(argv: list[str] | None = None) -> int:
    """Stub entry point. Real implementation lands in M3."""
    del argv
    print(
        "agent-hub-bridge-gemini: M0 stub. "
        "Real implementation lands in M3 (port + SDK migration from "
        "agent-hub-bridge-gemini repo).",
        file=sys.stderr,
    )
    return 1


if __name__ == "__main__":
    sys.exit(main())
