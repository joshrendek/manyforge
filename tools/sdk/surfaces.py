"""Snapshot installed/compiled SDK APIs, independently of OpenAPI route spelling."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import zipfile

ROOT = Path(__file__).resolve().parents[2]
LANGUAGES = ("python", "typescript", "go", "java")


def _signature_accepts(old: dict, new: dict) -> bool:
    if {k: v for k, v in old.items() if k != "params"} != {k: v for k, v in new.items() if k != "params"}:
        return False
    before, after = old.get("params", []), new.get("params", [])
    positional_before = [p for p in before if p.get("kind") != "KEYWORD_ONLY"]
    positional_after = [p for p in after if p.get("kind") != "KEYWORD_ONLY"]
    if len(positional_after) < len(positional_before):
        return False
    pairs = list(zip(positional_before, positional_after))
    keywords_before = {p["name"]: p for p in before if p.get("kind") == "KEYWORD_ONLY"}
    keywords_after = {p["name"]: p for p in after if p.get("kind") == "KEYWORD_ONLY"}
    if not keywords_before.keys() <= keywords_after.keys():
        return False
    pairs.extend((p, keywords_after[name]) for name, p in keywords_before.items())
    for left, right in pairs:
        if {k: v for k, v in left.items() if k != "required"} != {k: v for k, v in right.items() if k != "required"}:
            return False
        if not left.get("required", True) and right.get("required", True):
            return False
    additions = positional_after[len(positional_before):] + [p for name, p in keywords_after.items() if name not in keywords_before]
    return all(not param.get("required", True) for param in additions)


def _compare_node(old: dict, new: dict, path: str, errors: list[str]) -> None:
    for key, value in old.items():
        if key not in new:
            errors.append(f"{path}: removed {key}")
        elif key == "members":
            for name, member in value.items():
                if name not in new[key]:
                    errors.append(f"{path}.{name}: removed public member")
                else:
                    _compare_node(member, new[key][name], f"{path}.{name}", errors)
            for name, member in new[key].items():
                if name not in value and member.get("required", False):
                    errors.append(f"{path}.{name}: added required member")
        elif key == "signatures":
            for signature in value:
                if not any(_signature_accepts(signature, candidate) for candidate in new[key]):
                    errors.append(f"{path}: incompatible signature {json.dumps(signature, sort_keys=True)}")
        elif key == "required":
            if not value and new[key]:
                errors.append(f"{path}: optional member became required")
        elif key in ("required_fields", "non_nullable_fields"):
            added = set(new[key]) - set(value)
            if key == "non_nullable_fields":
                added &= set(old.get("model_fields", old.get("required_fields", [])))
            if added:
                errors.append(f"{path}: added {key}: {', '.join(sorted(added))}")
        elif key == "model_fields":
            if not set(value) <= set(new[key]):
                errors.append(f"{path}: removed model fields")
        elif value != new[key]:
            errors.append(f"{path}: changed {key}")
    for key in new.keys() - old.keys():
        if key not in ("members", "signatures"):
            errors.append(f"{path}: added incompatible descriptor {key}")


def compare_surfaces(base: dict, revision: dict) -> list[str]:
    """Conservative source-compatibility check; malformed/missing evidence fails closed."""
    errors: list[str] = []
    for label, snapshot in (("base", base), ("revision", revision)):
        if not isinstance(snapshot, dict) or snapshot.get("format") != 1 or set(snapshot.get("languages", {})) != set(LANGUAGES):
            errors.append(f"{label}: invalid public surface snapshot (expected format 1 and four languages)")
            continue
        for language in LANGUAGES:
            value = snapshot["languages"][language]
            if not isinstance(value, dict) or not isinstance(value.get("symbols"), dict) or not value["symbols"]:
                errors.append(f"{label}.{language}: missing public symbols")
            elif any(not isinstance(node, dict) or not isinstance(node.get("kind"), str) for node in value["symbols"].values()):
                errors.append(f"{label}.{language}: invalid public symbol descriptor")
    if errors:
        return errors
    for language in LANGUAGES:
        before = base["languages"][language]["symbols"]
        after = revision["languages"][language]["symbols"]
        for name, node in before.items():
            if name not in after:
                errors.append(f"{language}.{name}: removed public symbol")
            else:
                _compare_node(node, after[name], f"{language}.{name}", errors)
    return errors


def run(args: list[str], cwd: Path, *, capture: bool = True) -> str:
    result = subprocess.run(args, cwd=cwd, env={**os.environ, "GOWORK": "off"}, check=True, text=True,
                            stdout=subprocess.PIPE if capture else None)
    return result.stdout or ""


def artifact(directory: Path, suffix: str, *, exclude: tuple[str, ...] = ()) -> Path:
    manifest = json.loads((directory / "artifacts.json").read_text())
    matches = []
    for record in manifest["artifacts"]:
        filename = record["filename"]
        if Path(filename).name != filename:
            raise ValueError("unsafe artifact filename")
        path = directory / filename
        if hashlib.sha256(path.read_bytes()).hexdigest() != record["sha256"]:
            raise ValueError(f"artifact digest mismatch: {path}")
        if filename.endswith(suffix) and not filename.endswith(exclude):
            matches.append(path)
    if len(matches) != 1:
        raise ValueError(f"expected exactly one {suffix} artifact in {directory}")
    return matches[0]


def extract(root: Path, artifacts: Path, temporary: Path) -> dict:
    helpers = ROOT / "tools/sdk/surface"
    result = {}
    python = temporary / "python"
    run([sys.executable, "-m", "venv", str(python)], root)
    executable = python / "bin/python"
    run(["uv", "pip", "install", "--python", str(executable), str(artifact(artifacts / "python", ".whl"))], root, capture=False)
    result["python"] = json.loads(run([str(executable), str(helpers / "python.py")], temporary))
    ts = temporary / "typescript"
    ts.mkdir()
    with tarfile.open(artifact(artifacts / "typescript", ".tgz")) as archive:
        archive.extractall(ts, filter="data")
    compiler = root / "sdk/typescript/node_modules/typescript/lib/typescript.js"
    if not compiler.is_file():
        raise RuntimeError("TypeScript compiler missing; run npm ci --ignore-scripts in sdk/typescript")
    (ts / "package/node_modules").symlink_to(root / "sdk/typescript/node_modules", target_is_directory=True)
    result["typescript"] = json.loads(run(["node", str(helpers / "typescript.cjs"), str(compiler), str(ts / "package")], root))
    go = temporary / "go"
    go.mkdir()
    with zipfile.ZipFile(artifact(artifacts / "go", ".zip")) as archive:
        archive.extractall(go)
    modules = list(go.rglob("go.mod"))
    if len(modules) != 1:
        raise ValueError("Go archive must contain exactly one module")
    result["go"] = json.loads(run(["go", "run", str(helpers / "go.go"), str(modules[0].parent)], root))
    from surface.java import extract_java
    result["java"] = extract_java(artifact(artifacts / "java", ".jar", exclude=("-sources.jar", "-javadoc.jar")))
    return {"format": 1, "languages": result}


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--write", action="store_true")
    mode.add_argument("--check", action="store_true")
    parser.add_argument("--built", action="store_true", help="consume sdk/dist artifacts without rebuilding SDKs")
    args = parser.parse_args()
    snapshot = ROOT / "sdk/public-symbols.json"
    if args.check and not snapshot.is_file():
        raise SystemExit("missing sdk/public-symbols.json; bootstrap explicitly with surfaces.py --write")
    with tempfile.TemporaryDirectory(prefix="manyforge-surfaces-") as directory:
        temporary = Path(directory)
        artifacts = ROOT / "sdk/dist"
        if not args.built:
            from pack import pack_language
            from version import package_versions
            artifacts = temporary / "artifacts"
            versions = package_versions((ROOT / "sdk/version.txt").read_text().strip())
            for language in LANGUAGES:
                pack_language(ROOT, language, artifacts / language, versions)
        actual = extract(ROOT, artifacts, temporary)
        invalid = compare_surfaces(actual, actual)
        if invalid:
            raise RuntimeError("; ".join(invalid))
        data = json.dumps(actual, indent=2, sort_keys=True, ensure_ascii=False) + "\n"
        if args.write:
            snapshot.write_text(data, encoding="utf-8")
            print("Wrote compiler-derived sdk/public-symbols.json")
        elif json.loads(snapshot.read_text()) != actual:
            raise SystemExit("compiled public surface differs from sdk/public-symbols.json; run surfaces.py --write and review compatibility")
        else:
            print("All four compiled public surfaces match sdk/public-symbols.json")


if __name__ == "__main__":
    main()
