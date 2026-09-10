"""Immutable, source-bound release artifacts shared by staging and publication.

Copyright (c) 2026 ManyForge contributors. SPDX-License-Identifier: MIT
"""
from __future__ import annotations

import base64
from datetime import datetime, timezone
import json
from pathlib import Path
import re
import shutil
import subprocess
import zipfile
import sys

from contract import json_bytes
from generate import file_digest
from version import package_versions, parse_release

COORDINATES = {
    "python": "manyforge",
    "typescript": "@manyforge/sdk",
    "go": "github.com/joshrendek/manyforge/sdk/go",
    "java": "com.manyforge:manyforge-sdk",
}
REPOSITORY = "joshrendek/manyforge"
MANIFEST_NAME = "sdk-manifest.json"
_SHA = re.compile(r"[0-9a-f]{40}\Z")
_DIGEST = re.compile(r"[0-9a-f]{64}\Z")
_NAME = re.compile(r"[A-Za-z0-9][A-Za-z0-9._+\-]*\Z")
KINDS = {
    "python": {"wheel", "sdist"}, "typescript": {"npm"}, "go": {"module-zip"},
    "java": {"jar", "pom", "sources", "javadoc", "central-bundle"},
    "contract": {"canonical-contract", "sdk-contract", "public-symbols"},
}


def _positive(value: object) -> bool:
    return isinstance(value, int) and not isinstance(value, bool) and value > 0


def _aware(value: str) -> datetime:
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None or parsed.utcoffset() is None:
        raise ValueError("release preparation time must be timezone-aware")
    return parsed


def validate_manifest(manifest: dict, directory: Path | None = None, tag: str | None = None,
                      source_sha: str | None = None, publishing: bool = False) -> None:
    """Fail before uploads on provenance, version, coordinate or byte mismatches."""
    if not isinstance(manifest, dict) or type(manifest.get("format")) is not int or manifest["format"] != 1:
        raise ValueError("unsupported SDK release manifest format")
    version = manifest.get("version", "")
    parse_release(version)
    expected = package_versions(version)
    if manifest.get("package_versions") != expected or manifest.get("go_module_version") != "v" + expected["go"]:
        raise ValueError("coordinated package-version mapping mismatch")
    if tag is not None and tag != "sdk-v" + version:
        raise ValueError("tag does not equal the canonical release manifest version")
    sha = manifest.get("source_sha", "")
    if not isinstance(sha, str) or not _SHA.fullmatch(sha):
        raise ValueError("release source SHA must be an immutable full commit ID")
    if source_sha is not None and source_sha != sha:
        raise ValueError("tag/check-out commit does not equal approved source_sha")
    if manifest.get("coordinates") != COORDINATES:
        raise ValueError("SDK package coordinates differ from the approved identities")
    if not _positive(manifest.get("release_pr")) or not _positive(manifest.get("staging_run_id")):
        raise ValueError("release PR and staging run IDs must be positive")
    if manifest.get("staging_artifact_name") != f"sdk-candidate-{version}-{sha}":
        raise ValueError("staging artifact does not identify the exact version/source candidate")
    _aware(manifest.get("prepared_at", ""))
    if not isinstance(manifest.get("rehearsal"), bool):
        raise ValueError("manifest must explicitly identify rehearsal versus publishable artifacts")
    if publishing and manifest["rehearsal"]:
        raise ValueError("rehearsal artifacts cannot be published")
    runs = manifest.get("verification_runs")
    if not isinstance(runs, list) or not runs:
        raise ValueError("release requires successful verification run provenance")
    for run in runs:
        if not isinstance(run, dict) or not _positive(run.get("id")) or run.get("source_sha") != sha or run.get("conclusion") != "success":
            raise ValueError("verification run does not prove the approved source revision")
    if not isinstance(manifest.get("tools"), dict) or not manifest["tools"]:
        raise ValueError("release tool versions must be recorded")
    digest = manifest.get("contract_sha256", "")
    if not isinstance(digest, str) or not _DIGEST.fullmatch(digest):
        raise ValueError("canonical contract checksum is invalid")
    module_sum = manifest.get("go_module_sum", "")
    try:
        if not isinstance(module_sum, str) or not module_sum.startswith("h1:") or len(base64.b64decode(module_sum[3:], validate=True)) != 32:
            raise ValueError("invalid Go module sum")
    except (ValueError, TypeError) as error:
        raise ValueError("canonical Go module h1 digest is invalid") from error
    records = manifest.get("artifacts")
    if not isinstance(records, list) or not records:
        raise ValueError("release contains no artifacts")
    names, pairs = set(), set()
    for record in records:
        if not isinstance(record, dict):
            raise ValueError("artifact records must be objects")
        if record.get("rehearsal", False) and not manifest["rehearsal"]:
            raise ValueError("rehearsal artifact cannot become a publishable candidate")
        name = record.get("filename", "")
        target, kind = record.get("target"), record.get("kind")
        if not isinstance(name, str) or not _NAME.fullmatch(name) or name in names:
            raise ValueError("artifact filenames must be unique safe basenames")
        names.add(name)
        if target not in KINDS or kind not in KINDS[target] or (target, kind) in pairs:
            raise ValueError("artifact target/kind is invalid or duplicated")
        pairs.add((target, kind))
        checksum, size = record.get("sha256", ""), record.get("size")
        if not isinstance(checksum, str) or not _DIGEST.fullmatch(checksum) or not isinstance(size, int) or isinstance(size, bool) or size < 0:
            raise ValueError("artifact digest/size metadata is invalid")
        if kind == "canonical-contract" and checksum != digest:
            raise ValueError("canonical contract artifact checksum differs from provenance")
        if directory is not None:
            artifact = directory / name
            if artifact.is_symlink() or not artifact.is_file() or artifact.stat().st_size != size or file_digest(artifact) != checksum:
                raise ValueError(f"retained artifact checksum mismatch: {name}")
            if kind == "central-bundle" and not manifest["rehearsal"]:
                with zipfile.ZipFile(artifact) as bundle:
                    if bundle.comment:
                        raise ValueError("marked rehearsal Central bundle cannot be published")
    required = {(target, kind) for target, kinds in KINDS.items() for kind in kinds}
    if manifest["rehearsal"] and not publishing:
        required.remove(("java", "central-bundle"))
    if not required.issubset(pairs):
        raise ValueError(f"release is missing required artifacts: {sorted(required - pairs)}")


