"""Inspect the installed wheel's typed API and resolved Pydantic fields."""
from __future__ import annotations

import enum
import importlib
import inspect
import json
import pkgutil
import types
import typing

import manyforge
from pydantic import BaseModel


def type_name(value):
    if value is inspect.Signature.empty:
        return "unannotated"
    if value is None or value is type(None):
        return "None"
    if isinstance(value, str):
        return value
    if isinstance(value, typing.ForwardRef):
        return value.__forward_arg__
    origin = typing.get_origin(value)
    args = typing.get_args(value)
    if origin is typing.Annotated:
        return type_name(args[0])
    if origin in (typing.Union, types.UnionType):
        return " | ".join(sorted(type_name(arg) for arg in args))
    if origin is typing.Literal:
        return "Literal[" + ",".join(sorted(repr(arg) for arg in args)) + "]"
    if origin is not None and origin is not value:
        return type_name(origin) + "[" + ",".join(type_name(arg) for arg in args) + "]"
    if isinstance(value, (list, tuple)):
        return "[" + ",".join(type_name(arg) for arg in value) + "]"
    if hasattr(value, "__qualname__"):
        module = getattr(value, "__module__", "")
        return (module + "." if module not in ("builtins", "typing") else "") + value.__qualname__
    if isinstance(value, typing.TypeVar):
        return value.__name__
    return str(value)


def signature(value, *, receiver=False):
    sig = inspect.signature(value)
    hints = typing.get_type_hints(value, include_extras=True)
    params = []
    for index, (name, param) in enumerate(sig.parameters.items()):
        if receiver and index == 0 and name in ("self", "cls"):
            continue
        item = {"name": name if param.kind != param.POSITIONAL_ONLY else str(index),
                "kind": param.kind.name, "type": type_name(hints.get(name, param.annotation)),
                "required": param.default is param.empty and param.kind not in (param.VAR_POSITIONAL, param.VAR_KEYWORD)}
        params.append(item)
    return {"params": params, "returns": type_name(hints.get("return", sig.return_annotation)),
            "async": inspect.iscoroutinefunction(value), "async_generator": inspect.isasyncgenfunction(value)}


def describe_class(cls):
    result = {"kind": "class", "bases": [type_name(base) for base in cls.__bases__], "members": {}}
    members = result["members"]
    is_model = issubclass(cls, BaseModel)
    if is_model:
        for name, field in cls.model_fields.items():
            members[name] = {"kind": "field", "type": type_name(field.annotation),
                             "required": field.is_required(), "alias": field.alias or name}
    hints = typing.get_type_hints(cls, include_extras=True)
    for name, annotation in hints.items():
        if not name.startswith("_") and name not in members:
            # Pydantic's own machinery is not an SDK-owned member.
            if any(name in vars(base).get("__annotations__", {}) and base.__module__.startswith("manyforge") for base in cls.__mro__):
                members[name] = {"kind": "field", "type": type_name(annotation), "required": False}
    for base in reversed(cls.__mro__):
        if not base.__module__.startswith("manyforge"):
            continue
        for name, raw in vars(base).items():
            if name.startswith("_") and name not in ("__init__", "__enter__", "__exit__", "__aenter__", "__aexit__", "__iter__", "__aiter__", "__next__", "__anext__"):
                continue
            if is_model and name == "__init__":
                continue
            if isinstance(raw, property):
                members[name] = {"kind": "property", "type": type_name(typing.get_type_hints(raw.fget).get("return", inspect.Signature.empty)), "readonly": raw.fset is None, "required": False}
            elif isinstance(raw, (staticmethod, classmethod)) or inspect.isfunction(raw):
                fn = raw.__func__ if isinstance(raw, (staticmethod, classmethod)) else raw
                members[name] = {"kind": "method", "binding": "static" if isinstance(raw, staticmethod) else "class" if isinstance(raw, classmethod) else "instance", "signatures": [signature(fn, receiver=not isinstance(raw, staticmethod))]}
            elif isinstance(raw, cls) and isinstance(raw, (str, enum.Enum)):
                members[name] = {"kind": "constant", "type": type_name(cls), "value": raw.value if isinstance(raw, enum.Enum) else str(raw)}
    return result


def main():
    symbols = {}
    names = [manyforge.__name__] + [module.name for module in pkgutil.walk_packages(manyforge.__path__, manyforge.__name__ + ".") if not any(part.startswith("_") for part in module.name.split("."))]
    classes = {}
    for name in sorted(names):
        module = importlib.import_module(name)
        exports = getattr(module, "__all__", [key for key in vars(module) if not key.startswith("_")])
        for key in sorted(exports):
            value = getattr(module, key)
            identity = name + "." + key
            if inspect.isclass(value) and value.__module__.startswith("manyforge"):
                classes[identity] = value
            elif inspect.isfunction(value) and value.__module__.startswith("manyforge"):
                symbols[identity] = {"kind": "function", "signatures": [signature(value)]}
            elif key == "__version__":
                symbols[identity] = {"kind": "constant", "type": "str"}
            elif key in vars(module).get("__annotations__", {}):
                symbols[identity] = {"kind": "value", "type": type_name(typing.get_type_hints(module)[key])}
            elif key in ("JsonValue", "UNSET"):
                symbols[identity] = {"kind": "value", "type": type_name(value if key == "JsonValue" else type(value))}
    for name, cls in sorted(classes.items()):
        symbols[name] = describe_class(cls)
    if not symbols:
        raise RuntimeError("installed Python SDK has no public symbols")
    print(json.dumps({"symbols": symbols}, sort_keys=True))


if __name__ == "__main__":
    main()
