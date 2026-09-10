"""Server-only signing clients. Keep issued secrets outside untrusted clients."""
from __future__ import annotations

from types import MappingProxyType

import httpx

from .client import AnalyticsClient, AsyncAnalyticsClient, _AsyncLifecycle, _SyncLifecycle
from .http import AsyncHTTPTransport, HTTPTransport
from .resources import AsyncFeedbackScope, AsyncMailingServerScope, AsyncTelemetryScope, FeedbackScope, MailingServerScope, TelemetryScope


class _Signed(_SyncLifecycle):
    _signing: str

    def __init__(self, *, base_url: str, publishable_key: str, signing_secret: str, timeout: float = 30, http_client: httpx.Client | None = None) -> None:
        self._transport = HTTPTransport(base_url, public=True, publishable_key=publishable_key, signing=self._signing, signing_secret=signing_secret, timeout=timeout, http_client=http_client)
        self._bindings = MappingProxyType({"key": publishable_key})


class _AsyncSigned(_AsyncLifecycle):
    _signing: str

    def __init__(self, *, base_url: str, publishable_key: str, signing_secret: str, timeout: float = 30, http_client: httpx.AsyncClient | None = None) -> None:
        self._transport = AsyncHTTPTransport(base_url, public=True, publishable_key=publishable_key, signing=self._signing, signing_secret=signing_secret, timeout=timeout, http_client=http_client)
        self._bindings = MappingProxyType({"key": publishable_key})


class SignedFeedbackClient(_Signed, FeedbackScope):
    _signing = "feedback"


class AsyncSignedFeedbackClient(_AsyncSigned, AsyncFeedbackScope):
    _signing = "feedback"


class SignedTelemetryClient(_Signed, TelemetryScope):
    _signing = "telemetry"


class AsyncSignedTelemetryClient(_AsyncSigned, AsyncTelemetryScope):
    _signing = "telemetry"


class MailingServerClient(_Signed, MailingServerScope):
    _signing = "mailing"


class AsyncMailingServerClient(_AsyncSigned, AsyncMailingServerScope):
    _signing = "mailing"


__all__ = ["SignedFeedbackClient", "AsyncSignedFeedbackClient", "SignedTelemetryClient", "AsyncSignedTelemetryClient", "MailingServerClient", "AsyncMailingServerClient", "AnalyticsClient", "AsyncAnalyticsClient"]