def save_manifest(path: Path, manifest: dict) -> None:
    """Never overwrite an existing release manifest with different bytes."""
    validate_manifest(manifest)
    content = json_bytes(manifest)
    if path.exists():
        if path.is_symlink() or path.read_bytes() != content:
            raise ValueError("immutable release manifest checksum conflict")
        return
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("xb") as destination:
        destination.write(content)


def _copy(source: Path, destination: Path) -> None:
    if source.is_symlink() or not source.is_file():
        raise ValueError(f"release artifact must be a regular file: {source.name}")
    if destination.exists():
        if destination.is_symlink() or file_digest(source) != file_digest(destination):
            raise ValueError(f"immutable staged artifact checksum conflict: {destination.name}")
        return
    shutil.copyfile(source, destination)


def _kind(language: str, name: str) -> str:
    if language == "python":
        if name.endswith(".whl"): return "wheel"
        if name.endswith(".tar.gz"): return "sdist"
    if language == "typescript" and name.endswith(".tgz"): return "npm"
    if language == "go" and name.endswith(".zip"): return "module-zip"
    if language == "java":
        if name.endswith("-sources.jar"): return "sources"
        if name.endswith("-javadoc.jar"): return "javadoc"
        if name.endswith(".jar"): return "jar"
        if name.endswith(".pom"): return "pom"
        if name.endswith(".zip"): return "central-bundle"
    raise ValueError(f"unsupported staged package artifact: {language}/{name}")


def _tool_versions(root: Path) -> dict:
    versions = {"python": sys.version.split()[0], "pinned": json.loads((root / "tools/sdk/toolchain.json").read_text())}
    for name, command in {"go": ["go", "version"], "java": ["java", "-version"], "node": ["node", "--version"], "npm": ["npm", "--version"], "uv": ["uv", "--version"]}.items():
        result = subprocess.run(command, cwd=root, text=True, capture_output=True, check=True)
        versions[name] = (result.stdout or result.stderr).strip().splitlines()[0]
    return versions


