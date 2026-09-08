"""HTTPX transports. Generated requests are the only operation inventory."""
from __future__ import annotations

import asyncio
import hashlib
import hmac
import io
import json
import math
import time
from collections.abc import AsyncIterator, Awaitable, Callable
from datetime import date, datetime
from threading import Event
from typing import Any, BinaryIO, TypeVar
from urllib.parse import quote, urlencode, urlsplit
from uuid import UUID

import httpx
from pydantic import BaseModel, ValidationError

from .errors import AuthenticationError, ManyForgeError, ProtocolError, TimeoutError, TransportError
from .model_support import UnsetType
from .models.token_pair import TokenPair
from .session import AsyncSession, Session
from .transport import Request, RequestOptions, Upload

T = TypeVar("T")


def instance_origin(value: str) -> str:
    if not isinstance(value, str) or not value or any(ord(c) <= 32 or ord(c) == 127 for c in value) or "\\" in value:
        raise ValueError("base_url must be an absolute HTTP(S) instance root")
    try:
        parsed = urlsplit(value)
        port = parsed.port
        url = httpx.URL(value)
    except (ValueError, httpx.InvalidURL):
        raise ValueError("base_url must be an absolute HTTP(S) instance root") from None
    if parsed.scheme not in ("http", "https") or not parsed.hostname or not url.host or parsed.username is not None or parsed.password is not None or parsed.path not in ("", "/") or "?" in value or "#" in value or (port is not None and not 0 < port < 65536):
        raise ValueError("base_url must be an absolute HTTP(S) instance root without credentials, path, query or fragment")
    return str(url).rstrip("/")


def _timeout(value: float) -> float:
    if not math.isfinite(value) or value <= 0:
        raise ValueError("timeout must be a finite positive number of seconds")
    return value


def _scalar(value: Any) -> str:
    if isinstance(value, bool):
        return "true" if value else "false"
    if isinstance(value, (date, datetime)):
        return value.isoformat()
    return str(value)


def _json_value(value: Any) -> Any:
    if isinstance(value, BaseModel):
        return value.model_dump(mode="json", by_alias=True, exclude_unset=True)
    if isinstance(value, (datetime, date)):
        return value.isoformat()
    if isinstance(value, UUID):
        return str(value)
    if isinstance(value, dict):
        return {key: _json_value(item) for key, item in value.items() if not isinstance(item, UnsetType)}
    if isinstance(value, (list, tuple)):
        return [_json_value(item) for item in value]
    return value


class _UploadReader:
    """Borrow a reader; stop between blocking calls without closing its owner."""

    def __init__(self, reader: BinaryIO, cancelled: Event) -> None:
        if isinstance(reader, io.TextIOBase):
            raise TypeError("Multipart file uploads must be opened in binary mode")
        self._reader = reader
        self._cancelled = cancelled

    def _check(self) -> None:
        if self._cancelled.is_set():
            raise asyncio.CancelledError()

    def read(self, size: int) -> bytes:
        self._check()
        return self._reader.read(size)

    def tell(self) -> int:
        self._check()
        return self._reader.tell()

    def seek(self, offset: int, whence: int = 0) -> int:
        self._check()
        try:
            return self._reader.seek(offset, whence)
        except AttributeError:
            raise io.UnsupportedOperation("Reader is not seekable") from None

    # Let HTTPX probe with tell/seek instead of fstat: a pipe's st_size is zero,
    # not its upload length. Nonseekable readers correctly use chunked framing.


class _AsyncMultipartStream(httpx.AsyncByteStream):
    def __init__(self, stream: httpx.SyncByteStream, cancelled: Event) -> None:
        self._stream = stream
        self._cancelled = cancelled

    async def __aiter__(self) -> AsyncIterator[bytes]:
        chunks = iter(self._stream)
        try:
            while not self._cancelled.is_set():
                # HTTPX's public sync stream retains its exact multipart encoding.
                # Advance only on demand, at most one bounded file read at a time.
                chunk = await asyncio.to_thread(next, chunks, None)
                if chunk is None:
                    break
                yield chunk
        finally:
            self._cancelled.set()

    async def aclose(self) -> None:
        # An in-flight OS read cannot be interrupted safely on a borrowed file.
        # Discard its result; do not wait, close the file, or schedule another read.
        self._cancelled.set()


