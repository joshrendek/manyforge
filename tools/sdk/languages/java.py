"""Java/native OpenAPI Generator adapter; no independent schema/type generation."""
from __future__ import annotations

import copy
import json
from pathlib import Path
import shutil
import subprocess
import tempfile


def _pascal(value: str) -> str:
    return "".join(part[:1].upper() + part[1:] for part in value.split("."))


def generate(root: Path, output: Path, jar: Path, document: dict, versions: dict[str, str]) -> None:
    prepared = copy.deepcopy(document)
    for name, schema in prepared["components"]["schemas"].items():
        if schema.get("type") == "object" and schema.get("additionalProperties") is False and not schema.get("properties"):
            # A semantic-equivalent allOf retains the named empty type instead
            # of OAG's Object alias, without inventing any model fields.
            prepared["components"]["schemas"][name] = {"allOf": [schema]}
    manifest = []
    for path_item in prepared["paths"].values():
        for operation in path_item.values():
            if not isinstance(operation, dict) or "operationId" not in operation:
                continue
            resource = operation["x-sdk-resource"]
            method = operation["x-sdk-method"]
            stem = "Ticket" if resource == "tickets" else operation["x-sdk-tag"]
            operation["x-java-params"] = stem + _pascal(method) + "Params"
            operation["x-java-default-params"] = not operation.get("requestBody") and not any(
                parameter.get("required") and not parameter.get("x-sdk-bound")
                for parameter in operation.get("parameters", [])
            )
            pagination = operation.get("x-manyforge-pagination")
            if pagination:
                operation["x-java-cursor"] = pagination["cursor"]
                response = next(value for status, value in operation["responses"].items() if status.startswith("2"))
                if "$ref" in response:
                    response = prepared["components"]["responses"][response["$ref"].rsplit("/", 1)[1]]
                schema = response["content"]["application/json"]["schema"]
                if "$ref" in schema:
                    schema = prepared["components"]["schemas"][schema["$ref"].rsplit("/", 1)[1]]
                item = schema["properties"][pagination["items"]]["items"]
                operation["x-java-item"] = item["$ref"].rsplit("/", 1)[1]
            manifest.append({"operationId": operation["operationId"], "resource": resource,
                             "method": method, "audience": operation["x-manyforge-audience"],
                             "businessParam": operation.get("x-manyforge-business-param"),
                             "pagination": pagination})
    with tempfile.TemporaryDirectory(prefix="manyforge-java-") as temporary:
        temporary = Path(temporary)
        source = temporary / "input.json"
        source.write_text(json.dumps(prepared, sort_keys=True))
        config = json.loads((root / "tools/sdk/config/java.json").read_text())
        config["artifactVersion"] = versions["java"]
        config_file = temporary / "config.json"
        config_file.write_text(json.dumps(config, sort_keys=True))
        generated = temporary / "generated"
        subprocess.run(["java", "-jar", str(jar), "generate", "-g", "java", "-c", str(config_file),
                        "-i", str(source), "-o", str(generated), "-t", str(root / "tools/sdk/templates/java"),
                        "--api-name-suffix", "Resource", "--type-mappings", "file=Upload,binary=Upload",
                        "--import-mappings", "Upload=com.manyforge.sdk.Upload", "--name-mappings", "file=file",
                        "--generate-alias-as-model",
                        "--openapi-normalizer", "SIMPLIFY_ONEOF_ANYOF=false,SIMPLIFY_BOOLEAN_ENUM=false,SIMPLIFY_ONEOF_ANYOF_ENUM=false",
                        "--global-property", "models,apis,modelDocs=false,apiDocs=false,modelTests=false,apiTests=false,skipFormModel=false"], check=True)
        for source_file in sorted((generated / "src/main/java").rglob("*.java")):
            destination = output / source_file.relative_to(generated)
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(source_file, destination)
    schemas_file = output / "src/main/resources/com/manyforge/sdk/union-schemas.json"
    schemas_file.parent.mkdir(parents=True, exist_ok=True)
    schemas_file.write_text(json.dumps(_union_schemas(document["components"]["schemas"]), sort_keys=True, separators=(",", ":")) + "\n")
    output.mkdir(parents=True, exist_ok=True)
    (output / "operation-manifest.json").write_text(json.dumps(sorted(manifest, key=lambda row: row["operationId"]), indent=2) + "\n")
    _trees(output, document["x-sdk-resources"])
    for template, destination in (("pom.xml", "pom.xml"), ("Version.java", "src/main/java/com/manyforge/sdk/Version.java")):
        target = output / destination
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text((root / "tools/sdk/templates/java" / template).read_text().replace("@VERSION@", versions["java"]))


def _union_schemas(schemas: dict) -> dict:
    """Retain only the shape metadata consumed by the composed-model selector."""
    selected = {}
    def include(name: str) -> None:
        if name in selected:
            return
        selected[name] = {}
        selected[name] = shape(schemas[name])
    def shape(schema: dict) -> dict:
        result = {key: schema[key] for key in ("type", "nullable", "required", "enum") if key in schema}
        if "$ref" in schema:
            result["$ref"] = schema["$ref"]
            include(schema["$ref"].rsplit("/", 1)[1])
        for key in ("oneOf", "anyOf", "allOf"):
            if key in schema:
                result[key] = [shape(value) for value in schema[key]]
        if "properties" in schema:
            result["properties"] = {key: shape(value) for key, value in schema["properties"].items()}
        if "items" in schema:
            result["items"] = shape(schema["items"])
        return result
    for name, schema in schemas.items():
        if "oneOf" in schema or "anyOf" in schema:
            include(name)
    return selected

def _trees(output: Path, resources: list[dict]) -> None:
    owners: dict[str, dict] = {}
    for resource in resources:
        owner = resource["owner"]
        branch = owners.setdefault(owner, {})
        segments = resource["resource"].split(".")
        if owner not in {"root", "business"} and segments[0] == owner.removesuffix("Server"):
            segments = segments[1:]
        for segment in segments:
            branch = branch.setdefault(segment, {})
        branch["@tag"] = resource["tag"]
    rows = ["// Generated resource ownership tree. Do not edit.", "package com.manyforge.sdk;", "",
            "import com.manyforge.sdk.resources.*;", "import java.util.Objects;", "",
            "public final class Resources {", "  private Resources() {}"]
    def emit(name: str, branch: dict) -> None:
        tag = branch.get("@tag")
        parent = f" extends {tag}Resource" if tag else ""
        rows.append(f"  public static class {name}{parent} {{")
        if not tag:
            rows.extend(["    protected final Transport transport;", "    protected final String binding;"])
        for segment in sorted(key for key in branch if key != "@tag"):
            child = name + _pascal(segment)
            rows.append(f"    private final {child} {segment};")
        rows.append(f"    public {name}(Transport transport, String binding) {{")
        rows.append("      super(transport, binding);" if tag else "      this.transport = Objects.requireNonNull(transport); this.binding = binding;")
        for segment in sorted(key for key in branch if key != "@tag"):
            rows.append(f"      this.{segment} = new {name + _pascal(segment)}(transport, binding);")
        rows.append("    }")
        for segment in sorted(key for key in branch if key != "@tag"):
            rows.append(f"    public {name + _pascal(segment)} {segment}() {{ return {segment}; }}")
        rows.append("  }")
        for segment in sorted(key for key in branch if key != "@tag"):
            emit(name + _pascal(segment), branch[segment])
    for owner in sorted(owners):
        emit(_pascal(owner), owners[owner])
    rows.append("}")
    destination = output / "src/main/java/com/manyforge/sdk/Resources.java"
    destination.write_text("\n".join(rows) + "\n")
