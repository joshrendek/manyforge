"""Fail-closed wire, operation metadata and compiled SDK compatibility gates.

Copyright (c) 2026 ManyForge contributors. SPDX-License-Identifier: MIT
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import tarfile
import tempfile
import urllib.request

from contract import load, operations, SDK_AUDIENCES
from version import package_versions, parse_release

ROOT = Path(__file__).resolve().parents[2]
CONTRACT = "api/openapi.yaml"
SURFACE = "sdk/public-symbols.json"
METADATA = ("resource", "method", "audience", "business-param", "pagination", "signing")


def git(root: Path, *args: str) -> bytes:
    result = subprocess.run(["git", *args], cwd=root, capture_output=True, env={**os.environ, "GIT_TERMINAL_PROMPT": "0"})
    if result.returncode:
        raise RuntimeError(f"git {args[0]} failed: {result.stderr.decode(errors='replace').strip()}")
    return result.stdout


def validate_ref(ref: str) -> str:
    # Accept names and object IDs, never revision expressions, options or paths.
    if not isinstance(ref, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_./-]*", ref) or any(part in ref for part in ("..", "//", "/.", ".lock")) or ref.endswith(("/", ".")):
        raise ValueError(f"invalid git ref: {ref!r}")
    return ref


def commit(root: Path, ref: str) -> str:
    validate_ref(ref)
    value = git(root, "rev-parse", "--verify", "--end-of-options", ref + "^{commit}").decode().strip()
    if not re.fullmatch(r"[0-9a-f]{40}|[0-9a-f]{64}", value):
        raise RuntimeError("git returned an invalid commit ID")
    return value


def blob(root: Path, sha: str, path: str, *, optional: bool = False) -> bytes | None:
    # Distinguish a missing path from an inaccessible/corrupt git object.
    sha = commit(root, sha)
    paths = git(root, "ls-tree", "-z", "--name-only", sha, "--", path).split(b"\0")
    if path.encode() not in paths:
        if optional:
            return None
        raise RuntimeError(f"baseline {sha} is missing {path}")
    return git(root, "show", f"{sha}:{path}")


def release_tag_version(ref: str) -> str:
    validate_ref(ref)
    if not ref.startswith("sdk-v"):
        raise ValueError("released baseline must be an sdk-vYYYY.M.N tag")
    version = ref.removeprefix("sdk-v")
    parse_release(version)
    return version


def discover_release(root: Path, explicit: str | None = None) -> str | None:
    """Consult origin even with zero local tags; an unavailable origin is not bootstrap."""
    remote = git(root, "ls-remote", "--tags", "origin", "refs/tags/sdk-v*", "refs/tags/sdk/go/v*").decode()
    tags: dict[str, str] = {}
    go_tags = False
    for row in remote.splitlines():
        sha, ref = row.split()
        if ref.endswith("^{}"):
            continue
        name = ref.removeprefix("refs/tags/")
        if name.startswith("sdk/go/v"):
            go_tags = True
            continue
        release_tag_version(name)
        if not re.fullmatch(r"[0-9a-f]{40}|[0-9a-f]{64}", sha):
            raise RuntimeError("origin returned an invalid SDK tag object")
        tags[name] = sha
    local = git(root, "tag", "--list", "sdk-v*").decode().splitlines()
    for name in local:
        release_tag_version(name)
        if name not in tags:
            raise RuntimeError(f"local SDK tag {name} is missing from origin; reconcile release history")
    if explicit is not None:
        release_tag_version(explicit)
        if explicit not in tags:
            raise RuntimeError(f"released baseline {explicit} is missing from origin")
    if not tags:
        if go_tags:
            raise RuntimeError("published Go tags exist but the umbrella release baseline is missing")
        return None
    latest = max(tags, key=lambda name: parse_release(release_tag_version(name)))
    if explicit is not None and explicit != latest:
        raise RuntimeError(f"released baseline must be latest release {latest}, not {explicit}")
    # Fetch that exact immutable object; no branch-head substitution and no tag writes.
    git(root, "fetch", "--no-tags", "origin", f"refs/tags/{latest}")
    sha = commit(root, "FETCH_HEAD")
    remote_object = git(root, "rev-parse", "--verify", "FETCH_HEAD").decode().strip()
    if remote_object != tags[latest]:
        raise RuntimeError("SDK release tag changed during baseline discovery")
    if latest in local and commit(root, f"refs/tags/{latest}") != sha:
        raise RuntimeError("local and remote SDK release identities disagree")
    version = blob(root, sha, "sdk/version.txt").decode().strip()
    if version != release_tag_version(latest):
        raise RuntimeError("released tag identity does not match sdk/version.txt")
    validate_package_versions(root, sha, version)
    return sha


def validate_package_versions(root: Path, sha: str, version: str) -> None:
    versions = package_versions(version)
    import tomllib
    import xml.etree.ElementTree as ET
    python = tomllib.loads(blob(root, sha, "sdk/python/pyproject.toml").decode())["project"]["version"]
    typescript = json.loads(blob(root, sha, "sdk/typescript/package.json"))["version"]
    java_root = ET.fromstring(blob(root, sha, "sdk/java/pom.xml"))
    java = java_root.findtext("{http://maven.apache.org/POM/4.0.0}version")
    # Go's generated constant records its module version, distinct from release ID.
    go_source = blob(root, sha, "sdk/go/version.go").decode()
    match = re.search(r'\bVersion\s*=\s*"([^"]+)"', go_source)
    actual = {"python": python, "typescript": typescript, "java": java, "go": match.group(1) if match else None}
    if actual != versions:
        raise RuntimeError(f"released package versions do not match {version}: {actual}")


def metadata(document: dict) -> dict:
    result = {}
    for _, _, operation, _ in operations(document):
        name = operation["operationId"]
        if name in result:
            raise ValueError(f"duplicate operationId: {name}")
        result[name] = {key: operation.get("x-manyforge-" + key) for key in METADATA}
    return result


def compare_metadata(base: dict, revision: dict) -> list[str]:
    old, new = metadata(base), metadata(revision)
    problems = []
    for name, descriptor in old.items():
        if descriptor["audience"] not in SDK_AUDIENCES:
            continue
        if name not in new:
            problems.append(f"SDK operation removed or operationId renamed: {name}")
        elif descriptor != new[name]:
            changed = ", ".join(key for key in METADATA if descriptor[key] != new[name][key])
            problems.append(f"SDK operation {name} changed public metadata: {changed}")
    return problems


def oasdiff(root: Path) -> Path:
    pin = json.loads((root / "tools/sdk/toolchain.json").read_text())["oasdiff"]
    if pin["version"] != "1.31.0":
        raise RuntimeError("compatibility requires reviewed oasdiff 1.31.0")
    system = platform.system().lower()
    machine = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine(), platform.machine())
    target = "darwin_all" if system == "darwin" else f"{system}_{machine}"
    artifact = pin["artifacts"].get(target)
    if artifact is None:
        raise RuntimeError(f"unsupported oasdiff platform: {target}")
    cache = root / "tools/sdk/.cache"
    cache.mkdir(parents=True, exist_ok=True)
    archive = cache / f"oasdiff_{pin['version']}_{target}.tar.gz"
    if not archive.exists():
        descriptor, temporary = tempfile.mkstemp(dir=cache, suffix=".download")
        try:
            with os.fdopen(descriptor, "wb") as destination, urllib.request.urlopen(artifact["url"], timeout=60) as source:
                shutil.copyfileobj(source, destination)
            if hashlib.sha256(Path(temporary).read_bytes()).hexdigest() != artifact["sha256"]:
                raise RuntimeError("downloaded oasdiff checksum mismatch; artifact was not executed")
            os.replace(temporary, archive)
        finally:
            Path(temporary).unlink(missing_ok=True)
    if hashlib.sha256(archive.read_bytes()).hexdigest() != artifact["sha256"]:
        raise RuntimeError("cached oasdiff checksum mismatch; artifact was not executed")
    # Derive executable bytes from the verified archive on every invocation;
    # an independently tampered cached executable must never be trusted.
    with tarfile.open(archive, "r:gz") as contents:
        members = [member for member in contents.getmembers() if member.name == "oasdiff" and member.isfile()]
        if len(members) != 1:
            raise RuntimeError("oasdiff archive has no unique regular executable")
        executable = contents.extractfile(members[0]).read()
    binary = cache / f"oasdiff-{pin['version']}-{target}"
    if not binary.is_file() or binary.is_symlink() or binary.read_bytes() != executable:
        descriptor, temporary = tempfile.mkstemp(dir=cache, suffix=".executable")
        try:
            with os.fdopen(descriptor, "wb") as destination:
                destination.write(executable)
            os.chmod(temporary, 0o755)
            os.replace(temporary, binary)
        finally:
            Path(temporary).unlink(missing_ok=True)
    return binary


def check_baseline(root: Path, sha: str, binary: Path, *, bootstrap: bool) -> list[str]:
    canonical = blob(root, sha, CONTRACT, optional=True)
    if canonical is None:
        if bootstrap and blob(root, sha, "sdk/version.txt", optional=True) is None and blob(root, sha, SURFACE, optional=True) is None:
            print(f"Pre-SDK merge-base {sha}: no historical SDK contract to compare")
            return []
        raise RuntimeError(f"baseline {sha} is missing {CONTRACT}")
    from surfaces import compare_surfaces
    snapshot = json.loads(blob(root, sha, SURFACE))
    current_snapshot = json.loads((root / SURFACE).read_bytes())
    with tempfile.TemporaryDirectory(prefix="manyforge-compatibility-") as temporary:
        base_path = Path(temporary) / "base.yaml"
        base_path.write_bytes(canonical)
        base = load(base_path)
        revision = load(root / CONTRACT)
        problems = compare_metadata(base, revision) + compare_surfaces(snapshot, current_snapshot)
        result = subprocess.run([str(binary), "breaking", str(base_path), str(root / CONTRACT), "--fail-on", "WARN"], cwd=root, capture_output=True, text=True)
        if result.returncode:
            problems.append(f"wire compatibility failed against {sha}:\n{result.stdout}{result.stderr}")
        return problems


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-ref", required=True)
    parser.add_argument("--released-ref")
    args = parser.parse_args()
    try:
        base = commit(ROOT, args.base_ref)
        # Also works when CI already supplies the merge-base SHA.
        base = git(ROOT, "merge-base", base, "HEAD").decode().strip()
        released = discover_release(ROOT, args.released_ref)
        if released is None:
            manifest = blob(ROOT, base, ".release-please-manifest.json", optional=True)
            if manifest is not None and json.loads(manifest):
                raise RuntimeError("release manifest records a release but no released SDK baseline exists")
        binary = oasdiff(ROOT)
        problems = check_baseline(ROOT, base, binary, bootstrap=released is None)
        if released is not None and released != base:
            problems.extend(check_baseline(ROOT, released, binary, bootstrap=False))
        if problems:
            raise RuntimeError("SDK compatibility failed:\n" + "\n".join(problems))
        print("SDK wire, operation metadata and public-symbol compatibility passed" + (" (before first release)" if released is None else ""))
    except (OSError, ValueError, KeyError, RuntimeError, tarfile.TarError) as error:
        parser.exit(1, f"{error}\n")


if __name__ == "__main__":
    main()
