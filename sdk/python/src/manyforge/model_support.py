"""Handwritten Pydantic wire semantics shared by generated models."""
from __future__ import annotations

from enum import Enum
from typing import Any, Self

from pydantic import BaseModel, ConfigDict, JsonValue, SerializationInfo, SerializerFunctionWrapHandler, model_serializer, model_validator
from pydantic_core import core_schema


class UnsetType:
    """A field or argument that was not supplied (distinct from JSON null)."""
    __slots__ = ()

    def __repr__(self) -> str:
        return "UNSET"

    def __copy__(self) -> Self:
        return self

    def __deepcopy__(self, memo: dict[int, Any]) -> Self:
        return self

    @classmethod
    def __get_pydantic_core_schema__(cls, source: Any, handler: Any) -> core_schema.CoreSchema:
        return core_schema.is_instance_schema(
            cls,
            serialization=core_schema.plain_serializer_function_ser_schema(
                lambda value: None, return_schema=core_schema.none_schema()
            ),
        )


UNSET = UnsetType()


class Model(BaseModel):
    model_config = ConfigDict(extra="allow", populate_by_name=True, validate_assignment=True, protected_namespaces=())

    @model_validator(mode="before")
    @classmethod
    def omit_unset(cls, value: Any) -> Any:
        if isinstance(value, dict):
            return {key: item for key, item in value.items() if not isinstance(item, UnsetType)}
        return value

    @model_serializer(mode="wrap")
    def serialize_supplied(self, handler: SerializerFunctionWrapHandler, info: SerializationInfo) -> dict[str, Any]:
        result = handler(self)
        for name, definition in type(self).model_fields.items():
            if isinstance(getattr(self, name), UnsetType):
                result.pop((definition.serialization_alias or definition.alias or name) if info.by_alias else name, None)
        return result

    def to_dict(self) -> dict[str, JsonValue]:
        return self.model_dump(mode="json", by_alias=True, exclude_unset=True)

    def to_json(self) -> str:
        import json
        return json.dumps(self.to_dict(), separators=(",", ":"), ensure_ascii=False)

    @classmethod
    def from_dict(cls, value: dict[str, JsonValue]) -> Self:
        return cls.model_validate(value)

    @classmethod
    def from_json(cls, value: str) -> Self:
        return cls.model_validate_json(value)


class OpenEnum(str, Enum):
    """Known constants plus lossless arbitrary future server strings."""
    @classmethod
    def _missing_(cls, value: object) -> Self | None:
        if not isinstance(value, str):
            return None
        member = str.__new__(cls, value)
        member._name_ = None
        member._value_ = value
        return member
