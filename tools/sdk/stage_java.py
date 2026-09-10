"""Prepare a signed Central bundle from the exact Java artifacts tested by sdk-pack.

No deploy lifecycle or registry upload is invoked. Settings and signing keyrings
are private, temporary, and removed even when Maven or signature verification fails.

Central plugin 0.11.0's PublishMojo filters all artifacts out when skipPublishing
is true, despite the Portal documentation promising a bundle. Its nonempty-bundle
path uploads unconditionally. The approved staging mechanism signs the retained
tested files and assembles Central's documented ZIP layout locally. That is not
a call to the plugin's publish/deploy Mojo, and staging never uploads.
Evidence: org/sonatype/central/publisher/plugin/PublishMojo.java in
https://repo.maven.apache.org/maven2/org/sonatype/central/
central-publishing-maven-plugin/0.11.0/central-publishing-maven-plugin-0.11.0-sources.jar

Copyright (c) 2026 ManyForge contributors. SPDX-License-Identifier: MIT
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import xml.etree.ElementTree as ET
import zipfile

from contract import json_bytes
from generate import ROOT, file_digest
from version import package_versions

REHEARSAL_MARKER = "ManyForge SDK REHEARSAL ONLY"
CHECKSUMS = ("md5", "sha1", "sha256", "sha512")
NS = {"m": "http://maven.apache.org/POM/4.0.0"}


def _pom_metadata(path: Path, version: str) -> None:
    pom = ET.parse(path).getroot()
    expected = {
        "groupId": "com.manyforge",
        "artifactId": "manyforge-sdk",
        "version": version,
        "url": "https://github.com/joshrendek/manyforge",
        "licenses/license/name": "MIT License",
        "licenses/license/url": "https://opensource.org/license/mit",
        "developers/developer/id": "manyforge",
        "developers/developer/name": "ManyForge contributors",
        "developers/developer/url": "https://manyforge.com",
        "scm/connection": "scm:git:https://github.com/joshrendek/manyforge.git",
        "scm/developerConnection": "scm:git:ssh://git@github.com/joshrendek/manyforge.git",
        "scm/url": "https://github.com/joshrendek/manyforge",
    }
    for key, value in expected.items():
        found = pom.findtext("/".join("m:" + part for part in key.split("/")), namespaces=NS)
        if found != value:
            raise ValueError(f"Java release POM requires {key}={value}")
    for key in ("name", "description"):
        if not (pom.findtext("m:" + key, namespaces=NS) or "").strip():
            raise ValueError(f"Java release POM requires {key}")
    profile = pom.find("m:profiles/m:profile[m:id='sdk-release']", NS)
    if profile is None:
        raise ValueError("Java release POM lacks sdk-release profile; regenerate and retest packages")
    if profile.find("m:build/m:plugins/m:plugin[m:artifactId='central-publishing-maven-plugin']", NS) is not None:
        raise ValueError("Offline Java staging must not activate Central's publishing plugin")
    signer = profile.find("m:build/m:plugins/m:plugin[m:artifactId='maven-gpg-plugin']", NS)
    if signer is None or signer.findtext("m:version", namespaces=NS) != "3.2.8":
        raise ValueError("Java release POM must pin the GPG signing plugin")
    timestamp = pom.findtext("m:build/m:plugins/m:plugin[m:artifactId='maven-javadoc-plugin']/m:configuration/m:notimestamp", namespaces=NS)
    if timestamp != "true":
        raise ValueError("Java Javadoc must use notimestamp=true; regenerate and retest packages")


def _run(arguments: list[str], cwd: Path, environment: dict[str, str], *, input_bytes: bytes | None = None) -> bytes:
    result = subprocess.run(arguments, cwd=cwd, env=environment, input=input_bytes, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if result.returncode:
        # Never echo tool output: Maven/GPG may include key UIDs or secret settings.
        raise RuntimeError(f"Java staging command {Path(arguments[0]).name} failed (exit {result.returncode}); no bundle was replaced or uploaded")
    return result.stdout


def _verify_signature(signature: Path, artifact: Path, home: Path, environment: dict[str, str]) -> None:
    status = _run(["gpg", "--batch", "--no-auto-key-retrieve", "--homedir", str(home), "--status-fd", "1", "--verify", str(signature), str(artifact)], home.parent, environment).decode()
    valid = [line.split() for line in status.splitlines() if line.startswith("[GNUPG:] VALIDSIG ")]
    if len(valid) != 1 or valid[0][-1] != environment["MAVEN_GPG_KEY_FINGERPRINT"]:
        raise ValueError(f"Java signature does not belong to the selected signing key: {artifact.name}")


def _descriptor(path: Path, rehearsal: bool) -> dict:
    return {"filename": path.name, "target": "java", "kind": "central-bundle", "sha256": file_digest(path), "size": path.stat().st_size, "rehearsal": rehearsal}


def _catalog_bundle(path: Path, catalog: dict, output: Path, rehearsal: bool) -> dict:
    descriptor = _descriptor(output, rehearsal)
    catalog["artifacts"].append(descriptor)
    catalog["rehearsal"] = rehearsal
    with tempfile.NamedTemporaryFile(dir=path.parent, prefix=".java-catalog-", delete=False) as handle:
        replacement = Path(handle.name)
        handle.write(json_bytes(catalog))
        handle.flush()
        os.fsync(handle.fileno())
    try:
        os.replace(replacement, path)
    finally:
        replacement.unlink(missing_ok=True)
    return descriptor


def _verify_bundle(bundle: Path, tested: dict[str, Path], version: str, rehearsal: bool, *, home: Path | None = None, environment: dict[str, str] | None = None) -> None:
    prefix = f"com/manyforge/manyforge-sdk/{version}/"
    with zipfile.ZipFile(bundle) as archive:
        if archive.comment != (REHEARSAL_MARKER.encode() if rehearsal else b""):
            raise ValueError("Central bundle rehearsal marking does not match staging mode")
        names = [entry.filename for entry in archive.infolist() if not entry.is_dir()]
        expected = {prefix + name + suffix for name in tested for suffix in ("", ".asc", *("." + alg for alg in CHECKSUMS))}
        # Central does not require checksums of signatures, but accepts them.
        optional = {prefix + name + ".asc." + alg for name in tested for alg in CHECKSUMS}
        if len(names) != len(set(names)) or not expected.issubset(names) or set(names) - expected - optional:
            raise ValueError("Central bundle has missing, duplicate, or unexpected artifacts")
        for name, path in tested.items():
            payload = archive.read(prefix + name)
            if hashlib.sha256(payload).hexdigest() != file_digest(path) or len(payload) != path.stat().st_size:
                raise ValueError(f"Central bundle differs from tested artifact: {name}")
            for suffix in ("", ".asc"):
                content = archive.read(prefix + name + suffix)
                for algorithm in CHECKSUMS:
                    checksum_name = prefix + name + suffix + "." + algorithm
                    if suffix and checksum_name not in names:
                        continue
                    if archive.read(checksum_name).decode("ascii").strip() != hashlib.new(algorithm, content).hexdigest():
                        raise ValueError(f"Central bundle checksum mismatch: {name}{suffix}.{algorithm}")
            if home is not None and environment is not None:
                signature = home.parent / (name + ".asc")
                signature.write_bytes(archive.read(prefix + name + ".asc"))
                _verify_signature(signature, path, home, environment)


def stage_java(root: Path, artifacts: Path, output: Path) -> dict:
    """Return and catalog a central-bundle descriptor, never replacing retained bytes.

    Real staging requires SDK_RELEASE_READY=true and the two MAVEN_GPG_* signing
    secrets. Explicit SDK_RELEASE_REHEARSAL=true permits only a key whose UID is
    marked 'ManyForge SDK REHEARSAL ONLY'; both ZIP and catalog carry that marker.
    The manifest builder must reject rehearsal records for a real release.
    """
    root, artifacts, output = root.resolve(), artifacts.resolve(), output.absolute()
    version = package_versions((root / "sdk/version.txt").read_text().strip())["java"]
    catalog_path = artifacts / "artifacts.json"
    catalog = json.loads(catalog_path.read_text())
    if catalog.get("language") != "java" or catalog.get("version") != version:
        raise ValueError("Java sdk-pack catalog does not match the candidate version")
    stem = f"manyforge-sdk-{version}"
    tested = {stem + suffix: artifacts / (stem + suffix) for suffix in (".jar", ".pom", "-sources.jar", "-javadoc.jar")}
    records = catalog.get("artifacts", [])
    by_name = {record["filename"]: record for record in records}
    if len(by_name) != len(records):
        raise ValueError("Java artifact catalog contains duplicate names")
    for name, path in tested.items():
        record = by_name.get(name)
        if path.is_symlink() or not path.is_file() or record is None or record.get("sha256") != file_digest(path) or record.get("size") != path.stat().st_size:
            raise ValueError(f"Tested Java artifact missing or checksum mismatch: {name}")
    license_bytes = (root / "sdk/java/LICENSE").read_bytes()
    for suffix, license_path in ((".jar", "META-INF/LICENSE"), ("-sources.jar", "META-INF/LICENSE"), ("-javadoc.jar", "resources/LICENSE")):
        with zipfile.ZipFile(tested[stem + suffix]) as archive:
            if license_path not in archive.namelist() or archive.read(license_path) != license_bytes:
                raise ValueError(f"Tested Java artifact lacks the SDK MIT license: {stem}{suffix}")
    _pom_metadata(tested[stem + ".pom"], version)
    source = root / "sdk/java"
    if file_digest(source / "pom.xml") != file_digest(tested[stem + ".pom"]):
        raise ValueError("Candidate Java POM differs from tested POM; regenerate and retest before staging")
    rehearsal = os.environ.get("SDK_RELEASE_REHEARSAL") == "true"
    if catalog.get("rehearsal", False) != rehearsal and any(record.get("kind") == "central-bundle" for record in records):
        raise ValueError("Cannot change rehearsal mode of a retained Java candidate")
    if output.is_symlink() or output.name in tested or output == catalog_path:
        raise ValueError("Central output must not replace tested artifacts or their catalog")
    if output.exists() and output.name in by_name:
        record = by_name[output.name]
        if record != _descriptor(output, rehearsal):
            raise ValueError("Existing Central bundle differs from its immutable catalog record; refusing to overwrite")
        _verify_bundle(output, tested, version, rehearsal)
        return record
    if any(record.get("kind") == "central-bundle" for record in records):
        raise ValueError("Central bundle is already reserved; restore its retained exact bytes instead of rebuilding")
    required = [name for name in ("MAVEN_GPG_PRIVATE_KEY", "MAVEN_GPG_PASSPHRASE") if not os.environ.get(name)]
    if not rehearsal and os.environ.get("SDK_RELEASE_READY") != "true":
        required.append("SDK_RELEASE_READY=true (verified namespace/trusted-publisher maintainer attestation)")
    if required:
        raise RuntimeError("Java staging blocked: missing " + ", ".join(required))
    for binary in ("mvn", "gpg", "gpgconf"):
        if shutil.which(binary) is None:
            raise RuntimeError(f"Java staging blocked: required executable {binary} is unavailable")
    # GPG Unix sockets exceed Darwin's path limit under its long default TMPDIR.
    temporary_root = "/tmp" if Path("/tmp").is_dir() else None
    with tempfile.TemporaryDirectory(prefix="mf-java-", dir=temporary_root) as temporary:
        work = Path(temporary)
        home = work / "gnupg"
        home.mkdir(mode=0o700)
        environment = {key: value for key, value in os.environ.items() if key in ("PATH", "HOME", "JAVA_HOME", "LANG", "LC_ALL", "TMPDIR", "SYSTEMROOT")}
        environment.update({"GNUPGHOME": str(home), "MAVEN_GPG_PASSPHRASE": os.environ["MAVEN_GPG_PASSPHRASE"], "MAVEN_SKIP_RC": "true"})
        settings = work / "settings.xml"
        settings.write_text('<settings xmlns="http://maven.apache.org/SETTINGS/1.2.0"><interactiveMode>false</interactiveMode><servers/></settings>\n')
        settings.chmod(0o600)
        try:
            _run(["gpg", "--batch", "--homedir", str(home), "--import"], work, environment, input_bytes=os.environ["MAVEN_GPG_PRIVATE_KEY"].encode())
            listing = _run(["gpg", "--batch", "--homedir", str(home), "--with-colons", "--list-secret-keys"], work, environment).decode()
            lines = [line.split(":") for line in listing.splitlines()]
            primary = [line for line in lines if line[0] == "sec"]
            if len(primary) != 1:
                raise ValueError("Java signing key must contain exactly one secret primary key")
            marked = any(line[0] == "uid" and REHEARSAL_MARKER in line[9] for line in lines)
            if marked != rehearsal:
                raise ValueError("Rehearsal requires a marked test-key UID; marked test keys are forbidden for real staging")
            fingerprint = next(line[9] for line in lines if line[0] == "fpr")
            environment["MAVEN_GPG_KEY_FINGERPRINT"] = fingerprint
            if output.exists():
                # Recover cancellation between atomic bundle retention and catalog
                # update without rebuilding, re-signing, or replacing those bytes.
                _verify_bundle(output, tested, version, rehearsal, home=home, environment=environment)
                return _catalog_bundle(catalog_path, catalog, output, rehearsal)
            project = work / "java"
            shutil.copytree(source, project, ignore=shutil.ignore_patterns("target", ".mvn"))
            _run(["mvn", "-B", "--no-transfer-progress", "--settings", str(settings), "--global-settings", str(settings), "-f", str(project / "pom.xml"), "-Psdk-release", "verify"], work, environment)
            signatures = {}
            for name, path in tested.items():
                rebuilt = project / "pom.xml" if name.endswith(".pom") else project / "target" / name
                if not rebuilt.is_file() or file_digest(rebuilt) != file_digest(path) or rebuilt.stat().st_size != path.stat().st_size:
                    raise ValueError(f"Java release rebuild differs from tested artifact: {name}; no bundle may be staged")
                # The pinned signer keeps signatures beside artifacts already in target/.
                signature = project / "target" / (name + ".asc")
                if not signature.is_file():
                    raise ValueError(f"Maven did not sign required Java artifact: {name}")
                _verify_signature(signature, path, home, environment)
                signatures[name] = signature
            bundle = work / "central-bundle.zip"
            _bundle(tested, signatures, version, bundle, rehearsal)
            _verify_bundle(bundle, tested, version, rehearsal, home=home, environment=environment)
            output.parent.mkdir(parents=True, exist_ok=True)
            # Atomically install without replacement. A cancellation cannot leave
            # a partial bundle at the immutable output path.
            with tempfile.NamedTemporaryFile(dir=output.parent, prefix=".central-bundle-", delete=False) as handle:
                prepared_path = Path(handle.name)
                with bundle.open("rb") as prepared:
                    shutil.copyfileobj(prepared, handle)
                handle.flush()
                os.fsync(handle.fileno())
            try:
                os.link(prepared_path, output)
            finally:
                prepared_path.unlink(missing_ok=True)
            return _catalog_bundle(catalog_path, catalog, output, rehearsal)
        finally:
            # Only the agent bound to our newly created keyring may be stopped.
            subprocess.run(["gpgconf", "--homedir", str(home), "--kill", "gpg-agent"], env=environment, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)


def _bundle(tested: dict[str, Path], signatures: dict[str, Path], version: str, output: Path, rehearsal: bool) -> None:
    """Assemble a real Central-format bundle, not the broken plugin publish goal."""
    with zipfile.ZipFile(output, "x", compression=zipfile.ZIP_DEFLATED) as archive:
        archive.comment = REHEARSAL_MARKER.encode() if rehearsal else b""
        for name in sorted(tested):
            for suffix, source in (("", tested[name]), (".asc", signatures[name])):
                content = source.read_bytes()
                path = f"com/manyforge/manyforge-sdk/{version}/{name}{suffix}"
                entries = {path: content, **{path + "." + algorithm: hashlib.new(algorithm, content).hexdigest().encode("ascii") for algorithm in CHECKSUMS}}
                for entry, data in entries.items():
                    info = zipfile.ZipInfo(entry, (1980, 1, 1, 0, 0, 2))
                    info.compress_type = zipfile.ZIP_DEFLATED
                    info.external_attr = 0o100644 << 16
                    archive.writestr(info, data)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--artifacts", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    try:
        print(json.dumps(stage_java(ROOT, args.artifacts, args.output), sort_keys=True))
    except (OSError, ValueError, RuntimeError, KeyError, zipfile.BadZipFile) as error:
        parser.exit(1, f"Java staging blocked: {error}\n")


if __name__ == "__main__":
    main()
