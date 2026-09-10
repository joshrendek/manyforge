"""Reconcile immutable SDK candidates; stage exact bytes before creating any tag.

Copyright (c) 2026 ManyForge contributors. SPDX-License-Identifier: MIT

Fixture state uses the same JSON shape as acquire_state. dry-run accepts an
explicit aware clock and staged sdk-pack inputs; it never invokes write APIs.
Real invocations require namespace/publisher attestation and a repository App.
"""
from __future__ import annotations

import argparse
import hashlib
import base64
from datetime import datetime, timezone
import io
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import urllib.error
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET
import zipfile

from generate import ROOT, file_digest
from release_manifest import build_manifest, save_manifest, validate_manifest
from release_signal import RELEASE_BRANCH, REPOSITORY, SIGNAL, configured_app_identity, validate_release_pr
from version import canonical_reservation, next_release, package_versions, parse_release

BOOTSTRAP_SHA = "34e6ea857ba5e2f0f86620bf76fb247ee8b4bcb8"
MANIFEST = "sdk-manifest.json"
SUPERSESSION = "manyforge-sdk-supersession: "


def clock(value: str) -> datetime:
    now = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if now.tzinfo is None or now.utcoffset() is None:
        raise ValueError("--now requires an explicit timezone-aware UTC clock")
    return now.astimezone(timezone.utc)


def identity(value: dict) -> tuple[str, str]:
    version, sha = value["version"], value["source_sha"]
    parse_release(version)
    if not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise ValueError("candidate source must be an exact commit SHA")
    return version, sha


def artifact_name(candidate: dict) -> str:
    version, sha = identity(candidate)
    return f"sdk-candidate-{version}-{sha}"


def reconcile(state: dict, now: datetime) -> dict:
    """Pure oldest-first reconciliation; state acquisition performs trust checks."""
    candidates: dict[str, dict] = {}
    reserved = list(state.get("registry_versions", []))
    for tag in state.get("tags", []):
        name = tag["name"]
        if name.startswith("sdk-v"):
            version = name.removeprefix("sdk-v")
        elif name.startswith("sdk/go/v"):
            version = canonical_reservation(name.removeprefix("sdk/go/"))
        else:
            continue
        parse_release(version)
        reserved.append(version)
    for candidate in state.get("merged", []) + state.get("drafts", []):
        version, sha = identity(candidate)
        reserved.append(version)
        previous = candidates.get(version)
        if previous and identity(previous) != (version, sha):
            raise ValueError(f"ownership conflict: {version} names different reviewed source SHAs")
        if previous and previous["release_pr"] != candidate["release_pr"]:
            raise ValueError(f"ownership conflict: {version} names different release PRs")
        candidates[version] = {**(previous or {}), **candidate}
    # Even resuming an older merged version must reject a regressed wall clock.
    next_release(now, reserved, None)
    superseded = {}
    for marker in state.get("supersessions", []):
        old, replacement = marker["old"], marker["replacement"]
        if identity(candidates[old["version"]]) != identity(old):
            raise ValueError("supersession does not match original immutable candidate")
        if identity(candidates[replacement["version"]]) != identity(replacement):
            raise ValueError("supersession replacement is not a reviewed merged candidate")
        if parse_release(replacement["version"]) <= parse_release(old["version"]) or not marker["reason"].strip():
            raise ValueError("supersession needs a newer version and explicit reason")
        if old["version"] in superseded and superseded[old["version"]] != marker:
            raise ValueError("conflicting supersession markers")
        superseded[old["version"]] = marker
    pending = sorted((c for c in candidates.values() if not c.get("complete") and c["version"] not in superseded),
                     key=lambda c: (c["merged_at"], parse_release(c["version"])))
    if pending:
        candidate = pending[0]
        for tag in state.get("tags", []):
            if tag["name"] in ("sdk-v" + candidate["version"], "sdk/go/v" + package_versions(candidate["version"])["go"]):
                if tag["sha"] != candidate["source_sha"] or tag.get("type", "commit") != "commit":
                    raise ValueError("existing release tag disagrees with immutable candidate")
        return {"action": "resume", "candidate": candidate, "package_versions": package_versions(candidate["version"])}
    opened = state.get("open", [])
    if len(opened) > 1:
        raise ValueError("multiple open coordinated SDK release PRs")
    if not state.get("sdk_changes") and not opened:
        return {"action": "idle", "reason": "No SDK changes waiting; calendar rollover does not create a release"}
    version = next_release(now, reserved, opened[0]["version"] if opened else None)
    return {"action": "release-pr", "version": version, "package_versions": package_versions(version)}