class _Wire:
    def __init__(self, base_url: str, timeout: float, *, public: bool = False, publishable_key: str | None = None, source_origin: str | None = None, signing: str | None = None, signing_secret: str | None = None) -> None:
        self.origin = instance_origin(base_url)
        self.timeout = _timeout(timeout)
        self.public = public
        self.publishable_key = publishable_key
        self.source_origin = instance_origin(source_origin) if source_origin is not None else None
        self.signing = signing
        self.signing_secret = signing_secret
        if public and (not isinstance(publishable_key, str) or not publishable_key.strip()):
            raise ValueError("publishable_key must be nonempty")
        if signing and (not isinstance(signing_secret, str) or not signing_secret.strip()):
            raise ValueError("signing_secret must be nonempty")

    def prepare(self, descriptor: Request[Any], options: RequestOptions | None, token: str | None, *, upload_cancelled: Event | None = None) -> httpx.Request:
        path = descriptor.path
        if not path.startswith("/") or path.startswith("//") or "?" in path or "#" in path:
            raise ValueError("Request path must be a generated absolute-path reference")
        for name, value in descriptor.path_parameters.items():
            encoded = quote(_scalar(value), safe="")
            if encoded in (".", ".."):
                encoded = encoded.replace(".", "%2E")
            path = path.replace("{" + name + "}", encoded)
        if "{" in path or "}" in path:
            raise ValueError("Unbound path parameter")
        query: list[tuple[str, str]] = []
        for parameter in descriptor.query:
            value = parameter.value
            if isinstance(value, UnsetType) or value is None:
                continue
            if isinstance(value, list):
                if parameter.style == "form" and parameter.explode:
                    query.extend((parameter.name, _scalar(item)) for item in value)
                else:
                    separator = {"form": ",", "spaceDelimited": " ", "pipeDelimited": "|"}.get(parameter.style)
                    if separator is None:
                        raise ValueError("Unsupported query parameter style")
                    query.append((parameter.name, separator.join(_scalar(item) for item in value)))
            else:
                query.append((parameter.name, _scalar(value)))
        target = path + ("?" + urlencode(query, quote_via=quote, safe="") if query else "")
        headers = {name: _scalar(value) for name, value in descriptor.headers.items() if value is not None and not isinstance(value, UnsetType)}
        # Never merge defaults from the caller's HTTP client, including cookies.
        for name in list(headers):
            if name.lower() in ("authorization", "cookie", "proxy-authorization"):
                del headers[name]
        if token is not None:
            headers["Authorization"] = "Bearer " + token
        if self.source_origin is not None:
            headers["Origin"] = self.source_origin
        if descriptor.accept:
            headers["Accept"] = descriptor.accept
        payload: dict[str, Any] = {}
        if descriptor.form:
            data: dict[str, str] = {}
            files: dict[str, tuple[str, Any, str]] = {}
            for name, value in descriptor.form.items():
                if isinstance(value, Upload):
                    content: Any = value.content
                    if upload_cancelled is not None and not isinstance(content, bytes):
                        content = _UploadReader(content, upload_cancelled)
                    files[name] = (value.filename, content, value.content_type)
                elif not isinstance(value, UnsetType) and value is not None:
                    data[name] = _scalar(value)
            payload = {"data": data, "files": files}
        elif not isinstance(descriptor.body, UnsetType):
            value = _json_value(descriptor.body)
            if descriptor.body_key:
                if not isinstance(value, dict) or not self.publishable_key:
                    raise ValueError("Publishable-key body must be an object")
                value = {**value, descriptor.body_key: self.publishable_key}
            payload["content"] = json.dumps(value, ensure_ascii=False, allow_nan=False, separators=(",", ":")).encode("utf-8")
            headers["Content-Type"] = descriptor.content_type or "application/json"
        timeout = _timeout(options.timeout) if options is not None and options.timeout is not None else self.timeout
        request = httpx.Request(descriptor.method, self.origin + target, headers=headers, extensions={"timeout": httpx.Timeout(timeout).as_dict()}, **payload)
        if self.signing:
            timestamp = str(int(time.time()))
            final_target = request.url.raw_path
            if self.signing == "mailing":
                final_target = final_target.split(b"?", 1)[0]
            message = timestamp.encode() + b"." + request.method.encode() + b"." + final_target + b"." + request.read()
            signature = hmac.new(self.signing_secret.encode(), message, hashlib.sha256).hexdigest()
            if self.signing == "mailing":
                request.headers["X-Mailing-Timestamp"] = timestamp
                request.headers["X-Mailing-Signature"] = signature
            else:
                request.headers[f"X-{self.signing.title()}-Signature"] = f"t={timestamp},v1={signature}"
        return request


