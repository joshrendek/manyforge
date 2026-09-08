"""Install a staged SDK outside source and exercise the actual application fixture."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import zipfile

from generate import ROOT
from pack import pack_language
from version import package_versions


def run(arguments: list[str], cwd: Path, environment: dict[str, str]) -> None:
    subprocess.run(arguments, cwd=cwd, env=environment, check=True)


def required(name: str) -> str:
    executable = shutil.which(name)
    if not executable:
        raise RuntimeError(f"Selected SDK smoke requires {name} on PATH")
    return executable


def install(language: str, artifacts: Path, consumer: Path, environment: dict[str, str]) -> list[str]:
    metadata = json.loads((artifacts / "artifacts.json").read_text())
    if metadata["language"] != language:
        raise RuntimeError("Staged artifact language mismatch")
    files = []
    for record in metadata["artifacts"]:
        path = artifacts / record["filename"]
        if path.parent != artifacts or hashlib.sha256(path.read_bytes()).hexdigest() != record["sha256"]:
            raise RuntimeError("Staged artifact digest/path mismatch")
        files.append(path)
    sources = ROOT / "tools/sdk/smoke" / language
    shutil.copytree(sources, consumer, dirs_exist_ok=True)
    (consumer / "browser").mkdir(exist_ok=True)
    if language == "python":
        run([sys.executable, "-m", "venv", str(consumer / "venv")], consumer, environment)
        python = str(consumer / "venv/bin/python")
        wheel, = [path for path in files if path.suffix == ".whl"]
        run([python, "-m", "pip", "install", "--disable-pip-version-check", "--constraint", str(consumer / "requirements.lock"), str(wheel)], consumer, environment)
        return [python, "consumer.py"]
    if language == "typescript":
        npm, node = required("npm"), required("node")
        archive, = [path for path in files if path.suffix == ".tgz"]
        run([npm, "ci", "--ignore-scripts"], consumer, environment)
        run([npm, "install", "--ignore-scripts", "--save-exact", str(archive)], consumer, environment)
        run([node, "node_modules/playwright/cli.js", "install", "chromium"], consumer, environment)
        # Both package export conditions must resolve from an installed package.
        run([node, "--input-type=module", "-e", "import('@manyforge/sdk').then(m=>{if(typeof m.ManyForge!=='function')throw Error('ESM export')})"], consumer, environment)
        run([node, "-e", "if(typeof require('@manyforge/sdk').ManyForge!=='function')throw Error('CJS export')"], consumer, environment)
        return [node, "consumer.mjs"]
    if language == "go":
        go = required("go")
        archive, = [path for path in files if path.suffix == ".zip"]
        module = "github.com/joshrendek/manyforge/sdk/go"
        version = "v" + metadata["version"]
        prefix = module + "@" + version + "/"
        extracted = consumer / "installed-sdk"
        with zipfile.ZipFile(archive) as zipped:
            for member in zipped.infolist():
                if not member.filename.startswith(prefix):
                    raise RuntimeError("Unexpected canonical Go module zip prefix")
                relative = Path(member.filename[len(prefix):])
                if relative.is_absolute() or ".." in relative.parts:
                    raise RuntimeError("Unsafe module zip member")
                target = extracted / relative
                if member.is_dir():
                    target.mkdir(parents=True, exist_ok=True)
                else:
                    target.parent.mkdir(parents=True, exist_ok=True)
                    target.write_bytes(zipped.read(member))
        (consumer / "go.mod").write_text(f"module manyforge-sdk-installed-smoke\n\ngo 1.25.0\n\nrequire {module} {version}\n\nreplace {module} => ./installed-sdk\n")
        run([go, "mod", "tidy"], consumer, environment)
        run([go, "build", "-o", "consumer", "."], consumer, environment)
        return [str(consumer / "consumer")]
    mvn, javac, java = required("mvn"), required("javac"), required("java")
    repository = consumer / "maven-repository"
    common = [mvn, "-B", f"-Dmaven.repo.local={repository}"]
    jar, = [path for path in files if path.suffix == ".jar" and not path.name.endswith(("-sources.jar", "-javadoc.jar"))]
    pom, = [path for path in files if path.suffix == ".pom"]
    run(common + ["org.apache.maven.plugins:maven-install-plugin:3.1.4:install-file", f"-Dfile={jar}", f"-DpomFile={pom}"], consumer, environment)
    (consumer / "pom.xml").write_text(f'''<project xmlns="http://maven.apache.org/POM/4.0.0"><modelVersion>4.0.0</modelVersion><groupId>smoke</groupId><artifactId>consumer</artifactId><version>1</version><dependencies><dependency><groupId>com.manyforge</groupId><artifactId>manyforge-sdk</artifactId><version>{metadata['version']}</version></dependency></dependencies></project>''')
    run(common + ["org.apache.maven.plugins:maven-dependency-plugin:3.8.1:build-classpath", "-Dmdep.outputFile=classpath.txt"], consumer, environment)
    classpath = (consumer / "classpath.txt").read_text().strip()
    run([javac, "--release", "17", "--add-modules", "jdk.httpserver", "-cp", classpath, "Consumer.java"], consumer, environment)
    return [java, "--add-modules", "jdk.httpserver", "-cp", os.pathsep.join([str(consumer), classpath]), "Consumer"]


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--language", required=True, choices=("python", "typescript", "go", "java"))
    parser.add_argument("--artifacts", type=Path, help="Existing sdk-pack output root; otherwise package fresh")
    parser.add_argument("--evidence-dir", type=Path, default=ROOT / "sdk/dist/smoke", help="Retain browser proof outside the disposable consumer workspace")
    args = parser.parse_args()
    required("go")
    required("docker")
    for executable in {"python": (), "typescript": ("node", "npm"), "go": ("go",), "java": ("java", "javac", "mvn")}[args.language]:
        required(executable)
    environment = os.environ.copy()
    # Consumer tools need their normal toolchain/cache settings, never source-tree import overrides.
    for key in ("PYTHONPATH", "PYTHONHOME", "NODE_PATH", "NODE_OPTIONS", "JAVA_TOOL_OPTIONS", "JDK_JAVA_OPTIONS", "_JAVA_OPTIONS", "GOFLAGS"):
        environment.pop(key, None)
    environment["GOWORK"] = "off"
    with tempfile.TemporaryDirectory(prefix="manyforge-sdk-smoke-") as temporary:
        stage = Path(temporary)
        artifacts = args.artifacts.resolve() / args.language if args.artifacts else stage / "artifacts" / args.language
        if not args.artifacts:
            versions = package_versions((ROOT / "sdk/version.txt").read_text().strip())
            pack_language(ROOT, args.language, artifacts, versions)
        consumer = stage / "consumer"
        consumer.mkdir()
        command = install(args.language, artifacts, consumer, environment)
        binary = stage / "manyforge"
        run([required("go"), "build", "-o", str(binary), "./cmd/manyforge"], ROOT, environment)
        environment.update({
            "MANYFORGE_SDK_SMOKE_BINARY": str(binary),
            "MANYFORGE_SDK_SMOKE_LANGUAGE": args.language,
            "MANYFORGE_SDK_SMOKE_CONSUMER_DIR": str(consumer),
            "MANYFORGE_SDK_SMOKE_CONSUMER_COMMAND": json.dumps(command),
        })
        completed = False
        try:
            run([required("go"), "test", "-tags", "integration sdk_smoke", "-count=1", "-timeout", "600s", "-run", "^TestSDKSmoke$", "./cmd/manyforge"], ROOT, environment)
            completed = True
        finally:
            evidence = args.evidence_dir.resolve() / args.language / stage.name
            evidence.mkdir(parents=True, exist_ok=True)
            retained = []
            for source in sorted((consumer / "browser").iterdir()):
                if source.is_file() and (source.suffix == ".png" or source.name == "network-proof.json"):
                    shutil.copyfile(source, evidence / source.name)
                    retained.append(source.name)
            (evidence / "result.json").write_text(json.dumps({"language": args.language, "completed": completed, "files": retained}, indent=2) + "\n")
            print(f"SDK smoke evidence: {evidence}")


if __name__ == "__main__":
    main()
