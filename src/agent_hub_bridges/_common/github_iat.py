"""GitHub App Installation Access Token (IAT) manager.

When GITHUB_APP_ID / GITHUB_APP_PRIVATE_KEY / GITHUB_APP_INSTALLATION_ID are
all set, this module fetches an IAT and caches it with auto-refresh (5 min
before expiry).  The token is suitable for use as the ``GH_TOKEN`` env var
passed to ``gh`` CLI subprocesses, which makes PR/issue comments appear under
the GitHub App bot identity (e.g. ``AgentHub [bot]``).

Usage::

    mgr = IATManager.from_env()
    if mgr is not None:
        token = mgr.get_token()   # str — valid for ~1 h, auto-refreshed

Returns ``None`` from ``from_env()`` when the required env vars are absent so
callers can fall back to the existing PAT-based ``gh auth login`` silently.

Required env vars (all three must be set to enable IAT mode):
    GITHUB_APP_ID                GitHub App numeric ID
    GITHUB_APP_PRIVATE_KEY       PEM-encoded RSA private key, or an absolute
                                 file path beginning with "/"
    GITHUB_APP_INSTALLATION_ID   Installation ID for the target org/account

Optional:
    GITHUB_APP_API_URL           GitHub API base URL (default: https://api.github.com)
                                 Override for GitHub Enterprise Server.
"""

from __future__ import annotations

import json
import logging
import os
import threading
import time
import urllib.request
from datetime import datetime, timezone
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    pass

logger = logging.getLogger(__name__)

_API_URL_DEFAULT = "https://api.github.com"
_IAT_REFRESH_MARGIN_S = 5 * 60  # refresh 5 min before expiry
_JWT_VALIDITY_S = 9 * 60  # 9 min (GitHub max is 10 min)
_CLOCK_SKEW_S = 30


def _require_pyjwt() -> None:
    try:
        import jwt  # noqa: F401 — presence check only
        import cryptography  # noqa: F401
    except ImportError as exc:
        raise ImportError(
            "GitHub App IAT mode requires PyJWT[crypto]. "
            "Install with: pip install 'PyJWT[crypto]>=2.0'"
        ) from exc


def _load_private_key_pem(value: str) -> str:
    """Return PEM content from either a file path or inline PEM string."""
    if value.startswith("/"):
        with open(value) as f:
            return f.read()
    return value


def _generate_jwt(app_id: str, private_key_pem: str) -> str:
    """Generate a GitHub App JWT signed with RS256."""
    _require_pyjwt()
    import jwt as pyjwt

    now = int(time.time())
    payload = {
        "iss": app_id,
        "iat": now - _CLOCK_SKEW_S,
        "exp": now + _JWT_VALIDITY_S,
    }
    return pyjwt.encode(payload, private_key_pem, algorithm="RS256")


def _fetch_iat(app_id: str, private_key_pem: str, installation_id: str, api_url: str) -> tuple[str, float]:
    """Exchange a JWT for an IAT.

    Returns (token, expires_at_unix) where expires_at_unix is a POSIX
    timestamp marking when the token expires.
    """
    jwt_token = _generate_jwt(app_id, private_key_pem)
    url = f"{api_url}/app/installations/{installation_id}/access_tokens"
    req = urllib.request.Request(
        url,
        method="POST",
        headers={
            "Authorization": f"Bearer {jwt_token}",
            "Accept": "application/vnd.github+json",
            "X-GitHub-Api-Version": "2022-11-28",
            "User-Agent": "agent-hub-bridges/github-iat",
        },
    )
    with urllib.request.urlopen(req, timeout=15) as resp:
        body = json.loads(resp.read())

    token = body.get("token")
    if not token:
        raise ValueError(f"empty token in IAT response: {body}")

    expires_at_str: str = body.get("expires_at", "")
    try:
        dt = datetime.fromisoformat(expires_at_str.replace("Z", "+00:00"))
        expires_at = dt.timestamp()
    except (ValueError, AttributeError):
        expires_at = time.time() + 3600  # fallback: 1 h

    return token, expires_at


class IATManager:
    """Fetches and caches GitHub App Installation Access Tokens.

    Thread-safe.  Tokens are refreshed automatically when they will expire
    within ``_IAT_REFRESH_MARGIN_S`` seconds (default 5 minutes).
    """

    def __init__(self, app_id: str, private_key_pem: str, installation_id: str, api_url: str = _API_URL_DEFAULT) -> None:
        self._app_id = app_id
        self._private_key_pem = private_key_pem
        self._installation_id = installation_id
        self._api_url = api_url

        self._lock = threading.Lock()
        self._token: str = ""
        self._expires_at: float = 0.0

    @classmethod
    def from_env(cls) -> "IATManager | None":
        """Create an IATManager from GITHUB_APP_* env vars.

        Returns ``None`` when any of the three required vars is absent —
        callers should fall back to PAT-based auth in that case.
        """
        app_id = os.environ.get("GITHUB_APP_ID", "")
        private_key_raw = os.environ.get("GITHUB_APP_PRIVATE_KEY", "")
        installation_id = os.environ.get("GITHUB_APP_INSTALLATION_ID", "")

        if not (app_id and private_key_raw and installation_id):
            return None

        private_key_pem = _load_private_key_pem(private_key_raw)
        api_url = os.environ.get("GITHUB_APP_API_URL", _API_URL_DEFAULT).rstrip("/")
        return cls(app_id, private_key_pem, installation_id, api_url)

    def get_token(self) -> str:
        """Return a valid IAT, refreshing if necessary.

        Raises on HTTP / JWT errors so callers can decide whether to fall back
        to PAT or hard-fail.
        """
        with self._lock:
            if self._token and time.time() < self._expires_at - _IAT_REFRESH_MARGIN_S:
                return self._token

            logger.debug("github_iat: fetching new IAT (app_id=%s, installation=%s)", self._app_id, self._installation_id)
            token, expires_at = _fetch_iat(
                self._app_id, self._private_key_pem, self._installation_id, self._api_url
            )
            self._token = token
            self._expires_at = expires_at
            exp_dt = datetime.fromtimestamp(expires_at, tz=timezone.utc).isoformat()
            logger.info("github_iat: IAT refreshed, expires=%s", exp_dt)
            return self._token
