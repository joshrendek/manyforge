"""Optional in-memory session owners; never storage or distributed coordination."""
from __future__ import annotations

import asyncio
import math
import threading
import time
from collections.abc import Awaitable, Callable
from typing import Self

from .errors import SessionError
from .models.token_pair import TokenPair


def _pair_state(pair: TokenPair) -> tuple[str, str, float, float]:
    if not pair.access_token or not pair.refresh_token or not math.isfinite(pair.expires_in) or pair.expires_in <= 0:
        raise SessionError("Session token pair is invalid; authenticate again")
    lifetime = float(pair.expires_in)
    return pair.access_token, pair.refresh_token, getattr(pair, "_sdk_received_at", time.monotonic()) + lifetime, min(30.0, lifetime / 10)


class Session:
    def __init__(self, pair: TokenPair, *, on_rotate: Callable[[TokenPair], None] | None = None) -> None:
        self._access, self._refresh, self._expires, self._margin = _pair_state(pair)
        self._on_rotate = on_rotate
        self._lock = threading.Lock()
        self._valid = True
        self._origin: str | None = None

    @classmethod
    def from_token_pair(cls, pair: TokenPair, *, on_rotate: Callable[[TokenPair], None] | None = None) -> Self:
        return cls(pair, on_rotate=on_rotate)

    def _token(self, origin: str, refresh: Callable[[str], TokenPair]) -> str:
        with self._lock:
            if not self._valid:
                raise SessionError("Session is invalid; authenticate again")
            if self._origin is not None and self._origin != origin:
                raise SessionError("Session is already bound to another instance")
            self._origin = origin
            if self._expires - time.monotonic() < self._margin:
                try:
                    pair = refresh(self._refresh)
                    state = _pair_state(pair)
                    if self._on_rotate is not None:
                        self._on_rotate(pair.model_copy(deep=True))
                    self._access, self._refresh, self._expires, self._margin = state
                except BaseException as error:
                    self._valid = False
                    self._access = self._refresh = ""
                    if not isinstance(error, Exception):
                        raise
                    raise SessionError("Session rotation failed; authenticate again") from None
            return self._access


class AsyncSession:
    def __init__(self, pair: TokenPair, *, on_rotate: Callable[[TokenPair], Awaitable[None]] | None = None) -> None:
        self._access, self._refresh, self._expires, self._margin = _pair_state(pair)
        self._on_rotate = on_rotate
        self._lock = asyncio.Lock()
        self._valid = True
        self._origin: str | None = None

    @classmethod
    def from_token_pair(cls, pair: TokenPair, *, on_rotate: Callable[[TokenPair], Awaitable[None]] | None = None) -> Self:
        return cls(pair, on_rotate=on_rotate)

    async def _token(self, origin: str, refresh: Callable[[str], Awaitable[TokenPair]]) -> str:
        async with self._lock:
            if not self._valid:
                raise SessionError("Session is invalid; authenticate again")
            if self._origin is not None and self._origin != origin:
                raise SessionError("Session is already bound to another instance")
            self._origin = origin
            if self._expires - time.monotonic() < self._margin:
                try:
                    pair = await refresh(self._refresh)
                    state = _pair_state(pair)
                    if self._on_rotate is not None:
                        await self._on_rotate(pair.model_copy(deep=True))
                    self._access, self._refresh, self._expires, self._margin = state
                except BaseException as error:
                    self._valid = False
                    self._access = self._refresh = ""
                    if not isinstance(error, Exception):
                        raise
                    raise SessionError("Session rotation failed; authenticate again") from None
            return self._access
