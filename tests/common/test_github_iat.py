"""Unit tests for github_iat.IATManager (issue #73)."""

from __future__ import annotations

import time
import unittest.mock as mock
from unittest.mock import MagicMock, patch

import pytest

from agent_hub_bridges._common.github_iat import (
    IATManager,
    _load_private_key_pem,
)


# ─── fixtures ────────────────────────────────────────────────────────────────

_APP_ID = "12345"
_INSTALLATION_ID = "99999"

# Minimal valid RSA-2048 key (test-only, never used in production)
_TEST_PEM = """\
-----BEGIN RSA PRIVATE KEY-----
MIIEowIBAAKCAQEA2a2rwplBQLzHPZe5TNJF7K6J8DFBkDJiVKoEJthJMCDZFagQ
NjTmAVE7Dqc6i5BPWJ7KsDIjHLtVdBRF7XvHSKBINJTnUE8Ql6QHXblAGqjn0OZ
E3IpO0ZNUqtZTa0N1wl3V5K+OHjZBJk+m7WPxR3KSEf1WG5SKOIrn6Y3X9oG3Y8e
3PCMN5dBO0hGxL+oHvGBH8E3n7D2q3bYWnL5OTKQ7qCrjEz3Xd4MTYH0PknX6CA
0wQfWr1E0MeGHRG8e3lZDqXqBh6aZMqE5ZbYi5e7T1EFiSNT1PXLBR8VeE5i2Q4
b5r1eJ4mvFgQ6YcVXTxY2r6qDfYPrMnJVkiPcwIDAQABAoIBAHnqOATDJMpBE2U5
0YqklP1Y0dKG0rF7WjYLgyCmOT5oVKNUMEFLEPq7+UakdL1vJb0xbDLl+Mku6M7
h0O7K8WVi5EaZKGGV4qlpbvkCgj9H6LI6C1Vw5dJt8EiV0lnJaJlnhk2m6EJqLo
wKFG9xQlLMjxWMCF0d/oRX35ooWRVfV5XkLjVlBFkxhVtlVu2E1Sk0xT6MXSVE5
I2Y5VfxqfCW5lJXGAWFXJuG2k/bFnbG3ioEWV7ot4mA3b3JoMb1S5VFrjEj6VxK
i/MOg2K3h0OzMcG+eX0iOzwSQCv3j9o4mJQ1QNiXoLixb7aznYOkSFU/fJbHf1s
RinZ3lECgYEA7RiGF8TJJQ+UG/ZvCt7/B8mOBIR7CknZ5ANz3VHiXUGEtAoHjn4w
5KMT0U6C0/5yTzLyG4G2GWzDUoXiXkqWKoMNLrJMsrS+TvgMjz3rWJbDijMLqJDO
c4DTGH6mj5fWYq0xq5/Mxk3Q7q3v1CvOaR5hmx7v5V9J5dSYsGkCgYEA6pFU6u7r
0i1XC7jUYUHwRMiZZsBL0JO5Kcj1p2Bq2bv6F5/Jm4t4LH/k5cJRHvn5K6sEpq5
8R6KkLVBXzb7jqjb0e3cMYEI4q3hn6BrlEmSE4bXz3NhE6EsW4p3f5r5Lm2FYCQ4
4HLKlZOj0BdEesFV1OV7J8Qgn1EjcwAqxJ0CgYEAq6JbXFxUvU/j/M5gMmN8y2nT
qRYyEqH1P4R4XOAFfAm7UYQRTY4v5jV1VxJfVKXWGLrFT2HYz1wJDFa2M1P7DdC+
pVYpg0VD43HEqEFJ1KkYR8pNvCQ6P1Fz3vRMbNVKXPdxW0VgKi3Xm7Q7p8YgT8e
G3ioL5qiKvkV2IkCgYBQ8pG3uEpb1xaZBCp0j2J2a1eE+Z5Q3T4QlZBVqVv5DmO
7HVl8o5TlCWy0mFLU0m2RJZJ+FNVt8V4k1pDTIMT1ZW2k5VPfDt73MR5a0aqf5u
QnXCqVP8gK0gy3l4J6M9A0vTe8v5dFJYZ9w6l4V8j6v0Y1L/0+s+LQKBgH2Iv5Jv
w4lV4V0Mm6l+M8c4J9z1o6E2n7kQ1KoJhY6S7oGZl3DhC5T8zKVbUJQjnJSU5pBc
nLZN1N4DQ0UJr8b8x3Xq1b8TJDEJqOh8dVjZ3N8F4X5t4z5P6U6q5Fv/5Wvm7k0
5kFpSlmT8K9d9D9y8qA5r0DKjvd5z6E0
-----END RSA PRIVATE KEY-----
"""


