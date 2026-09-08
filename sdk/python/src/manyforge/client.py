"""Management and credential-isolated public clients over generated scopes."""
from __future__ import annotations

from collections.abc import Awaitable, Callable
from types import MappingProxyType
from typing import Self
from uuid import UUID

import httpx

from .http import AsyncHTTPTransport, HTTPTransport
from .resources import (AnalyticsScope, AsyncAnalyticsScope, AsyncBusinessScope, AsyncFeedbackScope, AsyncMailingScope, AsyncRootScope, AsyncTelemetryScope, BusinessScope, FeedbackScope, MailingScope, RootScope, TelemetryScope)
from .session import AsyncSession, Session


class _SyncLifecycle:
    _transport: HTTPTransport

    def close(self) -> None:
        self._transport.close()

    def __enter__(self) -> Self:
        return self

    def __exit__(self, *args: object) -> None:
        self.close()


class _AsyncLifecycle:
    _transport: AsyncHTTPTransport

    async def aclose(self) -> None:
        await self._transport.aclose()

    async def __aenter__(self) -> Self:
        return self

    async def __aexit__(self, *args: object) -> None:
        await self.aclose()


class ManyForge(_SyncLifecycle, RootScope):
    def __init__(self, *, base_url: str, access_token: str | None = None, token_provider: Callable[[], str] | None = None, session: Session | None = None, timeout: float = 30, http_client: httpx.Client | None = None) -> None:
        super().__init__(HTTPTransport(base_url, access_token=access_token, token_provider=token_provider, session=session, timeout=timeout, http_client=http_client))

    def business(self, business_id: str | UUID) -> BusinessScope:
        return BusinessScope(self._transport, {"id": str(business_id)})


class AsyncManyForge(_AsyncLifecycle, AsyncRootScope):
    def __init__(self, *, base_url: str, access_token: str | None = None, token_provider: Callable[[], Awaitable[str]] | None = None, session: AsyncSession | None = None, timeout: float = 30, http_client: httpx.AsyncClient | None = None) -> None:
        super().__init__(AsyncHTTPTransport(base_url, access_token=access_token, token_provider=token_provider, session=session, timeout=timeout, http_client=http_client))

    def business(self, business_id: str | UUID) -> AsyncBusinessScope:
        return AsyncBusinessScope(self._transport, {"id": str(business_id)})


class _SyncPublic(_SyncLifecycle):
    def __init__(self, *, base_url: str, publishable_key: str, timeout: float = 30, http_client: httpx.Client | None = None) -> None:
        self._transport = HTTPTransport(base_url, public=True, publishable_key=publishable_key, timeout=timeout, http_client=http_client)
        self._bindings = MappingProxyType({"key": publishable_key})


class _AsyncPublic(_AsyncLifecycle):
    def __init__(self, *, base_url: str, publishable_key: str, timeout: float = 30, http_client: httpx.AsyncClient | None = None) -> None:
        self._transport = AsyncHTTPTransport(base_url, public=True, publishable_key=publishable_key, timeout=timeout, http_client=http_client)
        self._bindings = MappingProxyType({"key": publishable_key})


class FeedbackClient(_SyncPublic, FeedbackScope):
    """Unsigned feedback posts/list/vote; never accepts signing credentials."""


class AsyncFeedbackClient(_AsyncPublic, AsyncFeedbackScope):
    """Native async unsigned feedback client."""


class TelemetryClient(_SyncPublic, TelemetryScope):
    """Explicit telemetry ingestion, without batching, retries or timestamps."""


class AsyncTelemetryClient(_AsyncPublic, AsyncTelemetryScope):
    """Native async unsigned telemetry client."""


class MailingClient(_SyncPublic, MailingScope):
    """Public subscribe only; signed operations live in manyforge.server."""


class AsyncMailingClient(_AsyncPublic, AsyncMailingScope):
    """Native async public subscribe client."""


class AnalyticsClient(_SyncLifecycle, AnalyticsScope):
    def __init__(self, *, base_url: str, publishable_key: str, source_origin: str, timeout: float = 30, http_client: httpx.Client | None = None) -> None:
        super().__init__(HTTPTransport(base_url, public=True, publishable_key=publishable_key, source_origin=source_origin, timeout=timeout, http_client=http_client))


class AsyncAnalyticsClient(_AsyncLifecycle, AsyncAnalyticsScope):
    def __init__(self, *, base_url: str, publishable_key: str, source_origin: str, timeout: float = 30, http_client: httpx.AsyncClient | None = None) -> None:
        super().__init__(AsyncHTTPTransport(base_url, public=True, publishable_key=publishable_key, source_origin=source_origin, timeout=timeout, http_client=http_client))
