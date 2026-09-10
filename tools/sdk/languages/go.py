"""Go's native generator owns schema/type conversion; templates own SDK ergonomics.

Copyright (c) 2026 ManyForge contributors. SPDX-License-Identifier: MIT
"""
from __future__ import annotations

import copy
import json
import shutil
import subprocess
import tempfile
from pathlib import Path

from contract import operations, resolve
from prepare import pascal


def _schema(document: dict, schema: dict) -> dict:
    return resolve(document, schema["$ref"]) if "$ref" in schema else schema


def _shape(document: dict, schema: dict) -> tuple[set[str], dict]:
    schema = _schema(document, schema)
    required = set(schema.get("required", []))
    properties = dict(schema.get("properties", {}))
    for member in schema.get("allOf", []):
        child_required, child_properties = _shape(document, member)
        required.update(child_required)
        properties.update(child_properties)
    return required, properties


def _metadata(document: dict) -> tuple[dict, list[dict], list[dict]]:
    document = copy.deepcopy(document)
    schemas = document["components"]["schemas"]
    # Named inline enums keep known constants without closing their string type.
    def enums(value: dict, prefix: str) -> None:
        for name, prop in list(value.get("properties", {}).items()):
            if prop.get("type") == "string" and prop.get("enum"):
                enum_name = prefix + pascal(name)
                enum_schema = {key: prop[key] for key in ("type", "enum", "description", "nullable") if key in prop}
                enum_schema["enum"] = [v for v in enum_schema["enum"] if v is not None]
                if enum_name in schemas and schemas[enum_name] != enum_schema:
                    raise ValueError(f"Go inline enum name collision: {enum_name}")
                schemas[enum_name] = enum_schema
                value["properties"][name] = {"$ref": f"#/components/schemas/{enum_name}", **{key: item for key, item in prop.items() if key not in {"type", "enum", "description"}}}
    for name, schema in list(schemas.items()):
        enums(schema, name)
    for name, schema in schemas.items():
        required, properties = _shape(document, schema)
        def uses_time(value: dict) -> bool:
            return value.get("format") == "date-time" or any(uses_time(value[k]) for k in ("items", "additionalProperties") if isinstance(value.get(k), dict))
        schema["x-go-token-pair"] = name == "TokenPair"
        schema["x-go-has-time"] = schema["x-go-token-pair"] or any(uses_time(prop) for prop in properties.values())
        alternatives = schema.get("oneOf", schema.get("anyOf", []))
        if alternatives:
            variants = []
            for alternative in alternatives:
                if "$ref" not in alternative:
                    raise ValueError(f"Go union {name} requires named alternatives")
                child_required, child_properties = _shape(document, alternative)
                literals = {}
                for prop_name, prop in child_properties.items():
                    values = _schema(document, prop).get("enum")
                    if values and all(isinstance(item, str) for item in values):
                        literals[prop_name] = values
                variants.append({"name": alternative["$ref"].split("/")[-1], "required": ", ".join(json.dumps(v) for v in sorted(child_required)), "literals": ", ".join(json.dumps(key) + ": {" + ", ".join(json.dumps(v) for v in values) + "}" for key, values in sorted(literals.items()))})
            schema["x-go-union"] = variants
            schema["x-go-is-union"] = True
            schema["x-go-any-of"] = "anyOf" in schema
    manifest = []
    groups: dict[str, list[dict]] = {}
    for path, method, operation, _ in operations(document):
        original = operation["operationId"]
        operation["x-go-original-id"] = original
        operation["x-go-params"] = "TicketListParams" if operation["x-sdk-tag"] == "BusinessTickets" and operation["x-sdk-method"] == "list" else operation["x-sdk-tag"] + pascal(operation["x-sdk-method"]) + "Params"
        operation["x-go-has-params"] = any(p["in"] in {"query", "header"} for p in operation.get("parameters", [])) or "multipart/form-data" in operation.get("requestBody", {}).get("content", {})
        for parameter in operation.get("parameters", []):
            if parameter["in"] == "query" and parameter["name"] in {"limit", "cursor"}:
                parameter["x-go-direct"] = True
                parameter["x-go-zero"] = '""' if parameter["name"] == "cursor" else "0"
            parameter["x-go-explode"] = parameter.get("explode", parameter.get("style", "form") == "form")
            parameter["x-go-style"] = parameter.get("style", "form" if parameter["in"] == "query" else "simple")
        if operation["x-sdk-pagination"]:
            response = next(_schema(document, response) for status, response in operation["responses"].items() if status.startswith("2") and "application/json" in _schema(document, response).get("content", {}))
            page = _schema(document, response["content"]["application/json"]["schema"])
            _, page_properties = _shape(document, page)
            item = page_properties["items"]["items"]
            if "$ref" not in item:
                raise ValueError(f"Go cursor operation {original} needs named item schema")
            operation["x-go-item-type"] = item["$ref"].split("/")[-1]
        groups.setdefault(operation["x-sdk-tag"], []).append(operation)
        manifest.append({"operationId": original, "resource": operation["x-sdk-resource"], "method": operation["x-sdk-method"], "audience": operation["x-manyforge-audience"], "businessParam": operation.get("x-manyforge-business-param"), "pagination": operation.get("x-manyforge-pagination")})
    # Resource tree metadata is layout only; all methods/types come from the generator.
    trees: dict[str, dict] = {}
    for group in document["x-sdk-resources"]:
        owner = group["owner"]
        tree = trees.setdefault(owner, {"type": pascal(owner) + "Resources", "children": {}})
        parts = group["resource"].split(".")
        if owner != "business" and owner != "root" and len(parts) == 1 and parts[0] in {"analytics", "telemetry", "mailing"}:
            tree["direct"] = group["tag"] + "Resource"
            tree["directTag"] = group["tag"]
            continue
        current = tree
        for i, part in enumerate(parts):
            current = current["children"].setdefault(part, {"type": pascal(owner) + "".join(pascal(p) for p in parts[:i + 1]) + "Resource", "children": {}})
        current["tag"] = group["tag"]
    scope_types = []
    def flatten(node: dict) -> None:
        children = [{"name": pascal(name), "type": child["type"]} for name, child in sorted(node["children"].items())]
        if "tag" in node:
            groups[node["tag"]][0]["x-go-children"] = children
        else:
            scope_types.append({"type": node["type"], "children": children, "direct": node.get("direct")})
        for child in node["children"].values():
            flatten(child)
    for tree in trees.values():
        flatten(tree)
    constructors = []
    def expression(node: dict) -> str:
        fields = ["transport: transport", "binding: binding"]
        if node.get("direct"):
            fields.append(node["direct"] + ": &" + node["direct"] + "{transport: transport, binding: binding}")
        fields.extend(pascal(name) + ": " + expression(child) for name, child in sorted(node["children"].items()))
        return "&" + node["type"] + "{" + ", ".join(fields) + "}"
    for owner, tree in sorted(trees.items()):
        constructors.append({"name": pascal(owner), "type": tree["type"], "expression": expression(tree)})
    for tag, group in groups.items():
        group[0]["x-go-import-io"] = any(op["x-sdk-stream"] for op in group)
        group[0]["x-go-import-path"] = any(any(p["in"] == "path" for p in op.get("parameters", [])) for op in group)
        group[0]["x-go-import-fmt"] = any(any(p["in"] in {"path", "header"} for p in op.get("parameters", [])) for op in group)
    return document, sorted(manifest, key=lambda item: item["operationId"]), [{"types": scope_types, "constructors": constructors}]


