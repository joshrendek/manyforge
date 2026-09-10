"""Language-neutral resource metadata; OpenAPI Generator remains the type engine."""
from __future__ import annotations

import copy
import re

from contract import operations, resolve


def pascal(value: str) -> str:
    return "".join(part[:1].upper() + part[1:] for part in re.split(r"[._-]", value))


def snake(value: str) -> str:
    return re.sub(r"(?<!^)(?=[A-Z])", "_", value).lower()


def _allow_missing_cursor(document: dict, schema: dict, cursor: str, seen: set[str]) -> None:
    reference = schema.get("$ref")
    if reference:
        if reference in seen:
            return
        seen.add(reference)
        schema = resolve(document, reference)
    if cursor in schema.get("required", []):
        required = [field for field in schema["required"] if field != cursor]
        if required:
            schema["required"] = required
        else:
            schema.pop("required")
    for member in schema.get("allOf", []):
        _allow_missing_cursor(document, member, cursor, seen)


def prepare(document: dict) -> dict:
    """Add template-only bindings without changing the canonical wire contract."""
    result = copy.deepcopy(document)
    groups = {}
    for path, method, operation, path_item in operations(result):
        audience = operation["x-manyforge-audience"]
        resource = operation["x-manyforge-resource"]
        business = operation.get("x-manyforge-business-param")
        public_method = operation["x-manyforge-method"]
        if audience == "management":
            owner = "business" if business else "root"
        elif audience == "server":
            owner = "mailingServer"
        elif path.startswith("/api/v1/feedback/"):
            owner = "feedback"
        elif path.startswith("/api/v1/telemetry/"):
            owner = "telemetry"
        elif path.startswith("/api/v1/mailing/"):
            owner = "mailing"
        elif path == "/a/e":
            owner = "analytics"
        else:
            raise ValueError(f"SDK operation has no credential owner: {method} {path}")
        tag = pascal(owner) + pascal(resource)
        operation["tags"] = [tag]
        operation["x-sdk-owner"] = owner
        operation["x-sdk-tag"] = tag
        operation["x-sdk-resource"] = resource
        operation["x-sdk-method-python"] = snake(public_method)
        operation["x-sdk-method-go"] = pascal(public_method)
        operation["x-sdk-method"] = public_method
        operation["x-sdk-authenticated"] = bool(operation.get("security", result.get("security", [])))
        operation["x-sdk-path"] = path
        operation["x-sdk-http-method"] = method.upper()
        operation["x-sdk-pagination"] = "x-manyforge-pagination" in operation
        if operation["x-sdk-pagination"]:
            # The SDK explicitly accepts an omitted terminal cursor, even when
            # the current handler always emits it. Keep all other fields strict.
            for status, response in operation["responses"].items():
                if not status.startswith("2"):
                    continue
                response = resolve(result, response["$ref"]) if "$ref" in response else response
                for media in response.get("content", {}).values():
                    _allow_missing_cursor(result, media["schema"], operation["x-manyforge-pagination"]["next"], set())
        operation["x-sdk-stream"] = any("text/csv" in response.get("content", {}) for response in operation["responses"].values())
        parameters = path_item.get("parameters", []) + operation.get("parameters", [])
        operation["parameters"] = []
        for parameter in parameters:
            parameter = copy.deepcopy(resolve(result, parameter["$ref"]) if "$ref" in parameter else parameter)
            if parameter["in"] == "header" and parameter["name"].lower() == "origin" and audience != "management":
                continue  # Browsers supply Origin; server public transports own sourceOrigin.
            if parameter["in"] == "header" and parameter["name"].lower() in {"x-feedback-signature", "x-telemetry-signature", "x-mailing-signature", "x-mailing-timestamp"}:
                continue  # Signed transports own these credentials; never browser method arguments.
            parameter["x-sdk-bound"] = parameter["in"] == "path" and (parameter["name"] == business or (audience != "management" and parameter["name"] == "key"))
            operation["parameters"].append(parameter)
        request = operation.get("requestBody")
        if request and "$ref" in request:
            operation["requestBody"] = copy.deepcopy(resolve(result, request["$ref"]))
        if owner == "analytics":
            operation["x-sdk-body-key"] = "k"
            # A public client's credential is constructor-owned, not a required
            # field callers repeat in every event. The transport inserts it into
            # the serialized body before signing/sending; canonical YAML is intact.
            for media in operation["requestBody"]["content"].values():
                schema = media["schema"]
                schema = resolve(result, schema["$ref"]) if "$ref" in schema else schema
                schema.get("properties", {}).pop("k", None)
                required = [name for name in schema.get("required", []) if name != "k"]
                if required:
                    schema["required"] = required
                else:
                    schema.pop("required", None)
        # The native code generators choose the language types and model imports.
        group = groups.setdefault(tag, {"tag": tag, "owner": owner, "resource": resource, "business": bool(business), "operations": []})
        group["operations"].append({"operationId": operation["operationId"], "method": public_method, "pagination": operation.get("x-manyforge-pagination")})
    for path_item in result["paths"].values():
        path_item.pop("parameters", None)
    result["tags"] = [{"name": name} for name in sorted(groups)]
    result["x-sdk-resources"] = [groups[name] for name in sorted(groups)]
    return result
