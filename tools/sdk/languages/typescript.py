"""OpenAPI Generator owns TypeScript types and API methods; this adapter binds scopes."""
from __future__ import annotations

import copy
import json
from pathlib import Path
import shutil
import subprocess
import tempfile

from contract import operations, resolve
from prepare import pascal


def _json(value: object) -> str:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


def _metadata(document: dict) -> dict:
    result = copy.deepcopy(document)
    for schema in result["components"]["schemas"].values():
        alternatives = schema.get("oneOf", [])
        schema["x-ts-reference-union"] = bool(alternatives) and all("$ref" in alternative for alternative in alternatives)
    for _, _, operation, _ in operations(result):
        operation["x-ts-operation-id"] = operation["operationId"]
        operation["x-ts-params-name"] = operation["x-sdk-tag"] + pascal(operation["x-sdk-method"]) + "Params"
        operation["x-ts-required-params"] = any(parameter.get("required", False) and not parameter.get("x-sdk-bound", False) for parameter in operation.get("parameters", [])) or bool(operation.get("requestBody", {}).get("required"))
        for parameter in operation.get("parameters", []):
            parameter["x-ts-style"] = parameter.get("style", "form" if parameter["in"] == "query" else "simple")
            parameter["x-ts-explode"] = parameter.get("explode", parameter["x-ts-style"] == "form")
        body = operation.get("requestBody")
        if body:
            body["x-codegen-request-body-name"] = "body"
            body["x-ts-schema"] = _json(next(iter(body["content"].values()))["schema"])
        success = next(response for status, response in sorted(operation["responses"].items()) if status.startswith("2"))
        if "$ref" in success:
            success = resolve(result, success["$ref"])
        content = success.get("content", {})
        schema = next(iter(content.values())).get("schema", {}) if content else {}
        operation["x-ts-response-schema"] = _json(schema)
        operation["x-ts-response-kind"] = "stream" if operation["x-sdk-stream"] else "json" if content else "empty"
    return result


def _trees(document: dict, output: Path) -> None:
    owners: dict[str, dict] = {}
    for resource in document["x-sdk-resources"]:
        owner = owners.setdefault(resource["owner"], {})
        node = owner
        for segment in resource["resource"].split("."):
            node = node.setdefault(segment, {})
        node["$class"] = resource["tag"] + "Api"
    for owner, tree in sorted(owners.items()):
        resources = [resource for resource in document["x-sdk-resources"] if resource["owner"] == owner]
        lines = ["// Generated resource tree. Do not edit.", "import type { Transport } from './transport.js';"]
        for resource in resources:
            name = resource["tag"] + "Api"
            lines.append(f"import {{ {name} }} from './apis/{name}.js';")
            lines.append(f"export * from './apis/{name}.js';")
        binding = ", businessId: string" if owner == "business" else ", publishableKey: string" if owner not in {"root", "analytics"} else ""
        scope = "{ id: businessId }" if owner == "business" else "{ key: publishableKey }" if binding else "{}"
        def render_type(node: dict) -> str:
            children = "; ".join(f"readonly {key}: {render_type(value)}" for key, value in sorted(node.items()) if key != "$class")
            base = f"Readonly<{node['$class']}>" if "$class" in node else None
            shape = "{ " + children + " }"
            return f"{base} & {shape}" if base and children else base or shape

        lines.append(f"\nexport type {pascal(owner)}Resources = {render_type(tree)};")
        lines.append(f"\nexport function create{pascal(owner)}Resources(transport: Transport{binding}): {pascal(owner)}Resources {{")
        if binding:
            argument = "businessId" if owner == "business" else "publishableKey"
            lines.append(f"    if (!{argument}) throw new TypeError('A nonempty scope identifier is required');")
        lines.append(f"    const scope = Object.freeze({scope});")

        def render(node: dict) -> str:
            children = ", ".join(f"{key}: {render(value)}" for key, value in sorted(node.items()) if key != "$class")
            instance = f"new {node['$class']}(transport, scope)" if "$class" in node else None
            expression = f"Object.assign({instance}, {{ {children} }})" if instance and children else instance or "{ " + children + " }"
            return f"Object.freeze({expression})"

        lines.extend(["    return " + render(tree) + ";", "}", ""])
        (output / "src" / f"resources-{owner}.ts").write_text("\n".join(lines))


