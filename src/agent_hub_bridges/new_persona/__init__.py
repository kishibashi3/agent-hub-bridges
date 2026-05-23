"""agent_hub_bridges.new_persona: persona spawn utility (issue #61).

Automates the manual steps of persona creation:
  1. gh repo create
  2. clone to --workdir
  3. copy meta-persona CLAUDE.md + rewrite self-awareness
  4. git commit + push
  5. bridge spawn (wait for "listening on inbox")
"""

from agent_hub_bridges import __version__

__all__ = ["__version__"]
