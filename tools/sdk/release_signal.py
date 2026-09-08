"""Require an SDK release signal; narrowly authenticate version-only release PRs.

This gate never allocates or increments versions. feat/fix and calendar boundaries
have no effect on the wire or SDK compatibility gates.
Copyright (c) 2026 ManyForge contributors. SPDX-License-Identifier: MIT
"""
from __future__ import annotations

import argparse
import io
import json
import os
from pathlib import Path, PurePosixPath
import re
import tarfile
import tempfile
import urllib.parse
import urllib.request

from compatibility import ROOT, blob, commit, git
from version import package_versions, parse_release

REPOSITORY = "joshrendek/manyforge"
RELEASE_BRANCH = "release-please--branches--master--components--sdk"
SIGNAL = re.compile(r"^(?:feat|fix)\(sdk\):\s+\S", re.MULTILINE)
RELEASE_FOOTER = "This PR was generated with [Release Please](https://github.com/googleapis/release-please)."


def affects_sdk(paths: set[str]) -> bool:
    return any(path.startswith(("api/", "sdk/", "tools/sdk/")) for path in paths)


def changed_paths(root: Path, base: str) -> set[str]:
    return {path.decode() for path in git(root, "diff", "--name-only", "--no-renames", "-z", base, "HEAD", "--").split(b"\0") if path}


def github_json(path: str):
    request = urllib.request.Request("https://api.github.com/" + path, headers={"Accept": "application/vnd.github+json", "User-Agent": "manyforge-sdk-signal", "X-GitHub-Api-Version": "2022-11-28"})
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.load(response)


def configured_app_identity(environment: dict[str, str]) -> tuple[str, int]:
    slug, app_id = environment.get("SDK_RELEASE_APP_SLUG", ""), environment.get("SDK_RELEASE_APP_ID", "")
    if not re.fullmatch(r"[a-z0-9]+(?:-[a-z0-9]+)*", slug) or not re.fullmatch(r"[1-9][0-9]*", app_id):
        raise ValueError("release exemption requires configured SDK_RELEASE_APP_SLUG and SDK_RELEASE_APP_ID")
    app = github_json("apps/" + slug)
    if app.get("slug") != slug or app.get("id") != int(app_id):
        raise ValueError("configured release GitHub App identity does not match GitHub")
    bot = github_json("users/" + urllib.parse.quote(slug + "[bot]", safe=""))
    if bot.get("login") != slug + "[bot]" or bot.get("type") != "Bot" or not isinstance(bot.get("id"), int):
        raise ValueError("release GitHub App bot identity could not be verified")
    return bot["login"], bot["id"]


def validate_release_pr(pr: dict, version: str, bot: tuple[str, int]) -> None:
    parse_release(version)
    author = pr.get("user", {})
    if (author.get("login"), author.get("id")) != bot or author.get("type") != "Bot":
        raise ValueError("release PR was not authored by the configured GitHub App")
    base, head = pr.get("base", {}), pr.get("head", {})
    if base.get("ref") != "master" or head.get("ref") != RELEASE_BRANCH:
        raise ValueError("release PR branch does not match the configured SDK component")
    if any(part.get("repo", {}).get("full_name") != REPOSITORY for part in (base, head)):
        raise ValueError("release PR must originate in the configured repository, not a fork")
    if "autorelease: pending" not in {label.get("name") for label in pr.get("labels", [])}:
        raise ValueError("release PR is not pending publication")
    if pr.get("title") != f"chore(master): release sdk {version}":
        raise ValueError("release PR title and canonical version disagree")
    body = pr.get("body") or ""
    parts = re.split(r"(?m)^---\s*$", body.replace("\r\n", "\n"))
    if len(parts) < 3 or RELEASE_FOOTER not in parts[-1]:
        raise ValueError("release PR lacks Release Please body metadata")
    notes = "---".join(parts[1:-1]).strip()
    summaries = re.findall(r"<summary>([^<]+)</summary>", notes)
    if summaries:
        valid = summaries == [f"sdk: {version}"]
    else:
        heading = re.match(r"^#{2,}\s+\[?([0-9]+\.[0-9]+\.[0-9]+)(?=\]|\s|$)", notes)
        valid = heading is not None and heading.group(1) == version
    if not valid:
        raise ValueError("release body metadata identifies a different version or component")


def validate_release_delta(base_files: dict[str, bytes], revision_files: dict[str, bytes], generated: dict[str, bytes], version: str) -> None:
    """Require exact deterministic regeneration and only the release component's notes.

    generated is obtained by running the unchanged base generator with only its
    version.txt altered. It is never derived by replacing arbitrary source text.
    """
    package_versions(version)
    old_version = base_files["sdk/version.txt"].decode().strip()
    previous_release = json.loads(base_files.get(".release-please-manifest.json", b"{}"))
    if parse_release(version) < parse_release(old_version) or (version == old_version and previous_release):
        raise ValueError("release candidate must advance a previously released canonical version")
    if revision_files.get("sdk/version.txt") != (version + "\n").encode():
        raise ValueError("release version.txt is not the exact canonical stamp")
    old_manifest = json.loads(base_files["sdk/generated-files.json"])
    new_manifest = json.loads(revision_files["sdk/generated-files.json"])
    if new_manifest != {**old_manifest, "release": version}:
        raise ValueError("release changes generated ownership rather than only version stamps")
    if set(generated) != set(old_manifest["generated"]):
        raise ValueError("version-only regeneration changed the generated file set")
    if set(previous_release) - {"sdk"}:
        raise ValueError("release manifest contains unexpected components")
    if json.loads(revision_files.get(".release-please-manifest.json", b"null")) != {"sdk": version}:
        raise ValueError("release manifest and canonical version disagree")
    allowed = {"sdk/version.txt", "sdk/generated-files.json", ".release-please-manifest.json", "sdk/CHANGELOG.md"} | set(generated)
    for path in base_files.keys() | revision_files.keys():
        old, new = base_files.get(path), revision_files.get(path)
        if path in generated:
            if new != generated[path]:
                raise ValueError(f"release change is not the deterministic version stamp: {path}")
        elif old != new and path not in allowed:
            raise ValueError(f"release includes a non-version change: {path}")
    if "sdk/CHANGELOG.md" in base_files and "sdk/CHANGELOG.md" not in revision_files:
        raise ValueError("release removed its changelog")