def generate(root: Path, output: Path, jar: Path, document: dict, versions: dict[str, str]) -> None:
    prepared, manifest, scopes = _metadata(document)
    config = json.loads((root / "tools/sdk/config/go.json").read_text())
    config.update({"x-go-scopes": scopes, "packageVersion": versions["go"], "x-go-release-version": versions["python"], "files": {"resources.mustache": {"templateType": "SupportingFiles", "destinationFilename": "resources.go"}, "version.mustache": {"templateType": "SupportingFiles", "destinationFilename": "version.go"}, "module.mustache": {"templateType": "SupportingFiles", "destinationFilename": "go.mod"}}})
    with tempfile.TemporaryDirectory(prefix="manyforge-go-generator-") as temporary:
        temp = Path(temporary)
        (temp / "input.json").write_text(json.dumps(prepared, sort_keys=True))
        (temp / "config.json").write_text(json.dumps(config, sort_keys=True))
        command = ["java", "-jar", str(jar), "generate", "-g", "go", "-c", str(temp / "config.json"), "-i", str(temp / "input.json"), "-t", str(root / "tools/sdk/templates/go"), "-o", str(temp / "generated"), "--global-property", "models,apis,supportingFiles=resources.go:version.go:go.mod,modelDocs=false,apiDocs=false,modelTests=false,apiTests=false", "--openapi-normalizer", "SIMPLIFY_ONEOF_ANYOF=false"]
        result = subprocess.run(command, capture_output=True, text=True)
        if result.returncode:
            raise RuntimeError("Go OpenAPI generation failed:\n" + result.stdout + result.stderr)
        output.mkdir(parents=True, exist_ok=True)
        for file in sorted((temp / "generated").iterdir()):
            if file.suffix == ".go" or file.name == "go.mod":
                shutil.copyfile(file, output / file.name)
    subprocess.run(["gofmt", "-w", *[str(path) for path in sorted(output.glob("*.go"))]], check=True)
    (output / "operation-manifest.json").write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n")
