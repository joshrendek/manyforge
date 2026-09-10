"""Build local installable artifacts; never upload or create release tags."""
from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

from contract import json_bytes
from generate import LANGUAGES, ROOT, file_digest
from version import package_versions


def run(arguments: list[str], cwd: Path, *, capture: bool = False, extra_env: dict[str, str] | None = None) -> str:
    environment = os.environ.copy()
    environment.update(extra_env or {})
    result = subprocess.run(arguments, cwd=cwd, env=environment, check=True, text=True, stdout=subprocess.PIPE if capture else None)
    return result.stdout or ""


def pack_language(root: Path, language: str, destination: Path, versions: dict[str, str]) -> dict:
    source = root / "sdk" / language
    destination.mkdir(parents=True, exist_ok=True)
    artifacts = []
    metadata = {}
    with tempfile.TemporaryDirectory(prefix=f"manyforge-pack-{language}-") as temporary:
        stage = Path(temporary)
        if language == "python":
            run([sys.executable, "-m", "build", "--no-isolation", str(source), "--outdir", str(stage)], root)
            artifacts = sorted(stage.glob("*.whl")) + sorted(stage.glob("*.tar.gz"))
            if len(artifacts) != 2:
                raise RuntimeError("Python packaging must produce one wheel and one sdist")
        elif language == "typescript":
            run(["npm", "ci", "--ignore-scripts"], source)
            run(["npm", "run", "build"], source)
            packed = json.loads(run(["npm", "pack", "--json", "--pack-destination", str(stage)], source, capture=True))
            if len(packed) != 1:
                raise RuntimeError("npm packaging must produce exactly one tarball")
            artifacts = [stage / packed[0]["filename"]]
        elif language == "go":
            run(["go", "test", "./..."], source, extra_env={"GOWORK": "off"})
            module_version = "v" + versions["go"]
            archive = stage / f"manyforge-go-{module_version}.zip"
            metadata = json.loads(run([
                "go", "run", ".", "-dir", str(source), "-version", module_version, "-output", str(archive),
            ], root / "tools/sdk/modulezip", capture=True, extra_env={"GOWORK": "off"}))
            artifacts = [archive]
        elif language == "java":
            run(["mvn", "-B", "-f", str(source / "pom.xml"), "verify"], root)
            stem = f"manyforge-sdk-{versions['java']}"
            for suffix in (".jar", "-sources.jar", "-javadoc.jar"):
                built = source / "target" / (stem + suffix)
                if not built.is_file():
                    raise RuntimeError(f"Maven did not produce required artifact: {built.name}")
                shutil.copyfile(built, stage / built.name)
            shutil.copyfile(source / "pom.xml", stage / (stem + ".pom"))
            artifacts = sorted(stage.iterdir())
        else:
            raise ValueError(f"unsupported SDK language: {language}")
        records = []
        for artifact in artifacts:
            target = destination / artifact.name
            shutil.copyfile(artifact, target)
            records.append({"filename": target.name, "sha256": file_digest(target), "size": target.stat().st_size})
    result = {"language": language, "version": versions[language], "artifacts": records}
    if language == "go":
        result["module"] = metadata
    (destination / "artifacts.json").write_bytes(json_bytes(result))
    return result


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--language", choices=LANGUAGES)
    parser.add_argument("--output", type=Path, default=ROOT / "sdk/dist")
    args = parser.parse_args()
    release = (ROOT / "sdk/version.txt").read_text(encoding="utf-8").strip()
    versions = package_versions(release)
    selected = (args.language,) if args.language else LANGUAGES
    records = [pack_language(ROOT, language, args.output.resolve() / language, versions) for language in selected]
    catalog = {"version": release, "package_versions": versions, "packages": records}
    filename = "artifacts.json" if args.language is None else f"artifacts-{args.language}.json"
    (args.output / filename).write_bytes(json_bytes(catalog))
    print(f"Staged {', '.join(selected)} artifacts for {release}; no registry writes")


if __name__ == "__main__":
    main()
