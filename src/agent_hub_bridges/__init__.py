"""agent-hub-bridges: monorepo of agent-hub bridge workers.

ここでは **重い sub-package を eager import しない**。 ユーザが
`pip install "agent-hub-bridges[slack]"` のように 特定 extra だけ
入れた場合、 import されていない bridge の依存ライブラリは未インストール
の可能性がある。 例えば `claude_agent_sdk` を `agent_hub_bridges.claude`
の import 時にだけ要求する形にしておけば、 `[slack]` だけのユーザが
`import agent_hub_bridges` しても落ちない。

各 bridge は自身の `agent_hub_bridges.<name>` package を明示的に
import すること。
"""

__version__ = "0.3.2"

__all__ = ["__version__"]