def release_delta(root: Path, base: str, paths: set[str], version: str) -> None:
    from generate import generate_tree, generator_jar
    # Copy only generator inputs/packages from the reviewed base. No checkout,
    # hooks, branch writes or untrusted PR tooling execution occurs here.
    with tempfile.TemporaryDirectory(prefix="manyforge-release-signal-") as temporary:
        workspace = Path(temporary)
        base_root = workspace / "base"
        base_root.mkdir()
        archive = git(root, "archive", "--format=tar", base, "api", "sdk", "tools/sdk")
        with tarfile.open(fileobj=io.BytesIO(archive), mode="r:") as contents:
            for member in contents.getmembers():
                path = PurePosixPath(member.name)
                if path.is_absolute() or ".." in path.parts or not (member.isdir() or member.isfile()):
                    raise ValueError("release generator inputs include an unsafe archive entry")
                target = base_root / path
                if member.isdir():
                    target.mkdir(parents=True, exist_ok=True)
                else:
                    target.parent.mkdir(parents=True, exist_ok=True)
                    target.write_bytes(contents.extractfile(member).read())
        manifest = json.loads((base_root / "sdk/generated-files.json").read_bytes())
        # The running generator itself must also be unchanged. This is checked
        # before execution, not by trusting a claimed generated-files list.
        permitted = set(manifest["generated"]) | {"sdk/version.txt", "sdk/generated-files.json", ".release-please-manifest.json", "sdk/CHANGELOG.md"}
        for path in paths:
            if path not in permitted:
                raise ValueError(f"release includes a non-version change: {path}")
        selected = paths | set(manifest["generated"]) | {"sdk/version.txt", "sdk/generated-files.json", ".release-please-manifest.json"}
        base_files = {}
        revision_files = {}
        for path in selected:
            source, current = base_root / path, root / path
            previous = source.read_bytes() if source.is_file() else None
            if path == ".release-please-manifest.json":
                previous = blob(root, base, path, optional=True)
            if current.is_symlink():
                raise ValueError(f"release contains a symlink: {path}")
            if previous is not None:
                base_files[path] = previous
            if current.is_file():
                revision_files[path] = current.read_bytes()
        (base_root / "sdk/version.txt").write_text(version + "\n")
        generated, actual_release = generate_tree(base_root, workspace / "generated", generator_jar(root))
        if actual_release != version:
            raise ValueError("generator produced a different release identity")
        validate_release_delta(base_files, revision_files, {str(path): data for path, data in generated.items()}, version)


def check_signal(root: Path, event: dict, base: str, environment: dict[str, str]) -> str:
    paths = changed_paths(root, base)
    if not affects_sdk(paths):
        return "No SDK-affecting paths changed"
    pr = event.get("pull_request")
    if pr is not None:
        message = (pr.get("title") or "") + "\n" + (pr.get("body") or "")
    else:
        # On master pushes the squash commit contains the reviewed title/body.
        # Never trust an event's truncated commits array as the only source.
        message = git(root, "log", "-1", "--format=%B", "HEAD").decode()
    if SIGNAL.search(message):
        return "SDK feat/fix release signal accepted (version remains allocator-owned)"
    bot = configured_app_identity(environment)
    if pr is None:
        head = commit(root, "HEAD")
        candidates = github_json(f"repos/{REPOSITORY}/commits/{head}/pulls")
        matching = [candidate for candidate in candidates if candidate.get("merged_at") and candidate.get("merge_commit_sha") == head]
        if len(matching) != 1:
            raise ValueError("master release commit has no unique reviewed merged release PR")
        pr = matching[0]
    version = (root / "sdk/version.txt").read_text().strip()
    validate_release_pr(pr, version, bot)
    release_delta(root, base, paths, version)
    return "Verified GitHub App release PR: deterministic version-only exception accepted"


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--event", type=Path, required=True)
    parser.add_argument("--base-ref", required=True)
    args = parser.parse_args()
    try:
        event = json.loads(args.event.read_bytes())
        base = commit(ROOT, args.base_ref)
        print(check_signal(ROOT, event, base, dict(os.environ)))
    except (OSError, ValueError, KeyError, RuntimeError, tarfile.TarError) as error:
        parser.exit(1, f"SDK-affecting changes require feat(sdk): or fix(sdk): in the PR title/body. Release exception rejected: {error}\n")


if __name__ == "__main__":
    main()
