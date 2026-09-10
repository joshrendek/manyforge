"""Native transports implement these protocols; resources own no HTTP pools."""
from __future__ import annotations

from collections.abc import AsyncIterator, Mapping
from dataclasses import dataclass, field
from datetime import date, datetime
from typing import BinaryIO, Generic, Protocol, TypeVar
from uuid import UUID

from pydantic import BaseModel, TypeAdapter
from .model_support import JsonValue, UNSET, UnsetType

T = TypeVar("T")
Scalar = str | int | float | bool | date | datetime | UUID
ParameterValue = Scalar | list[Scalar] | None | UnsetType


@dataclass(frozen=True)
class RequestOptions:
    timeout: float | None = None


@dataclass(frozen=True)
class Upload:
    filename: str
    content: bytes | BinaryIO
    content_type: str = "text/csv"


@dataclass(frozen=True)
class QueryParameter:
    name: str
    value: ParameterValue
    style: str = "form"
    explode: bool = True


@dataclass(frozen=True)
class Request(Generic[T]):
    operation_id: str
    method: str
    path: str
    response: TypeAdapter[T]
    authenticated: bool
    path_parameters: Mapping[str, Scalar] = field(default_factory=dict)
    query: tuple[QueryParameter, ...] = ()
    headers: Mapping[str, ParameterValue] = field(default_factory=dict)
    body: BaseModel | JsonValue | UnsetType = UNSET
    body_key: str | None = None
    form: Mapping[str, ParameterValue | Upload] = field(default_factory=dict)
    content_type: str | None = None
    accept: str | None = None


class ByteStream(Protocol):
    def read(self, size: int = -1) -> bytes: ...
    def close(self) -> None: ...
    def __enter__(self) -> ByteStream: ...
    def __exit__(self, exc_type: object, exc: object, traceback: object) -> None: ...


class AsyncByteStream(Protocol):
    def __aiter__(self) -> AsyncIterator[bytes]: ...
    async def aclose(self) -> None: ...
    async def __aenter__(self) -> AsyncByteStream: ...
    async def __aexit__(self, exc_type: object, exc: object, traceback: object) -> None: ...


class SyncTransport(Protocol):
    def request(self, request: Request[T], *, request_options: RequestOptions | None = None) -> T: ...
    def stream(self, request: Request[None], *, request_options: RequestOptions | None = None) -> ByteStream: ...


class AsyncTransport(Protocol):
    async def request(self, request: Request[T], *, request_options: RequestOptions | None = None) -> T: ...
    async def stream(self, request: Request[None], *, request_options: RequestOptions | None = None) -> AsyncByteStream: ...


class PaginationError(RuntimeError):
    """A server cursor repeated, so further iteration would loop indefinitely."""