def build_manifest(root: Path, artifacts_root: Path, destination: Path, source_sha: str,
                   release_pr: int, verification_runs: list[dict], staging_run_id: int,
                   prepared_at: datetime, rehearsal: bool = False) -> dict:
    """Stage tested bytes and contracts, never regenerate or rebuild them."""
    if prepared_at.tzinfo is None or prepared_at.utcoffset() is None:
        raise ValueError("manifest creation requires an explicit aware preparation time")
    current = subprocess.run(["git", "rev-parse", "HEAD"], cwd=root, check=True, text=True, capture_output=True).stdout.strip()
    if current != source_sha:
        raise ValueError("candidate checkout does not match manifest source_sha")
    if not rehearsal:
        dirty = subprocess.run(["git", "status", "--porcelain", "--untracked-files=all", "--", "api", "sdk", "tools/sdk"], cwd=root, check=True, text=True, capture_output=True).stdout
        if dirty:
            raise ValueError("publishable candidate has uncommitted SDK/contract/tooling inputs")
    version = (root / "sdk/version.txt").read_text().strip()
    versions = package_versions(version)
    destination.mkdir(parents=True, exist_ok=True)
    records = []
    go_sum = None
    for language in ("python", "typescript", "go", "java"):
        package_dir = artifacts_root / language
        metadata = json.loads((package_dir / "artifacts.json").read_text())
        if metadata.get("language") != language or metadata.get("version") != versions[language]:
            raise ValueError(f"tested {language} package version differs from candidate")
        if metadata.get("rehearsal", False) and not rehearsal:
            raise ValueError("rehearsal package catalog cannot stage a publishable release")
        if language == "go":
            module = metadata.get("module", {})
            if module.get("path") != COORDINATES["go"] or module.get("version") != "v" + versions["go"]:
                raise ValueError("tested Go module identity differs from candidate mapping")
            go_sum = module.get("sum")
        for artifact in metadata["artifacts"]:
            if artifact.get("rehearsal", False) and not rehearsal:
                raise ValueError("rehearsal package artifact cannot stage a publishable release")
            name = artifact["filename"]
            if not _NAME.fullmatch(name):
                raise ValueError("unsafe package artifact filename")
            source = package_dir / name
            if source.stat().st_size != artifact["size"] or file_digest(source) != artifact["sha256"]:
                raise ValueError(f"tested package artifact changed: {name}")
            _copy(source, destination / name)
            records.append({**artifact, "target": language, "kind": _kind(language, name)})
        # stage_java may leave the retained bundle beside metadata before it is appended.
        if language == "java" and not any(record["kind"] == "central-bundle" for record in records):
            bundle = package_dir / "central-bundle.zip"
            if bundle.exists():
                _copy(bundle, destination / bundle.name)
                records.append({"filename": bundle.name, "target": "java", "kind": "central-bundle", "sha256": file_digest(bundle), "size": bundle.stat().st_size})
    for relative, name, kind in (("api/openapi.yaml", "openapi.yaml", "canonical-contract"), ("api/sdk.openapi.json", "sdk.openapi.json", "sdk-contract"), ("sdk/public-symbols.json", "public-symbols.json", "public-symbols")):
        source = root / relative
        _copy(source, destination / name)
        records.append({"filename": name, "target": "contract", "kind": kind, "sha256": file_digest(source), "size": source.stat().st_size})
    manifest = {
        "format": 1, "version": version, "package_versions": versions, "go_module_version": "v" + versions["go"],
        "source_sha": source_sha, "contract_sha256": file_digest(root / "api/openapi.yaml"), "coordinates": COORDINATES.copy(),
        "release_pr": release_pr, "verification_runs": verification_runs, "tools": _tool_versions(root),
        "artifacts": sorted(records, key=lambda record: record["filename"]), "go_module_sum": go_sum,
        "staging_run_id": staging_run_id, "staging_artifact_name": f"sdk-candidate-{version}-{source_sha}",
        "prepared_at": prepared_at.astimezone(timezone.utc).isoformat().replace("+00:00", "Z"), "rehearsal": rehearsal,
    }
    validate_manifest(manifest, destination)
    save_manifest(destination / MANIFEST_NAME, manifest)
    return manifest
