"""Pinned generation and full path-set drift checks for ManyForge SDK packages.

Copyright (c) 2026 ManyForge contributors. SPDX-License-Identifier: MIT
"""
from __future__ import annotations

import argparse
import copy
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import tempfile
import urllib.request

from contract import json_bytes, load, operations, sdk_projection
from prepare import prepare
from version import package_versions

ROOT = Path(__file__).resolve().parents[2]
LANGUAGES = ("python", "typescript", "go", "java")
IGNORED_DIRECTORIES = frozenset({".git", ".venv", "node_modules", "target", "dist", "build", "__pycache__", ".pytest_cache", ".mypy_cache", ".openapi-generator"})
MANIFEST = Path("sdk/generated-files.json")


def file_digest(path: Path) -> str:
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def generator_jar(root: Path) -> Path:
    toolchain = json.loads((root / "tools/sdk/toolchain.json").read_text(encoding="utf-8"))["openapi_generator"]
    cache = root / "tools/sdk/.cache"
    cache.mkdir(parents=True, exist_ok=True)
    jar = cache / f"openapi-generator-cli-{toolchain['version']}.jar"
    if not jar.exists():
        descriptor, temporary = tempfile.mkstemp(prefix="generator-", suffix=".download", dir=cache)
        try:
            with os.fdopen(descriptor, "wb") as destination:
                with urllib.request.urlopen(toolchain["url"], timeout=60) as source:
                    shutil.copyfileobj(source, destination)
            if file_digest(Path(temporary)) != toolchain["sha256"]:
                raise RuntimeError("downloaded OpenAPI Generator checksum mismatch; artifact was not executed")
            os.replace(temporary, jar)
        finally:
            Path(temporary).unlink(missing_ok=True)
    if file_digest(jar) != toolchain["sha256"]:
        raise RuntimeError(f"OpenAPI Generator checksum mismatch: {jar}; artifact was not executed")
    return jar


def package_files(package: Path) -> set[Path]:
    result = set()
    if not package.exists():
        return result
    for directory, subdirectories, filenames in os.walk(package):
        subdirectories[:] = sorted(name for name in subdirectories if name not in IGNORED_DIRECTORIES and not name.endswith(".egg-info"))
        for filename in filenames:
            if filename.endswith((".pyc", ".class", ".DS_Store")):
                continue
            path = Path(directory) / filename
            if path.is_symlink():
                raise RuntimeError(f"SDK package sources must not contain symlinks: {path}")
            result.add(path.relative_to(package))
    return result


def operation_manifest(document: dict) -> list[dict]:
    return sorted(({
        "operationId": operation["operationId"],
        "resource": operation["x-manyforge-resource"],
        "method": operation["x-manyforge-method"],
        "audience": operation["x-manyforge-audience"],
        "businessParam": operation.get("x-manyforge-business-param"),
        "pagination": operation.get("x-manyforge-pagination"),
    } for _, _, operation, _ in operations(document)), key=lambda operation: operation["operationId"])


def generate_tree(root: Path, destination: Path, jar: Path) -> tuple[dict[Path, bytes], str]:
    document = sdk_projection(load(root / "api/openapi.yaml"))
    release = (root / "sdk/version.txt").read_text(encoding="utf-8").strip()
    versions = package_versions(release)
    prepared = prepare(document)
    expected_operations = operation_manifest(document)
    files = {Path("api/sdk.openapi.json"): json_bytes(document)}
    consumer_module = root / "tools/sdk/smoke/go/go.mod.template"
    files[Path("tools/sdk/smoke/go/go.mod")] = consumer_module.read_text(encoding="utf-8").replace("@GO_VERSION@", versions["go"]).encode("utf-8")
    for language in LANGUAGES:
        module_path = root / "tools/sdk/languages" / f"{language}.py"
        spec = importlib.util.spec_from_file_location(f"manyforge_sdk_generator_{language}", module_path)
        if spec is None or spec.loader is None:
            raise RuntimeError(f"cannot load generator adapter {module_path}")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        package = destination / language
        package.mkdir(parents=True)
        module.generate(root, package, jar, copy.deepcopy(prepared), versions.copy())
        actual_operations = json.loads((package / "operation-manifest.json").read_text(encoding="utf-8"))
        if actual_operations != expected_operations:
            raise RuntimeError(f"{language} operation manifest does not match the canonical selected operation set")
        for relative in package_files(package):
            files[Path("sdk") / language / relative] = (package / relative).read_bytes()
        print(f"Generated {language}: {len(actual_operations)} operations")
    return files, release


