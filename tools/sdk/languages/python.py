"""Python generation: OpenAPI Generator owns every model and operation type."""
from __future__ import annotations

import copy
import json
import re
import shutil
import subprocess
import tempfile
from pathlib import Path

from contract import operations, resolve
from prepare import pascal, snake


def _prepare(document: dict) -> dict:
    document = copy.deepcopy(document)
    schemas = document["components"]["schemas"]

    def lift_enums(node: object, name: str, top: bool = False) -> None:
        if isinstance(node, list):
            for index, item in enumerate(node):
                lift_enums(item, name + str(index))
        elif isinstance(node, dict):
            node.pop("default", None)
            if node.get("type") in ("integer", "number", "boolean") and "enum" in node:
                node["x-python-closed-enum"] = True
                node["x-python-enum-values"] = ", ".join(repr(value) for value in node["enum"])
            if node.get("type") == "string" and "enum" in node and not top:
                enum_name = name + "Enum"
                enum = {key: value for key, value in node.items() if key in {"type", "enum", "description"}}
                enum["enum"] = [value for value in enum["enum"] if value is not None]
                if enum_name in schemas and schemas[enum_name] != enum:
                    raise ValueError(f"Python enum name collision: {enum_name}")
                schemas[enum_name] = enum
                nullable = node.get("nullable", False)
                node.clear()
                node["$ref"] = "#/components/schemas/" + enum_name
                if nullable:
                    node["nullable"] = True
                return
            for key, value in list(node.items()):
                if key not in {"example", "examples", "enum"}:
                    lift_enums(value, name + pascal(key))

    for name, schema in list(schemas.items()):
        lift_enums(schema, name, top=True)
    for _, _, operation, _ in operations(document):
        operation["x-python-operation-id"] = operation["operationId"]
        for parameter in operation.get("parameters", []):
            lift_enums(parameter.get("schema", {}), pascal(operation["operationId"]) + pascal(parameter["name"]))
            parameter["x-python-style"] = parameter.get("style", "form")
            parameter["x-python-explode"] = "True" if parameter.get("explode", parameter.get("style", "form") == "form") else "False"
            parameter["x-python-cursor"] = parameter["name"] == operation.get("x-manyforge-pagination", {}).get("cursor")
        if operation["x-sdk-owner"] == "root" and operation["x-sdk-resource"] == "auth" and operation["x-sdk-method"] in {"login", "refresh", "logout"}:
            fields = [{"name": name} for name in (["email", "password"] if operation["x-sdk-method"] == "login" else ["refresh_token"])]
            operation["x-python-auth-fields"] = fields
            operation["requestBody"]["x-python-hide"] = True
            operation["requestBody"]["x-python-auth-fields"] = fields
        pagination = operation.get("x-manyforge-pagination")
        if pagination:
            responses = {code: resolve(document, response["$ref"]) if "$ref" in response else response for code, response in operation["responses"].items()}
            response = next(response for code, response in responses.items() if code.startswith("2") and "application/json" in response.get("content", {}))
            schema = response["content"]["application/json"]["schema"]
            schema = resolve(document, schema["$ref"]) if "$ref" in schema else schema
            item = schema["properties"][pagination["items"]]["items"]
            # Canonical pages contain named models; OAG resolves their Python imports/types.
            if "$ref" not in item:
                raise ValueError("Python cursor pages require the canonical named item schema")
            operation["x-python-page-item"] = item["$ref"].rsplit("/", 1)[1]
            operation["x-python-page-items"] = snake(pagination["items"])
            operation["x-python-page-next"] = snake(pagination["next"])
    return document