def _decode(descriptor: Request[T], body: bytes) -> T:
    try:
        result = descriptor.response.validate_json(body) if body else descriptor.response.validate_python(None)
    except (ValidationError, ValueError, TypeError):
        raise ProtocolError("ManyForge returned an invalid success payload") from None
    if isinstance(result, TokenPair):
        object.__setattr__(result, "_sdk_received_at", time.monotonic())
    return result


def _valid_token(token: str | None) -> str:
    if not isinstance(token, str) or not token.strip() or any(ord(char) <= 32 or ord(char) == 127 for char in token):
        raise AuthenticationError("A nonempty access token is required")
    return token


def _credentials(access_token: str | None, token_provider: Any, session: Any) -> None:
    if sum(value is not None for value in (access_token, token_provider, session)) > 1:
        raise ValueError("access_token, token_provider and session are mutually exclusive")


class HTTPTransport:
    def __init__(self, base_url: str, *, timeout: float = 30, http_client: httpx.Client | None = None, access_token: str | None = None, token_provider: Callable[[], str] | None = None, session: Session | None = None, **wire: Any) -> None:
        _credentials(access_token, token_provider, session)
        self._wire = _Wire(base_url, timeout, **wire)
        if self._wire.public and any(item is not None for item in (access_token, token_provider, session)):
            raise ValueError("Public clients cannot own management credentials")
        if http_client is not None and self._wire.public and any(http_client.event_hooks.values()):
            raise ValueError("Public clients require an injected HTTPX client without event hooks")
        self._owned = http_client is None
        self._client = http_client if http_client is not None else httpx.Client(trust_env=False, follow_redirects=False)
        self._access_token, self._provider, self._session = access_token, token_provider, session

    def _token(self, descriptor: Request[Any], options: RequestOptions | None) -> str | None:
        if not descriptor.authenticated:
            return None
        if self._wire.public:
            raise AuthenticationError("Management operations are unavailable to public clients")
        if self._session is not None:
            from .api.root_auth_resource import RootAuthResource
            return _valid_token(self._session._token(self._wire.origin, lambda token: RootAuthResource(self).refresh(token, request_options=options)))
        return _valid_token(self._provider() if self._provider is not None else self._access_token)

    def _send(self, descriptor: Request[Any], options: RequestOptions | None) -> httpx.Response:
        request = self._wire.prepare(descriptor, options, self._token(descriptor, options))
        try:
            response = self._client.send(request, stream=True, follow_redirects=False, auth=None)
            if not 200 <= response.status_code < 300:
                raw = bytearray()
                try:
                    for chunk in response.iter_bytes():
                        raw.extend(chunk[:65537 - len(raw)])
                        if len(raw) > 65536:
                            break
                finally:
                    response.close()
                raise ManyForgeError(response.status_code, response.headers, bytes(raw[:65536]), len(raw) > 65536)
            return response
        except httpx.TimeoutException:
            raise TimeoutError("ManyForge request timed out") from None
        except httpx.HTTPError:
            raise TransportError("ManyForge transport exchange failed") from None

    def request(self, request: Request[T], *, request_options: RequestOptions | None = None) -> T:
        response = self._send(request, request_options)
        try:
            return _decode(request, response.read())
        except httpx.TimeoutException:
            raise TimeoutError("ManyForge request timed out") from None
        except httpx.HTTPError:
            raise TransportError("ManyForge transport exchange failed") from None
        finally:
            response.close()

    def stream(self, request: Request[None], *, request_options: RequestOptions | None = None) -> ResponseStream:
        return ResponseStream(self._send(request, request_options))

    def close(self) -> None:
        if self._owned:
            self._client.close()