def _safe_paths(values: list[str]) -> set[Path]:
    result = set()
    for value in values:
        path = Path(value)
        allowed_root = bool(path.parts) and (path.parts[0] in {"api", "sdk"} or path == Path("tools/sdk/smoke/go/go.mod"))
        if path.is_absolute() or ".." in path.parts or not allowed_root:
            raise RuntimeError(f"unsafe generated file manifest path: {value!r}")
        result.add(path)
    return result


def synchronize(root: Path, generated: dict[Path, bytes], release: str, check: bool) -> None:
    manifest_path = root / MANIFEST
    previous = json.loads(manifest_path.read_text(encoding="utf-8")) if manifest_path.exists() else {"generated": [], "handwritten": []}
    old_generated = _safe_paths(previous["generated"])
    known_handwritten = _safe_paths(previous["handwritten"])
    actual = {Path("sdk") / language / path for language in LANGUAGES for path in package_files(root / "sdk" / language)}
    expected = set(generated)
    if check:
        problems = []
        if not manifest_path.exists():
            problems.append("missing generated-files manifest")
        for path in sorted(expected - old_generated):
            problems.append(f"new generated path not recorded: {path}")
        for path in sorted(old_generated - expected):
            problems.append(f"obsolete generated path still recorded: {path}")
        for path in sorted(actual - expected - known_handwritten):
            problems.append(f"unexpected package file: {path}")
        for path in sorted(known_handwritten - actual):
            problems.append(f"missing handwritten package file: {path}")
        for path, data in sorted(generated.items()):
            target = root / path
            if not target.is_file():
                problems.append(f"missing generated file: {path}")
            elif target.read_bytes() != data:
                problems.append(f"generated content differs: {path}")
        if previous.get("release") != release:
            problems.append("generated release stamp differs from sdk/version.txt")
        if problems:
            raise RuntimeError("SDK generation drift:\n" + "\n".join(problems))
        print(f"SDK drift check passed: {len(expected)} generated files, including complete path sets")
        return
    for path, data in generated.items():
        target = root / path
        if target.exists() and path not in old_generated and target.read_bytes() != data:
            raise RuntimeError(f"refusing to overwrite an unowned handwritten file: {path}")
    for path in sorted(old_generated - expected):
        (root / path).unlink(missing_ok=True)
    for path, data in sorted(generated.items()):
        target = root / path
        target.parent.mkdir(parents=True, exist_ok=True)
        if not target.exists() or target.read_bytes() != data:
            target.write_bytes(data)
    handwritten = actual - old_generated - expected
    manifest_path.write_bytes(json_bytes({"release": release, "generated": sorted(str(path) for path in expected), "handwritten": sorted(str(path) for path in handwritten)}))
    print(f"Updated {len(expected)} generated files; retained {len(handwritten)} handwritten package files")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="Regenerate in isolation and compare full file sets and bytes")
    parser.add_argument("--root", type=Path, default=ROOT, help="Repository root, including temporary negative-control copies")
    args = parser.parse_args()
    root = args.root.resolve()
    jar = generator_jar(root)
    with tempfile.TemporaryDirectory(prefix="manyforge-sdk-generate-") as temporary:
        files, release = generate_tree(root, Path(temporary), jar)
        synchronize(root, files, release, args.check)


if __name__ == "__main__":
    main()