class GitHub:
    def api(self, path: str, *, method: str = "GET", data: dict | None = None, binary: bool = False, missing: bool = False):
        args = ["gh", "api", path, "--method", method]
        if binary:
            args += ["-H", "Accept: application/octet-stream"]
        if data is not None:
            args += ["--input", "-"]
        result = subprocess.run(args, input=json.dumps(data).encode() if data is not None else None,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        if result.returncode:
            if missing and b"HTTP 404" in result.stderr:
                return None
            raise RuntimeError(f"GitHub {method} {path.split('?')[0]} failed: " + result.stderr.decode())
        return result.stdout if binary else json.loads(result.stdout) if result.stdout.strip() else None

    def pages(self, path: str, key: str | None = None) -> list:
        rows = []
        for page in range(1, 10001):
            value = self.api(path + ("&" if "?" in path else "?") + f"per_page=100&page={page}")
            batch = value[key] if key else value
            rows.extend(batch)
            if len(batch) < 100:
                return rows
        raise RuntimeError("GitHub pagination exceeded bound; refusing partial release state")

    def file(self, path: str, ref: str) -> bytes:
        value = self.api(f"repos/{REPOSITORY}/contents/{path}?ref={urllib.parse.quote(ref, safe='')}")
        return base64.b64decode(value["content"], validate=False)

    def asset(self, asset: dict) -> bytes:
        return self.api(f"repos/{REPOSITORY}/releases/assets/{asset['id']}", binary=True)


def public_bytes(url: str) -> bytes | None:
    request = urllib.request.Request(url, headers={"User-Agent": "manyforge-sdk-release", "Accept": "application/json, application/xml, text/plain"})
    try:
        with urllib.request.urlopen(request, timeout=60) as response:
            return response.read()
    except urllib.error.HTTPError as error:
        if error.code == 404:
            return None
        raise RuntimeError(f"public registry lookup failed ({error.code}): {url}") from None


def registry_versions() -> list[str]:
    versions = []
    for url, key in (("https://pypi.org/pypi/manyforge/json", "releases"),
                     ("https://registry.npmjs.org/@manyforge%2Fsdk", "versions")):
        raw = public_bytes(url)
        if raw is not None:
            versions.extend(json.loads(raw)[key])
    raw = public_bytes("https://proxy.golang.org/github.com/joshrendek/manyforge/sdk/go/@v/list")
    if raw is not None:
        versions.extend(raw.decode().splitlines())
    raw = public_bytes("https://repo.maven.apache.org/maven2/com/manyforge/manyforge-sdk/maven-metadata.xml")
    if raw is not None:
        versions.extend(node.text for node in ET.fromstring(raw).findall("./versioning/versions/version"))
    # Unexpected preexisting versions are an ownership conflict, not an empty namespace.
    return sorted({canonical_reservation(value) for value in versions}, key=parse_release)


def pr_candidate(gh: GitHub, pr: dict, bot: tuple[str, int], *, historical: bool = False) -> dict:
    ref = pr["merge_commit_sha"] if pr.get("merged_at") else pr["head"]["sha"]
    version = gh.file("sdk/version.txt", ref).decode().strip()
    checked = dict(pr)
    if historical:
        checked["labels"] = [*pr["labels"], {"name": "autorelease: pending"}]
    validate_release_pr(checked, version, bot)
    manifest = json.loads(gh.file(".release-please-manifest.json", ref))
    if manifest != {"sdk": version}:
        raise ValueError("Release Please manifest and version.txt differ")
    result = {"version": version, "source_sha": ref, "release_pr": pr["number"], "merged_at": pr.get("merged_at")}
    if pr.get("merged_at"):
        reviews = gh.pages(f"repos/{REPOSITORY}/pulls/{pr['number']}/reviews")
        latest = {}
        for review in reviews:
            if review["state"] in ("APPROVED", "CHANGES_REQUESTED", "DISMISSED"):
                latest[review["user"]["id"]] = review
        if not any(r["state"] == "APPROVED" and r["commit_id"] == pr["head"]["sha"] and r["user"]["type"] == "User" for r in latest.values()):
            raise ValueError(f"release PR {pr['number']} has no human approval of its final head")
        if any(r["state"] == "CHANGES_REQUESTED" for r in latest.values()):
            raise ValueError("release PR has an unresolved change request")
    identity(result)
    return result


def exact_ci(gh: GitHub, sha: str) -> list[dict]:
    runs = gh.pages(f"repos/{REPOSITORY}/actions/workflows/ci.yml/runs?event=push&branch=master&head_sha={sha}", "workflow_runs")
    runs = [r for r in runs if r["head_sha"] == sha and r["event"] == "push" and r["head_branch"] == "master"
            and r["head_repository"]["full_name"] == REPOSITORY]
    if not runs:
        raise ValueError(f"waiting for exact master-push CI at reviewed candidate {sha}")
    latest = max(runs, key=lambda r: (r["run_number"], r.get("run_attempt", 1)))
    if latest["status"] != "completed" or latest["conclusion"] != "success":
        raise ValueError(f"exact candidate CI is not successful: run {latest['id']}")
    return [{"id": latest["id"], "source_sha": sha, "conclusion": "success"}]


def acquire_state(gh: GitHub, bot: tuple[str, int]) -> dict:
    state = {"merged": [], "drafts": [], "open": [], "tags": [], "supersessions": [], "registry_versions": registry_versions()}
    candidates = {}
    prs = gh.pages(f"repos/{REPOSITORY}/pulls?state=all&base=master&sort=created&direction=asc")
    for short in prs:
        author = short.get("user", {})
        if (author.get("login"), author.get("id")) != bot or author.get("type") != "Bot":
            continue
        if any((short.get(part, {}).get("repo") or {}).get("full_name") != REPOSITORY for part in ("base", "head")):
            continue
        labels = {label["name"] for label in short["labels"]}
        if short["head"]["ref"] != RELEASE_BRANCH and "autorelease: pending" not in labels:
            continue
        if not short.get("merged_at") and short["state"] != "open":
            continue
        pr = gh.api(f"repos/{REPOSITORY}/pulls/{short['number']}")
        candidate = pr_candidate(gh, pr, bot, historical="autorelease: tagged" in labels)
        candidate["complete"] = False
        candidate["tagged"] = "autorelease: tagged" in labels
        candidates[pr["number"]] = candidate
        state["merged" if pr.get("merged_at") else "open"].append(candidate)
        if pr.get("merged_at"):
            for comment in gh.pages(f"repos/{REPOSITORY}/issues/{pr['number']}/comments"):
                author = comment.get("user", {})
                if (author.get("login"), author.get("id")) != bot or author.get("type") != "Bot":
                    continue
                if comment["body"].startswith(SUPERSESSION):
                    state["supersessions"].append(json.loads(comment["body"][len(SUPERSESSION):]))
    releases = gh.pages(f"repos/{REPOSITORY}/releases")
    state["releases"] = releases
    for release in releases:
        if not release["tag_name"].startswith("sdk-v"):
            continue
        version = release["tag_name"].removeprefix("sdk-v")
        parse_release(version)
        assets = gh.pages(f"repos/{REPOSITORY}/releases/{release['id']}/assets")
        release["assets"] = assets
        for asset in assets:
            if asset["name"] == "sdk-release-superseded.json":
                marker = json.loads(gh.asset(asset))
                if marker["old"]["version"] != version or marker["old"]["source_sha"] != release["target_commitish"]:
                    raise ValueError("supersession asset names a different candidate")
                state["supersessions"].append(marker)
        manifests = [a for a in assets if a["name"] == MANIFEST]
        if not manifests:
            # Draft creation or initial manifest upload may have been interrupted.
            matches = [c for c in state["merged"] if c["version"] == version]
            if len(matches) != 1 or not release["draft"] or release["target_commitish"] != matches[0]["source_sha"]:
                raise ValueError("SDK release missing immutable manifest and reviewed candidate binding")
            state["drafts"].append({**matches[0], "release_id": release["id"], "complete": False})
            continue
        manifest = json.loads(gh.asset(manifests[0]))
        validate_manifest(manifest, tag=release["tag_name"], source_sha=release["target_commitish"])
        candidate = candidates.get(manifest["release_pr"])
        if not candidate or identity(candidate) != identity(manifest):
            raise ValueError("retained release manifest has no matching reviewed merged release PR")
        completion_assets = [a for a in assets if a["name"] == "sdk-release-complete.json"]
        completed = False
        if completion_assets:
            completion = json.loads(gh.asset(completion_assets[0]))
            if (identity(completion) != identity(manifest) or completion["status"] != "complete"
                    or completion["manifest_sha256"] != hashlib.sha256(gh.asset(manifests[0])).hexdigest()
                    or set(completion["targets"]) != {"python", "typescript", "java", "go"}):
                raise ValueError("invalid full-publication completion marker")
            clock(completion["published_at"])
            completed = not release["draft"] and candidate["tagged"]
            candidate["complete"] = completed
        if not release["draft"] and not completion_assets:
            raise ValueError("published SDK release is missing verified completion evidence")
        state["drafts"].append({**candidate, "manifest": manifest, "release_id": release["id"], "complete": completed})
    for prefix in ("sdk-v", "sdk/go/v"):
        refs = gh.api(f"repos/{REPOSITORY}/git/matching-refs/tags/{prefix}")
        for ref in refs:
            state["tags"].append({"name": ref["ref"].removeprefix("refs/tags/"), "sha": ref["object"]["sha"], "type": ref["object"]["type"]})
    state["artifacts"] = gh.pages(f"repos/{REPOSITORY}/actions/artifacts", "artifacts")
    merged = state["merged"]
    baseline = max(merged, key=lambda c: c["merged_at"])["source_sha"] if merged else BOOTSTRAP_SHA
    commits = gh.pages(f"repos/{REPOSITORY}/compare/{baseline}...master", "commits")
    state["sdk_changes"] = any(SIGNAL.search(c["commit"]["message"]) for c in commits)
    return state


def prerequisites(*, signing: bool = False) -> None:
    required = ["GH_TOKEN", "SDK_RELEASE_APP_ID", "SDK_RELEASE_APP_SLUG"]
    if signing:
        required += ["MAVEN_CENTRAL_USERNAME", "MAVEN_CENTRAL_PASSWORD", "MAVEN_GPG_PRIVATE_KEY", "MAVEN_GPG_PASSPHRASE"]
    missing = [key for key in required if not os.environ.get(key)]
    if os.environ.get("SDK_RELEASE_READY") != "true":
        missing.append("SDK_RELEASE_READY=true (verified exact PyPI/npm/Central ownership and trusted publishers)")
    if missing:
        raise ValueError("publication blocked: missing " + ", ".join(missing) + "; no tags or registry writes performed")


def run(args: list[str], root: Path, **kwargs) -> str:
    result = subprocess.run(args, cwd=root, check=True, text=True, **kwargs)
    return result.stdout or ""


def checkout(root: Path, ref: str) -> None:
    run(["git", "fetch", "origin", ref], root)
    run(["git", "checkout", "--detach", "FETCH_HEAD"], root)
    sha = run(["git", "rev-parse", "HEAD"], root, stdout=subprocess.PIPE).strip()
    if re.fullmatch(r"[0-9a-f]{40}", ref) and sha != ref:
        raise ValueError("checkout did not resolve exact reviewed source")


def maintain_pr(gh: GitHub, root: Path, version: str, bot: tuple[str, int]) -> None:
    prerequisites()
    # The token file prevents a token value in argv or subprocess error reports.
    with tempfile.TemporaryDirectory(prefix="manyforge-release-token-") as temporary:
        token_file = Path(temporary) / "token"
        token_file.write_text(os.environ["GH_TOKEN"])
        token_file.chmod(0o600)
        run([str(root / "tools/sdk/release/node_modules/.bin/release-please"), "release-pr",
             f"--repo-url={REPOSITORY}", "--target-branch=master", "--path=sdk", f"--release-as={version}",
             f"--token={token_file}"], root)
    opened = gh.pages(f"repos/{REPOSITORY}/pulls?state=open&base=master&head=joshrendek:{RELEASE_BRANCH}")
    if len(opened) != 1:
        raise ValueError("Release Please did not produce exactly one expected SDK PR")
    pr = gh.api(f"repos/{REPOSITORY}/pulls/{opened[0]['number']}")
    candidate = pr_candidate(gh, pr, bot)
    if candidate["version"] != version:
        raise ValueError("Release Please ignored allocated --release-as")
    checkout(root, candidate["source_sha"])
    run([sys.executable, "tools/sdk/generate.py"], root)
    # Reuse the CI version-only proof rather than creating a second exemption.
    from release_signal import changed_paths, release_delta
    base = pr["base"]["sha"]
    run(["git", "fetch", "origin", base], root)
    run(["git", "add", "--", "sdk", "api", "tools/sdk"], root)
    # release_delta compares committed trees; commit only when regeneration changed bytes.
    changed = run(["git", "diff", "--cached", "--name-only"], root, stdout=subprocess.PIPE).strip()
    if changed:
        run(["git", "-c", f"user.name={bot[0]}", "-c", f"user.email={bot[1]}+{bot[0]}@users.noreply.github.com",
             "commit", "-m", f"chore(master): release sdk {version}"], root)
    release_delta(root, base, changed_paths(root, base), version)
    run(["git", "push", f"--force-with-lease=refs/heads/{RELEASE_BRANCH}:{candidate['source_sha']}",
         "origin", f"HEAD:refs/heads/{RELEASE_BRANCH}"], root)


def recover_stage(gh: GitHub, state: dict, candidate: dict, destination: Path) -> bool:
    name = artifact_name(candidate)
    retained = [a for a in state.get("artifacts", []) if a["name"] == name]
    manifests = [c["manifest"] for c in state.get("drafts", []) if identity(c) == identity(candidate) and "manifest" in c]
    manifest = manifests[0] if manifests else None
    if retained:
        available = [a for a in retained if not a["expired"]]
        if not available:
            if manifest is None:
                raise ValueError("retained candidate artifact expired; restore original bytes, never rebuild a staged candidate")
        else:
            # More than one retry may retain the same candidate: compare every copy.
            original = None
            for item in available:
                retention_run = gh.api(f"repos/{REPOSITORY}/actions/runs/{item['workflow_run']['id']}")
                if (retention_run["path"] != ".github/workflows/sdk-release.yml"
                        or retention_run["event"] not in ("schedule", "workflow_run")
                        or retention_run["head_branch"] != "master"
                        or retention_run["head_repository"]["full_name"] != REPOSITORY):
                    raise ValueError("candidate artifact was not retained by the trusted master coordinator")
                raw = gh.api(f"repos/{REPOSITORY}/actions/artifacts/{item['id']}/zip", binary=True)
                with zipfile.ZipFile(io.BytesIO(raw)) as archive:
                    names = archive.namelist()
                    if any(Path(n).name != n or n in ("", ".", "..") for n in names) or len(names) != len(set(names)):
                        raise ValueError("unsafe retained staging archive")
                    current = {n: archive.read(n) for n in names}
                if original is not None and current != original:
                    raise ValueError("conflicting retained bytes for immutable candidate")
                original = current
            destination.mkdir(parents=True, exist_ok=True)
            for filename, content in original.items():
                path = destination / filename
                if path.exists() and path.read_bytes() != content:
                    raise ValueError("local recovery checksum conflict")
                path.write_bytes(content)
            recovered = json.loads((destination / MANIFEST).read_text())
            validate_manifest(recovered, destination, source_sha=candidate["source_sha"], tag="sdk-v" + candidate["version"])
            if manifest is not None and recovered != manifest:
                raise ValueError("retained Actions manifest disagrees with draft manifest")
            return True
    if manifest:
        release = next(r for r in state["releases"] if r["id"] == candidate["release_id"])
        assets = {a["name"]: a for a in release["assets"]}
        destination.mkdir(parents=True, exist_ok=True)
        for record in manifest["artifacts"]:
            if record["filename"] not in assets:
                raise ValueError("incomplete draft and no retained Actions bytes; restore original artifact before recovery")
            (destination / record["filename"]).write_bytes(gh.asset(assets[record["filename"]]))
        save_manifest(destination / MANIFEST, manifest)
        validate_manifest(manifest, destination)
        return True
    if candidate.get("release_id"):
        raise ValueError("draft exists without recoverable staging bytes; refusing a rebuild")
    return False


def stage(gh: GitHub, state: dict, candidate: dict, root: Path, destination: Path, now: datetime) -> dict:
    prerequisites(signing=True)
    verification = exact_ci(gh, candidate["source_sha"])
    if recover_stage(gh, state, candidate, destination):
        return json.loads((destination / MANIFEST).read_text())
    checkout(root, candidate["source_sha"])
    if (root / "sdk/version.txt").read_text().strip() != candidate["version"]:
        raise ValueError("candidate checkout version mismatch")
    run([sys.executable, "tools/sdk/generate.py", "--check"], root)
    artifacts = root / "sdk/dist"
    run([sys.executable, "tools/sdk/pack.py", "--output", str(artifacts)], root)
    for language in ("python", "typescript", "go", "java"):
        run([sys.executable, "tools/sdk/smoke.py", "--language", language, "--artifacts", str(artifacts)], root)
    from stage_java import stage_java
    stage_java(root, artifacts / "java", artifacts / "java/central-bundle.zip")
    manifest = build_manifest(root, artifacts, destination, candidate["source_sha"], candidate["release_pr"],
                              verification, int(os.environ["GITHUB_RUN_ID"]), now)
    save_manifest(destination / MANIFEST, manifest)
    validate_manifest(manifest, destination, publishing=True)
    return manifest


def upload_asset(gh: GitHub, release: dict, filename: Path) -> None:
    assets = gh.pages(f"repos/{REPOSITORY}/releases/{release['id']}/assets")
    existing = [a for a in assets if a["name"] == filename.name]
    if existing:
        if len(existing) != 1 or gh.asset(existing[0]) != filename.read_bytes():
            raise ValueError(f"immutable draft asset checksum conflict: {filename.name}")
        return
    url = release["upload_url"].split("{")[0] + "?name=" + urllib.parse.quote(filename.name, safe="")
    result = subprocess.run(["gh", "api", url, "--method", "POST", "-H", "Content-Type: application/octet-stream", "--input", str(filename)],
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if result.returncode:
        raise RuntimeError("draft asset upload interrupted; resume retained Actions artifact: " + result.stderr.decode())


def resume_publish(gh: GitHub, candidate: dict) -> str:
    tag = "sdk-v" + candidate["version"]
    runs = gh.pages(f"repos/{REPOSITORY}/actions/workflows/sdk-publish.yml/runs?head_sha={candidate['source_sha']}", "workflow_runs")
    runs = [r for r in runs if r["head_sha"] == candidate["source_sha"] and r["head_branch"] == tag
            and r["event"] in ("push", "workflow_dispatch") and r["head_repository"]["full_name"] == REPOSITORY]
    if runs:
        latest = max(runs, key=lambda r: (r["run_number"], r.get("run_attempt", 1)))
        if latest["status"] != "completed":
            return "Original tag publication is already running"
        gh.api(f"repos/{REPOSITORY}/actions/runs/{latest['id']}/rerun", method="POST")
        return f"Requested original tag run {latest['id']} rerun"
    gh.api(f"repos/{REPOSITORY}/actions/workflows/sdk-publish.yml/dispatches", method="POST",
           data={"ref": tag, "inputs": {"tag_ref": "refs/tags/" + tag}})
    return "Requested recovery dispatch against original tag " + tag


def finalize(gh: GitHub, state: dict, candidate: dict, destination: Path) -> str:
    prerequisites()
    manifest = json.loads((destination / MANIFEST).read_text())
    tag = "sdk-v" + candidate["version"]
    validate_manifest(manifest, destination, tag=tag, source_sha=candidate["source_sha"], publishing=True)
    exact_ci(gh, candidate["source_sha"])
    # Prove full immutable Actions retention before creating even a draft release.
    with tempfile.TemporaryDirectory(prefix="manyforge-retention-proof-") as temporary:
        retained_state = {**state, "drafts": [], "artifacts": gh.pages(f"repos/{REPOSITORY}/actions/artifacts", "artifacts")}
        if not recover_stage(gh, retained_state, {k: v for k, v in candidate.items() if k != "release_id"}, Path(temporary)):
            raise ValueError("complete Actions staging artifact is required before draft upload/tagging")
        restored = json.loads((Path(temporary) / MANIFEST).read_text())
        if restored != manifest:
            raise ValueError("Actions retained manifest differs from local immutable staging")
    matches = [r for r in gh.pages(f"repos/{REPOSITORY}/releases") if r["tag_name"] == tag]
    if len(matches) > 1:
        raise ValueError("multiple SDK releases claim one tag")
    if matches:
        release = matches[0]
        if release["target_commitish"] != candidate["source_sha"]:
            raise ValueError("release no longer matches incomplete candidate")
    else:
        release = gh.api(f"repos/{REPOSITORY}/releases", method="POST", data={"tag_name": tag,
                         "target_commitish": candidate["source_sha"], "name": f"SDK {candidate['version']}", "draft": True,
                         "body": f"Reviewed source {candidate['source_sha']} (PR #{candidate['release_pr']}). Publication incomplete; packages may become visible independently. See immutable {MANIFEST}."})
    upload_asset(gh, release, destination / MANIFEST)
    for record in manifest["artifacts"]:
        upload_asset(gh, release, destination / record["filename"])
    # Every upload helper confirms existing bytes; newly uploaded bytes are checked again.
    assets = {a["name"]: a for a in gh.pages(f"repos/{REPOSITORY}/releases/{release['id']}/assets")}
    for filename in [MANIFEST] + [r["filename"] for r in manifest["artifacts"]]:
        if gh.asset(assets[filename]) != (destination / filename).read_bytes():
            raise ValueError("post-upload draft checksum conflict")
    ref = gh.api(f"repos/{REPOSITORY}/git/ref/tags/{tag}", missing=True)
    if ref:
        if ref["object"]["sha"] != candidate["source_sha"] or ref["object"]["type"] != "commit":
            raise ValueError("umbrella tag is not a lightweight tag at original source SHA")
        return resume_publish(gh, candidate)
    gh.api(f"repos/{REPOSITORY}/git/refs", method="POST", data={"ref": "refs/tags/" + tag, "sha": candidate["source_sha"]})
    return "Created original source-bound umbrella tag; publication remains incomplete"


def supersede(gh: GitHub, state: dict, old_version: str, replacement_version: str, reason: str) -> None:
    prerequisites()
    candidates = {c["version"]: c for c in state["merged"]}
    old, replacement = candidates[old_version], candidates[replacement_version]
    if old.get("complete"):
        raise ValueError("completed releases cannot be marked incomplete/superseded")
    marker = {"old": {k: old[k] for k in ("version", "source_sha", "release_pr")},
              "replacement": {k: replacement[k] for k in ("version", "source_sha", "release_pr")}, "reason": reason}
    reconcile({**state, "supersessions": [*state["supersessions"], marker]}, datetime.now(timezone.utc))
    exact_ci(gh, replacement["source_sha"])
    runs = gh.pages(f"repos/{REPOSITORY}/actions/workflows/sdk-publish.yml/runs?head_sha={old['source_sha']}", "workflow_runs")
    if any(r["status"] != "completed" for r in runs):
        raise ValueError("cancel/wait for original candidate publication before explicit supersession")
    for release in state["releases"]:
        if release["tag_name"] == "sdk-v" + old_version:
            with tempfile.TemporaryDirectory(prefix="manyforge-supersession-") as temporary:
                path = Path(temporary) / "sdk-release-superseded.json"
                write_json(path, marker)
                upload_asset(gh, release, path)
    if marker not in state["supersessions"]:
        gh.api(f"repos/{REPOSITORY}/issues/{old['release_pr']}/comments", method="POST",
               data={"body": SUPERSESSION + json.dumps(marker, sort_keys=True)})
    for release in state["releases"]:
        if release["tag_name"] == "sdk-v" + old_version:
            text = "\n\nINCOMPLETE / SUPERSEDED: " + replacement_version + " at " + replacement["source_sha"] + ". " + reason
            if text not in (release["body"] or ""):
                gh.api(f"repos/{REPOSITORY}/releases/{release['id']}", method="PATCH", data={"body": (release["body"] or "") + text})
    # Preserve pending label, every artifact, and every tag. The marker, not removal,
    # makes the candidate nonblocking while its number remains permanently reserved.


def write_json(path: Path, value: dict) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("plan", "stage", "finalize", "dry-run", "supersede"))
    parser.add_argument("--now", help="Explicit aware clock (required for dry-run)")
    parser.add_argument("--state", type=Path, help="Read-only fixture state, dry-run only")
    parser.add_argument("--plan", type=Path, default=Path("sdk-release-plan.json"))
    parser.add_argument("--root", type=Path, default=ROOT)
    parser.add_argument("--artifacts", type=Path, help="Existing sdk-pack artifacts, dry-run only")
    parser.add_argument("--output", type=Path, default=Path("sdk-release-stage"))
    parser.add_argument("--old-version")
    parser.add_argument("--replacement-version")
    parser.add_argument("--reason")
    args = parser.parse_args()
    try:
        if args.command != "dry-run" and (args.state or args.artifacts):
            raise ValueError("fixture state/artifact inputs are allowed only in zero-write dry-run")
        if args.command == "dry-run" and not args.now:
            raise ValueError("dry-run requires --now; no implicit clock")
        now = clock(args.now) if args.now else datetime.now(timezone.utc)
        gh = GitHub()
        bot = None
        if args.state:
            state = json.loads(args.state.read_text())
        else:
            bot = configured_app_identity(dict(os.environ))
            state = acquire_state(gh, bot)
        decision = reconcile(state, now)
        if args.command in ("plan", "dry-run"):
            decision["prepared_at"] = now.isoformat()
            if args.command == "plan":
                prerequisites()
                if decision["action"] == "release-pr":
                    maintain_pr(gh, args.root.resolve(), decision["version"], bot)
            if args.command == "dry-run" and args.artifacts:
                if decision["action"] == "idle":
                    raise ValueError("cannot stage a calendar-only release")
                candidate = decision.get("candidate") or state.get("rehearsal_candidate")
                if not candidate or candidate["version"] != decision.get("version", candidate["version"]):
                    raise ValueError("dry-run staged inputs need matching explicit rehearsal_candidate identity")
                if (args.root / "sdk/version.txt").read_text().strip() != candidate["version"]:
                    raise ValueError("dry-run source stamps differ from allocated candidate")
                manifest = build_manifest(args.root.resolve(), args.artifacts.resolve(), args.output.resolve(), candidate["source_sha"],
                                          candidate["release_pr"], candidate["verification_runs"], candidate["staging_run_id"], now, rehearsal=True)
                save_manifest(args.output / MANIFEST, manifest)
                decision["manifest"] = manifest
                decision["registry_writes"] = 0
                decision["tag_writes"] = 0
            write_json(args.plan, decision)
            if os.environ.get("GITHUB_OUTPUT"):
                with open(os.environ["GITHUB_OUTPUT"], "a") as output:
                    output.write("action=" + decision["action"] + "\n")
                    if decision["action"] == "resume":
                        output.write("artifact_name=" + artifact_name(decision["candidate"]) + "\n")
            print(json.dumps(decision, indent=2, sort_keys=True))
        elif args.command == "supersede":
            if not all((args.old_version, args.replacement_version, args.reason)):
                raise ValueError("supersede requires --old-version, --replacement-version, and --reason")
            supersede(gh, state, args.old_version, args.replacement_version, args.reason)
        else:
            original = json.loads(args.plan.read_text())
            if decision["action"] != "resume" or identity(decision["candidate"]) != identity(original["candidate"]):
                raise ValueError("candidate changed between workflow phases; refusing substitution")
            if args.command == "stage":
                manifest = stage(gh, state, decision["candidate"], args.root.resolve(), args.output.resolve(), clock(original["prepared_at"]))
                print("Retained staging ready: " + manifest["staging_artifact_name"])
                if os.environ.get("GITHUB_OUTPUT"):
                    retained = any(a["name"] == artifact_name(decision["candidate"]) and not a["expired"] for a in state["artifacts"])
                    with open(os.environ["GITHUB_OUTPUT"], "a") as output:
                        output.write("upload_required=" + ("false" if retained else "true") + "\n")
            else:
                print(finalize(gh, state, decision["candidate"], args.output.resolve()))
    except (ValueError, RuntimeError, KeyError, OSError, subprocess.CalledProcessError) as error:
        parser.exit(1, f"SDK release blocked: {error}\n")


if __name__ == "__main__":
    main()
