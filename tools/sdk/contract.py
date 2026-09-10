"""Validate the editable API contract and derive the SDK-only projection.

Copyright (c) 2026 ManyForge contributors. SPDX-License-Identifier: MIT
"""
from __future__ import annotations

import copy
import json
import re
from pathlib import Path
from typing import Any, Iterator

import yaml
from openapi_spec_validator import validate

HTTP_METHODS = frozenset({"get", "put", "post", "delete", "options", "head", "patch", "trace"})
AUDIENCES = frozenset({"management", "public", "server", "callback", "browser", "operator"})
SDK_AUDIENCES = frozenset({"management", "public", "server"})
PAGINATION = {"cursor": "cursor", "limit": "limit", "items": "items", "next": "next_cursor"}


class ContractError(ValueError):
    """The canonical contract cannot safely describe or generate the API."""


class UniqueKeyLoader(yaml.SafeLoader):
    """Reject duplicate keys instead of silently discarding earlier declarations."""


def _mapping(loader: UniqueKeyLoader, node: yaml.MappingNode, deep: bool = False) -> dict:
    result = {}
    for key_node, value_node in node.value:
        key = loader.construct_object(key_node, deep=deep)
        if key in result:
            raise ContractError(f"duplicate YAML key {key!r} at {key_node.start_mark}")
        result[key] = loader.construct_object(value_node, deep=deep)
    return result


UniqueKeyLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _mapping)


def operations(document: dict) -> Iterator[tuple[str, str, dict, dict]]:
    for path, path_item in document["paths"].items():
        for method, operation in path_item.items():
            if method in HTTP_METHODS:
                yield path, method, operation, path_item


def resolve(document: dict, reference: str) -> Any:
    if not reference.startswith("#/"):
        raise ContractError(f"external reference is not self-contained: {reference}")
    value: Any = document
    try:
        for key in reference[2:].split("/"):
            value = value[key.replace("~1", "/").replace("~0", "~")]
    except (KeyError, TypeError) as exc:
        raise ContractError(f"unresolved reference: {reference}") from exc
    return value


def references(value: Any) -> Iterator[str]:
    if isinstance(value, dict):
        if "$ref" in value:
            yield value["$ref"]
        for child in value.values():
            yield from references(child)
    elif isinstance(value, list):
        for child in value:
            yield from references(child)


def _pascal(value: str) -> str:
    return "".join(segment[0].upper() + segment[1:] for segment in value.split("."))


def load(path: Path) -> dict:
    with path.open(encoding="utf-8") as stream:
        document = yaml.load(stream, Loader=UniqueKeyLoader)
    validate_contract(document)
    return document