class AsyncHTTPTransport:
    def __init__(self, base_url: str, *, timeout: float = 30, http_client: httpx.AsyncClient | None = None, access_token: str | None = None, token_provider: Callable[[], Awaitable[str]] | None = None, session: AsyncSession | None = None, **wire: Any) -> None:
        _credentials(access_token, token_provider, session)
        self._wire = _Wire(base_url, timeout, **wire)
        if self._wire.public and any(item is not None for item in (access_token, token_provider, session)):
            raise ValueError("Public clients cannot own management credentials")
        if http_client is not None and self._wire.public and any(http_client.event_hooks.values()):
            raise ValueError("Public clients require an injected HTTPX client without event hooks")
        self._owned = http_client is None
        self._client = http_client if http_client is not None else httpx.AsyncClient(trust_env=False, follow_redirects=False)
        self._access_token, self._provider, self._session = access_token, token_provider, session

    async def _token(self, descriptor: Request[Any], options: RequestOptions | None) -> str | None:
        if not descriptor.authenticated:
            return None
        if self._wire.public:
            raise AuthenticationError("Management operations are unavailable to public clients")
        if self._session is not None:
            from .api.root_auth_resource import AsyncRootAuthResource
            return _valid_token(await self._session._token(self._wire.origin, lambda token: AsyncRootAuthResource(self).refresh(token, request_options=options)))
        return _valid_token(await self._provider() if self._provider is not None else self._access_token)

    async def _send(self, descriptor: Request[Any], options: RequestOptions | None) -> httpx.Response:
        token = await self._token(descriptor, options)
        upload_cancelled = Event() if descriptor.form else None
        try:
            if upload_cancelled is None:
                request = self._wire.prepare(descriptor, options, token)
            else:
                # Request construction probes file lengths; keep that I/O off-loop
                # too. The HTTP exchange itself remains native asynchronous HTTPX.
                request = await asyncio.to_thread(self._wire.prepare, descriptor, options, token, upload_cancelled=upload_cancelled)
                assert isinstance(request.stream, httpx.SyncByteStream)
                request.stream = _AsyncMultipartStream(request.stream, upload_cancelled)
            response = await self._client.send(request, stream=True, follow_redirects=False, auth=None)
            if not 200 <= response.status_code < 300:
                raw = bytearray()
                try:
                    async for chunk in response.aiter_bytes():
                        raw.extend(chunk[:65537 - len(raw)])
                        if len(raw) > 65536:
                            break
                finally:
                    await response.aclose()
                raise ManyForgeError(response.status_code, response.headers, bytes(raw[:65536]), len(raw) > 65536)
            return response
        except httpx.TimeoutException:
            raise TimeoutError("ManyForge request timed out") from None
        except httpx.HTTPError:
            raise TransportError("ManyForge transport exchange failed") from None
        finally:
            if upload_cancelled is not None:
                upload_cancelled.set()

    async def request(self, request: Request[T], *, request_options: RequestOptions | None = None) -> T:
        response = await self._send(request, request_options)
        try:
            return _decode(request, await response.aread())
        except httpx.TimeoutException:
            raise TimeoutError("ManyForge request timed out") from None
        except httpx.HTTPError:
            raise TransportError("ManyForge transport exchange failed") from None
        finally:
            await response.aclose()

    async def stream(self, request: Request[None], *, request_options: RequestOptions | None = None) -> AsyncResponseStream:
        return AsyncResponseStream(await self._send(request, request_options))

    async def aclose(self) -> None:
        if self._owned:
            await self._client.aclose()


class ResponseStream:
    def __init__(self, response: httpx.Response) -> None:
        self._response = response
        self.headers = response.headers
        self._chunks = response.iter_bytes()
        self._pending = bytearray()

    def read(self, size: int = -1) -> bytes:
        if size < -1:
            raise ValueError("size must be -1 or nonnegative")
        try:
            while size < 0 or len(self._pending) < size:
                self._pending.extend(next(self._chunks))
        except StopIteration:
            pass
        except httpx.TimeoutException:
            self.close()
            raise TimeoutError("ManyForge stream timed out") from None
        except httpx.HTTPError:
            self.close()
            raise TransportError("ManyForge stream failed") from None
        count = len(self._pending) if size < 0 else min(size, len(self._pending))
        result = bytes(self._pending[:count])
        del self._pending[:count]
        return result

    def close(self) -> None:
        self._response.close()

    def __enter__(self) -> ResponseStream:
        return self

    def __exit__(self, *args: object) -> None:
        self.close()


class AsyncResponseStream:
    def __init__(self, response: httpx.Response) -> None:
        self._response = response
        self.headers = response.headers

    async def __aiter__(self):
        try:
            async for chunk in self._response.aiter_bytes():
                yield chunk
        except httpx.TimeoutException:
            raise TimeoutError("ManyForge stream timed out") from None
        except httpx.HTTPError:
            raise TransportError("ManyForge stream failed") from None
        finally:
            await self.aclose()

    async def aclose(self) -> None:
        await self._response.aclose()

    async def __aenter__(self) -> AsyncResponseStream:
        return self

    async def __aexit__(self, *args: object) -> None:
        await self.aclose()
