"""Errors keep wire diagnostics explicit and default formatting credential-free."""
from __future__ import annotations

import json
from collections.abc import Mapping
from types import MappingProxyType
from typing import Any


class ManyForgeError(Exception):
    """An HTTP failure, including a redirect that was not followed."""

    def __init__(self, status: int, headers: Mapping[str, str], raw_body: bytes, truncated: bool = False) -> None:
        self.status = status
        self.headers = MappingProxyType(dict(headers))
        self.request_id = headers.get("x-request-id")
        self.raw_body = raw_body[:65536]
        self.truncated = truncated or len(raw_body) > 65536
        self.details: Any = None
        self.code: str | None = None
        self.message: str | None = None
        if not self.truncated:
            try:
                self.details = json.loads(self.raw_body)
            except (ValueError, UnicodeError):
                pass
        if isinstance(self.details, dict):
            code = self.details.get("code")
            message = self.details.get("message", self.details.get("error"))
            self.code = code if isinstance(code, str) else None
            self.message = message if isinstance(message, str) else None
        # Even server messages/codes/request IDs can echo attacker-controlled secrets.
        super().__init__(f"ManyForge HTTP response {status}")


class ProtocolError(Exception):
    """A successful HTTP response did not conform to the declared payload."""


class TransportError(Exception):
    """The exchange failed without an HTTP response; it is not retried."""


class TimeoutError(TransportError):
    """The native HTTPX request deadline expired."""


class AuthenticationError(Exception):
    """No usable management token was supplied."""


class SessionError(AuthenticationError):
    """This session owner requires a new independent login."""