def _make_manager(token: str = "ghs_test_token", expires_in: int = 3600) -> IATManager:
    """Return an IATManager with _fetch_iat mocked out."""
    mgr = IATManager(_APP_ID, _TEST_PEM, _INSTALLATION_ID)
    mgr._token = ""
    mgr._expires_at = 0.0

    expires_at = time.time() + expires_in
    with patch(
        "agent_hub_bridges._common.github_iat._fetch_iat",
        return_value=(token, expires_at),
    ):
        tok = mgr.get_token()

    assert tok == token
    return mgr


# ─── from_env ────────────────────────────────────────────────────────────────


def test_from_env_returns_none_when_vars_missing(monkeypatch):
    monkeypatch.delenv("GITHUB_APP_ID", raising=False)
    monkeypatch.delenv("GITHUB_APP_PRIVATE_KEY", raising=False)
    monkeypatch.delenv("GITHUB_APP_INSTALLATION_ID", raising=False)
    assert IATManager.from_env() is None


def test_from_env_returns_none_when_partial(monkeypatch):
    monkeypatch.setenv("GITHUB_APP_ID", "1")
    monkeypatch.delenv("GITHUB_APP_PRIVATE_KEY", raising=False)
    monkeypatch.delenv("GITHUB_APP_INSTALLATION_ID", raising=False)
    assert IATManager.from_env() is None


def test_from_env_creates_manager_when_all_set(monkeypatch, tmp_path):
    key_path = tmp_path / "key.pem"
    key_path.write_text(_TEST_PEM)

    monkeypatch.setenv("GITHUB_APP_ID", _APP_ID)
    monkeypatch.setenv("GITHUB_APP_PRIVATE_KEY", str(key_path))
    monkeypatch.setenv("GITHUB_APP_INSTALLATION_ID", _INSTALLATION_ID)
    monkeypatch.delenv("GITHUB_APP_API_URL", raising=False)

    mgr = IATManager.from_env()
    assert mgr is not None
    assert mgr._app_id == _APP_ID
    assert mgr._installation_id == _INSTALLATION_ID


# ─── get_token caching ───────────────────────────────────────────────────────


def test_get_token_caches_result():
    mgr = IATManager(_APP_ID, _TEST_PEM, _INSTALLATION_ID)
    expires_at = time.time() + 3600
    fetch_mock = MagicMock(return_value=("tok1", expires_at))

    with patch("agent_hub_bridges._common.github_iat._fetch_iat", fetch_mock):
        t1 = mgr.get_token()
        t2 = mgr.get_token()

    assert t1 == t2 == "tok1"
    fetch_mock.assert_called_once()


def test_get_token_refreshes_when_near_expiry():
    mgr = IATManager(_APP_ID, _TEST_PEM, _INSTALLATION_ID)
    # Seed with a token that expires in 2 minutes (< 5 min margin)
    mgr._token = "old_token"
    mgr._expires_at = time.time() + 120

    fetch_mock = MagicMock(return_value=("new_token", time.time() + 3600))
    with patch("agent_hub_bridges._common.github_iat._fetch_iat", fetch_mock):
        tok = mgr.get_token()

    assert tok == "new_token"
    fetch_mock.assert_called_once()


def test_get_token_does_not_refresh_when_fresh():
    mgr = IATManager(_APP_ID, _TEST_PEM, _INSTALLATION_ID)
    mgr._token = "fresh_token"
    mgr._expires_at = time.time() + 3600  # expires in 1 h — well outside margin

    fetch_mock = MagicMock()
    with patch("agent_hub_bridges._common.github_iat._fetch_iat", fetch_mock):
        tok = mgr.get_token()

    assert tok == "fresh_token"
    fetch_mock.assert_not_called()


# ─── _load_private_key_pem ───────────────────────────────────────────────────


def test_load_private_key_pem_inline():
    result = _load_private_key_pem(_TEST_PEM)
    assert result == _TEST_PEM


def test_load_private_key_pem_from_file(tmp_path):
    p = tmp_path / "key.pem"
    p.write_text(_TEST_PEM)
    result = _load_private_key_pem(str(p))
    assert result == _TEST_PEM