def validate_contract(document: dict) -> None:
    if document.get("openapi") != "3.0.3":
        raise ContractError("canonical OpenAPI dialect must be 3.0.3")
    if document.get("servers") != [{"url": "/"}]:
        raise ContractError("canonical server must be the deployment-independent instance root")
    for reference in references(document):
        resolve(document, reference)
    identities, ids, public_names = set(), set(), set()
    for path, method, operation, path_item in operations(document):
        if not (path.startswith("/api/v1/") or path in {"/a/e", "/a.js"} or path.startswith("/m/")):
            raise ContractError(f"operation is outside the application-route seam: {method} {path}")
        if "servers" in operation or "servers" in path_item:
            raise ContractError(f"operation-specific servers are forbidden: {method} {path}")
        identity = (method, re.sub(r"\{[^/{}]+\}", "{}", path.rstrip("/")))
        if identity in identities:
            raise ContractError(f"duplicate normalized operation: {method} {path}")
        identities.add(identity)
        operation_id = operation.get("operationId")
        if not operation_id or operation_id in ids:
            raise ContractError(f"missing or duplicate operationId: {operation_id!r}")
        ids.add(operation_id)
        audience = operation.get("x-manyforge-audience")
        resource = operation.get("x-manyforge-resource", "")
        public_method = operation.get("x-manyforge-method", "")
        if audience not in AUDIENCES:
            raise ContractError(f"invalid audience on {operation_id}: {audience!r}")
        if not re.fullmatch(r"[a-z][a-zA-Z0-9]*(?:\.[a-z][a-zA-Z0-9]*)*", resource):
            raise ContractError(f"invalid resource on {operation_id}: {resource!r}")
        if not re.fullmatch(r"[a-z][a-zA-Z0-9]*", public_method):
            raise ContractError(f"invalid public method on {operation_id}: {public_method!r}")
        if operation.get("tags") != [_pascal(resource)]:
            raise ContractError(f"expected one resource-derived tag on {operation_id}")
        business = operation.get("x-manyforge-business-param")
        if business is not None and (business != "id" or not path.startswith("/api/v1/businesses/{id}/")):
            raise ContractError(f"invalid business binding on {operation_id}")
        name = (audience, bool(business), resource, public_method)
        if name in public_names:
            raise ContractError(f"public method collision: {name}")
        public_names.add(name)
        signing = operation.get("x-manyforge-signing")
        if signing is not None and signing not in {"feedback", "telemetry", "mailing"}:
            raise ContractError(f"invalid signing protocol on {operation_id}")
        if audience in {"public", "server", "callback", "browser"} and operation.get("security") != []:
            raise ContractError(f"non-management operation needs explicit security: [] on {operation_id}")
        pagination = operation.get("x-manyforge-pagination")
        if pagination is not None:
            if pagination != PAGINATION or method != "get":
                raise ContractError(f"invalid cursor pagination on {operation_id}")
            parameters = path_item.get("parameters", []) + operation.get("parameters", [])
            parameters = [resolve(document, p["$ref"]) if "$ref" in p else p for p in parameters]
            queries = {p["name"] for p in parameters if p["in"] == "query"}
            if not {"cursor", "limit"}.issubset(queries):
                raise ContractError(f"pagination query parameters missing on {operation_id}")
    validate(document)


def sdk_projection(document: dict) -> dict:
    """Select operation audiences, then retain the transitive component closure."""
    result = {key: copy.deepcopy(value) for key, value in document.items() if key not in {"paths", "components", "tags"}}
    result["paths"] = {}
    tags = set()
    for path, method, operation, path_item in operations(document):
        if operation["x-manyforge-audience"] not in SDK_AUDIENCES:
            continue
        selected = result["paths"].setdefault(path, {
            key: copy.deepcopy(value) for key, value in path_item.items() if key not in HTTP_METHODS
        })
        selected[method] = copy.deepcopy(operation)
        tags.update(operation["tags"])
    result["tags"] = [{"name": tag} for tag in sorted(tags)]
    result["components"] = {}
    pending = list(references(result))
    for security in [result.get("security", [])] + [op.get("security", []) for _, _, op, _ in operations(result)]:
        pending.extend(f"#/components/securitySchemes/{name}" for requirement in security for name in requirement)
    included = set()
    while pending:
        reference = pending.pop()
        if reference in included:
            continue
        included.add(reference)
        parts = reference.split("/")
        if len(parts) < 4 or parts[1] != "components":
            raise ContractError(f"SDK projection requires component-local references: {reference}")
        section, name = parts[2:4]
        component_ref = f"#/components/{section}/{name}"
        component = copy.deepcopy(resolve(document, component_ref))
        result["components"].setdefault(section, {})[name] = component
        pending.extend(references(component))
    return result


def json_bytes(value: Any) -> bytes:
    return (json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2, allow_nan=False) + "\n").encode("utf-8")


def main() -> None:
    import argparse
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--contract", type=Path, default=Path(__file__).resolve().parents[2] / "api/openapi.yaml")
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    document = load(args.contract)
    projection = sdk_projection(document)
    validate(projection)
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_bytes(json_bytes(projection))
    print(f"Validated {sum(1 for _ in operations(document))} application operations; {sum(1 for _ in operations(projection))} SDK operations")


if __name__ == "__main__":
    main()