def generate(root: Path, output: Path, jar: Path, document: dict, versions: dict[str, str]) -> None:
    config = json.loads((root / "tools/sdk/config/typescript.json").read_text())
    prepared = _metadata(document)
    config["npmVersion"] = versions["typescript"]
    config["files"] = {"schemas.mustache": {"templateType": "SupportingFiles", "destinationFilename": "schemas.json"}}
    with tempfile.TemporaryDirectory(prefix="manyforge-typescript-") as temporary:
        temporary = Path(temporary)
        specification = temporary / "openapi.json"
        configuration = temporary / "config.json"
        specification.write_text(_json(prepared))
        configuration.write_text(_json(config))
        generated = temporary / "generated"
        subprocess.run([
            "java", "-jar", str(jar), "generate", "-g", "typescript-fetch",
            "-i", str(specification), "-c", str(configuration),
            "-t", str(root / "tools/sdk/templates/typescript"), "-o", str(generated),
            "--openapi-normalizer", "SIMPLIFY_ONEOF_ANYOF=false,SIMPLIFY_BOOLEAN_ENUM=false",
            "--global-property", "models,apis,supportingFiles=schemas.json,skipFormModel=false,modelDocs=false,apiDocs=false,modelTests=false,apiTests=false",
        ], check=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        source = output / "src"
        source.mkdir(parents=True, exist_ok=True)
        for folder in ("models", "apis"):
            shutil.copytree(generated / "src" / folder, source / folder, dirs_exist_ok=True)
        generated_metadata = json.loads((generated / "schemas.json").read_text())
        schemas = copy.deepcopy(document["components"]["schemas"])
        property_names = {}
        for model in generated_metadata["models"]:
            schemas[model["name"]] = model["schema"]
            for wire_name, public_name in model["properties"]:
                previous = property_names.setdefault(wire_name, public_name)
                if previous != public_name:
                    raise ValueError(f"conflicting generated wire property mapping: {wire_name}")
        (source / "schemas.ts").write_text(
            "// Generated from OpenAPI Generator model metadata. Do not edit.\n"
            "import type { Schema } from './model-support.js';\n"
            "export const schemas: Record<string, Schema> = " + _json(schemas) + ";\n"
            "export const propertyNames: Record<string, string> = " + _json(property_names) + ";\n"
        )
        for folder in ("models", "apis"):
            paths = sorted(path for path in (source / folder).glob("*.ts") if path.name != "index.ts")
            (source / folder / "index.ts").write_text("// Generated exports. Do not edit.\n" + "".join(f"export * from './{path.stem}.js';\n" for path in paths))
    _trees(document, output)
    manifest = [{
        "operationId": operation["operationId"], "resource": operation["x-manyforge-resource"],
        "method": operation["x-manyforge-method"], "audience": operation["x-manyforge-audience"],
        "businessParam": operation.get("x-manyforge-business-param"), "pagination": operation.get("x-manyforge-pagination"),
    } for _, _, operation, _ in operations(document)]
    (output / "operation-manifest.json").write_text(json.dumps(sorted(manifest, key=lambda item: item["operationId"]), indent=2, sort_keys=True) + "\n")
    for template_name, destination in (("package.json", "package.json"), ("version.ts", "src/version.ts")):
        template = (root / "tools/sdk/templates/typescript" / template_name).read_text()
        (output / destination).write_text(template.replace("@SDK_VERSION@", versions["typescript"]))