def _trees(document: dict, package: Path) -> None:
    lines = ['"""Generated immutable resource scopes (OpenAPI Generator operations)."""', "from __future__ import annotations", "from types import MappingProxyType", "from collections.abc import Mapping", "from manyforge.transport import SyncTransport, AsyncTransport"]
    nodes: dict[tuple[str, str], dict[str, str]] = {}
    for resource in document["x-sdk-resources"]:
        owner, path, tag = resource["owner"], resource["resource"], resource["tag"]
        module = snake(tag) + "_resource"
        lines.append(f"from manyforge.api.{module} import {tag}Resource, Async{tag}Resource")
        segments = path.split(".")
        # Public clients expose their local nouns, not repeated product names.
        if owner not in {"root", "business"} and segments[0] in {"feedback", "telemetry", "mailing", "analytics"}:
            segments = segments[1:]
        current = ""
        for segment in segments:
            child = current + "." + segment if current else segment
            nodes.setdefault((owner, current), {})[snake(segment)] = pascal(owner) + pascal(child) + "Scope"
            current = child
        nodes.setdefault((owner, current), {})["__resource__"] = tag + "Resource"
    for asynchronous in (False, True):
        prefix = "Async" if asynchronous else ""
        transport = prefix + "Transport" if asynchronous else "SyncTransport"
        for (owner, path), children in sorted(nodes.items(), key=lambda item: (-item[0][1].count("."), item[0])):
            name = prefix + pascal(owner) + pascal(path) + "Scope"
            base = prefix + children["__resource__"] if "__resource__" in children else "object"
            lines += ["", f"class {name}({base}):", f"    def __init__(self, transport: {transport}, bindings: Mapping[str, str] | None = None) -> None:", "        self._transport = transport", "        self._bindings = MappingProxyType(dict(bindings or {}))"]
            for child, target in sorted(children.items()):
                if child != "__resource__":
                    lines += ["", "    @property", f"    def {child}(self) -> {prefix}{target}:", f"        return {prefix}{target}(self._transport, self._bindings)"]
    (package / "resources.py").write_text("\n".join(lines) + "\n")


def generate(root: Path, output: Path, jar: Path, document: dict, versions: dict[str, str]) -> None:
    templates = root / "tools/sdk/templates/python"
    prepared = _prepare(document)
    output.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="manyforge-python-") as temporary:
        temporary = Path(temporary)
        source = temporary / "openapi.json"
        source.write_text(json.dumps(prepared, sort_keys=True))
        command = ["java", "-jar", str(jar), "generate", "-i", str(source), "-c", str(root / "tools/sdk/config/python.json"), "-t", str(templates), "-o", str(temporary / "generated"), "--openapi-normalizer", "SIMPLIFY_ONEOF_ANYOF=false,SIMPLIFY_ONEOF_ANYOF_ENUM=false", "--global-property", "models,apis,modelTests=false,apiTests=false,modelDocs=false,apiDocs=false"]
        subprocess.run(command, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        source_package = temporary / "generated/manyforge"
        package = output / "src/manyforge"
        package.mkdir(parents=True, exist_ok=True)
        for directory in ("models", "api"):
            shutil.copytree(source_package / directory, package / directory)
            (package / directory / "__init__.py").write_text('"""Generated ' + directory + '."""\n')
        _trees(document, package)
        # Rebuild deferred/cyclic model annotations after every model is imported.
        imports = []
        for model in sorted((package / "models").glob("*.py")):
            if model.name == "__init__.py":
                continue
            classes = re.findall(r"^class (\w+)\(", model.read_text(), flags=re.MULTILINE)
            imports.extend(f"from .{model.stem} import {name}" for name in classes)
        (package / "models/__init__.py").write_text('"""Generated model exports."""\n' + "\n".join(imports) + '\n\nfrom pydantic import BaseModel as _BaseModel\nfor _model in tuple(globals().values()):\n    if isinstance(_model, type) and issubclass(_model, _BaseModel) and _model is not _BaseModel:\n        _model.model_rebuild(_types_namespace=globals())\ndel _model, _BaseModel\n')
        (package / "py.typed").write_text("")
        (package / "_version.py").write_text((templates / "version.py.template").read_text().replace("@VERSION@", versions["python"]))
        (output / "pyproject.toml").write_text((templates / "pyproject.toml.template").read_text().replace("@VERSION@", versions["python"]))
    manifest = [{"operationId": operation["operationId"], "resource": operation["x-manyforge-resource"], "method": operation["x-manyforge-method"], "audience": operation["x-manyforge-audience"], "businessParam": operation.get("x-manyforge-business-param"), "pagination": operation.get("x-manyforge-pagination")} for _, _, operation, _ in operations(document)]
    (output / "operation-manifest.json").write_text(json.dumps(sorted(manifest, key=lambda row: row["operationId"]), indent=2) + "\n")
