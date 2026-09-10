"""Persistent candidate recovery and release ownership boundaries (no live writes)."""
from copy import deepcopy
from datetime import datetime, timezone
from pathlib import Path
import json
import tempfile
import unittest
from unittest.mock import patch

from release import acquire_state, artifact_name, exact_ci, identity, reconcile, resume_publish, SUPERSESSION, upload_asset
from release_signal import RELEASE_BRANCH, RELEASE_FOOTER, REPOSITORY


NOW = datetime(2026, 9, 8, tzinfo=timezone.utc)
SHA = "a" * 40
OTHER_SHA = "b" * 40


def candidate(version="2026.9.1", sha=SHA, number=101, merged_at="2026-09-07T12:00:00Z"):
    return {"version": version, "source_sha": sha, "release_pr": number, "merged_at": merged_at, "complete": False}


def state(**updates):
    return {"merged": [], "drafts": [], "open": [], "registry_versions": [], "tags": [], "supersessions": [], "sdk_changes": True, **updates}


class ReconciliationTest(unittest.TestCase):
    def test_canceled_event_recovers_oldest_merged_sha_even_after_master_advances_and_month_rolls(self):
        first = candidate()
        later = candidate("2026.9.2", OTHER_SHA, 102, "2026-09-08T12:00:00Z")
        before = reconcile(state(merged=[later, first]), NOW)
        after = reconcile(state(merged=[later, first], drafts=[{**first, "release_id": 99}], current_master="c" * 40),
                          datetime(2026, 10, 1, tzinfo=timezone.utc))
        self.assertEqual(before["action"], "resume")
        self.assertEqual(identity(before["candidate"]), identity(after["candidate"]))
        self.assertEqual(after["package_versions"]["go"], "1.202609.1")
        self.assertEqual(artifact_name(before["candidate"]), artifact_name(after["candidate"]))

    def test_retained_draft_is_not_lost_when_pending_event_is_missing(self):
        retained = candidate()
        decision = reconcile(state(drafts=[retained], sdk_changes=False), NOW)
        self.assertEqual(decision["action"], "resume")
        self.assertEqual(identity(decision["candidate"]), identity(retained))

    def test_version_cannot_be_reassigned_to_corrected_source_or_another_pr(self):
        for draft in (candidate(sha=OTHER_SHA), candidate(number=102)):
            with self.subTest(draft=draft), self.assertRaisesRegex(ValueError, "ownership conflict"):
                reconcile(state(merged=[candidate()], drafts=[draft]), NOW)

    def test_same_month_refresh_reuses_unmerged_candidate_but_external_collision_stops_it(self):
        opened = candidate("2026.9.2", merged_at=None)
        original = state(open=[opened], registry_versions=["2026.9.1"])
        self.assertEqual(reconcile(original, NOW)["version"], "2026.9.2")
        collided = deepcopy(original)
        collided["registry_versions"].append("v1.202609.2")
        with self.assertRaisesRegex(ValueError, "collides"):
            reconcile(collided, NOW)
        self.assertEqual(reconcile(original, datetime(2026, 10, 1, tzinfo=timezone.utc))["version"], "2026.10.1")

    def test_go_and_umbrella_tags_reserve_the_same_calendar_space(self):
        decision = reconcile(state(tags=[{"name": "sdk/go/v1.202609.9", "sha": SHA},
                                        {"name": "sdk-v2026.9.10", "sha": SHA}]), NOW)
        self.assertEqual(decision["version"], "2026.9.11")
        self.assertEqual(decision["package_versions"]["go"], "1.202609.11")

    def test_existing_umbrella_or_go_tag_cannot_point_elsewhere(self):
        for tag in ("sdk-v2026.9.1", "sdk/go/v1.202609.1"):
            with self.subTest(tag=tag), self.assertRaisesRegex(ValueError, "tag disagrees"):
                reconcile(state(merged=[candidate()], tags=[{"name": tag, "sha": OTHER_SHA}]), NOW)
        with self.assertRaisesRegex(ValueError, "tag disagrees"):
            reconcile(state(merged=[candidate()], tags=[{"name": "sdk-v2026.9.1", "sha": SHA, "type": "tag"}]), NOW)

    def test_calendar_never_creates_empty_release_and_regressed_clock_still_fails(self):
        self.assertEqual(reconcile(state(sdk_changes=False), NOW)["action"], "idle")
        with self.assertRaisesRegex(ValueError, "precedes"):
            reconcile(state(merged=[candidate()], registry_versions=["2026.10.1"]), NOW)

    def test_only_explicit_reviewed_newer_supersession_unblocks_partial_candidate(self):
        old, new = candidate(), candidate("2026.9.2", OTHER_SHA, 102)
        marker = {"old": dict(old), "replacement": dict(new), "reason": "Reviewed correction after partial publication"}
        pending = state(merged=[old, new], registry_versions=["2026.9.1"], supersessions=[marker])
        self.assertEqual(identity(reconcile(pending, NOW)["candidate"]), identity(new))
        bad = deepcopy(pending)
        bad["supersessions"][0]["replacement"]["source_sha"] = "c" * 40
        with self.assertRaisesRegex(ValueError, "replacement"):
            reconcile(bad, NOW)
        finished = deepcopy(pending)
        finished["merged"][1]["complete"] = True
        self.assertEqual(reconcile(finished, NOW)["version"], "2026.9.3")

    def test_multiple_open_release_prs_are_an_ownership_error(self):
        with self.assertRaisesRegex(ValueError, "multiple open"):
            reconcile(state(open=[candidate(), candidate("2026.9.2")]), NOW)


class FakeGitHub:
    def __init__(self, runs=(), assets=(), content=b""):
        self.runs, self.assets, self.content = list(runs), list(assets), content
        self.writes = []

    def pages(self, path, key=None):
        return self.assets if path.endswith("/assets") else self.runs

    def api(self, path, *, method="GET", data=None):
        if method != "GET":
            self.writes.append((path, method, data))

    def asset(self, asset):
        return self.content

def discovery_pr(released):
    version = released["version"]
    return {
        "number": released["release_pr"], "state": "closed", "merged_at": released["merged_at"],
        "merge_commit_sha": released["source_sha"],
        "user": {"login": "sdk-release[bot]", "id": 123, "type": "Bot"},
        "base": {"ref": "master", "repo": {"full_name": REPOSITORY}},
        "head": {"ref": RELEASE_BRANCH, "sha": released["source_sha"], "repo": {"full_name": REPOSITORY}},
        "labels": [{"name": "autorelease: pending"}],
        "title": f"chore(master): release sdk {version}",
        "body": f"Release automation\n---\n## {version}\nNotes\n---\n{RELEASE_FOOTER}",
    }


class DiscoveryGitHub(FakeGitHub):
    def __init__(self, prs, versions, comments=None):
        super().__init__()
        self.prs, self.versions, self.comments = prs, versions, comments or {}

    def pages(self, path, key=None):
        root = f"repos/{REPOSITORY}"
        if path == f"{root}/pulls?state=all&base=master&sort=created&direction=asc":
            return self.prs
        for pr in self.prs:
            if path == f"{root}/pulls/{pr['number']}/reviews":
                return [{"state": "APPROVED", "commit_id": pr["head"]["sha"], "user": {"id": 456, "type": "User"}}]
            if path == f"{root}/issues/{pr['number']}/comments":
                return self.comments.get(pr["number"], [])
        if path in (f"{root}/releases", f"{root}/actions/artifacts"):
            return []
        if path.startswith(f"{root}/compare/") and path.endswith("...master"):
            return []
        raise AssertionError(f"unexpected discovery read: {path}")

    def api(self, path, *, method="GET", data=None):
        if method != "GET":
            raise AssertionError("discovery must not mutate GitHub")
        root = f"repos/{REPOSITORY}"
        for pr in self.prs:
            if path == f"{root}/pulls/{pr['number']}":
                return pr
        if path in (f"{root}/git/matching-refs/tags/sdk-v", f"{root}/git/matching-refs/tags/sdk/go/v"):
            return []
        raise AssertionError(f"unexpected discovery read: {path}")

    def file(self, path, ref):
        version = self.versions[ref]
        if path == "sdk/version.txt":
            return version.encode()
        if path == ".release-please-manifest.json":
            return json.dumps({"sdk": version}).encode()
        raise AssertionError(f"unexpected candidate file: {path}")


class DiscoveryOwnershipTest(unittest.TestCase):
    def setUp(self):
        self.old = candidate()
        self.new = candidate("2026.9.2", OTHER_SHA, 102, "2026-09-08T12:00:00Z")
        self.prs = [discovery_pr(self.new), discovery_pr(self.old)]
        self.bot = ("sdk-release[bot]", 123)
        self.versions = {SHA: self.old["version"], OTHER_SHA: self.new["version"]}
        registry = patch("release.registry_versions", return_value=[])
        registry.start()
        self.addCleanup(registry.stop)

    def discover(self, prs=None, comments=None):
        gh = DiscoveryGitHub(self.prs if prs is None else prs, self.versions, comments)
        return reconcile(acquire_state(gh, self.bot), NOW)

    def test_fork_or_non_app_branch_impostor_cannot_block_oldest_pending_candidate(self):
        for author, repository in (
            ({"login": "outsider", "id": 999, "type": "User"}, "outsider/fork"),
            ({"login": self.bot[0], "id": self.bot[1], "type": "Bot"}, "outsider/fork"),
            ({"login": self.bot[0], "id": 999, "type": "Bot"}, REPOSITORY),
            ({"login": "other-app[bot]", "id": self.bot[1], "type": "Bot"}, REPOSITORY),
            ({"login": self.bot[0], "id": self.bot[1], "type": "User"}, REPOSITORY),
        ):
            with self.subTest(author=author, repository=repository):
                impostor = discovery_pr(candidate(sha="untrusted-not-a-sha", number=999, merged_at=None))
                impostor.update(user=author, state="open", labels=[], title="ordinary contribution")
                impostor["head"]["repo"]["full_name"] = repository
                decision = self.discover([impostor, *self.prs])
                self.assertEqual(decision["action"], "resume")
                self.assertEqual(identity(decision["candidate"]), identity(self.old))

    def test_outsider_supersession_comment_is_ignored_before_parsing(self):
        for author in (
            {"login": "outsider", "id": 999, "type": "User"},
            {"login": self.bot[0], "id": 999, "type": "Bot"},
            {"login": "other-app[bot]", "id": self.bot[1], "type": "Bot"},
            {"login": self.bot[0], "id": self.bot[1], "type": "User"},
        ):
            with self.subTest(author=author):
                comment = {"user": author, "body": SUPERSESSION + "not JSON"}
                decision = self.discover(comments={101: [comment]})
                self.assertEqual(identity(decision["candidate"]), identity(self.old))

    def test_malformed_trusted_app_candidate_still_blocks_discovery(self):
        for changes, message in (
            ({"title": "wrong release"}, "title"),
            ({"labels": []}, "pending"),
            ({"head": {**self.prs[0]["head"], "ref": "wrong-branch"}}, "branch"),
        ):
            with self.subTest(changes=changes):
                malformed = {**self.prs[0], **changes}
                with self.assertRaisesRegex(ValueError, message):
                    self.discover([malformed, self.prs[1]])

    def test_malformed_trusted_supersession_marker_still_blocks_discovery(self):
        comment = {"user": self.prs[1]["user"], "body": SUPERSESSION + "not JSON"}
        with self.assertRaises(json.JSONDecodeError):
            self.discover(comments={101: [comment]})

    def test_conflicting_trusted_supersession_markers_still_block_recovery(self):
        markers = [
            {"old": self.old, "replacement": self.new, "reason": "Reviewed correction"},
            {"old": self.old, "replacement": self.new, "reason": "Conflicting disposition"},
        ]
        comments = [{"user": self.prs[1]["user"], "body": SUPERSESSION + json.dumps(marker)} for marker in markers]
        with self.assertRaisesRegex(ValueError, "conflicting supersession markers"):
            self.discover(comments={101: comments})



def run_record(**changes):
    return {"id": 51, "run_number": 10, "run_attempt": 1, "head_sha": SHA, "head_branch": "master",
            "event": "push", "head_repository": {"full_name": "joshrendek/manyforge"},
            "status": "completed", "conclusion": "success", **changes}


class PublicationRecoveryTest(unittest.TestCase):
    def test_exact_master_push_ci_not_current_master_or_fork_or_pr_ci(self):
        for changes in ({"head_sha": OTHER_SHA}, {"head_branch": "feature"}, {"event": "pull_request"},
                        {"head_repository": {"full_name": "someone/fork"}}):
            with self.subTest(changes=changes), self.assertRaisesRegex(ValueError, "waiting"):
                exact_ci(FakeGitHub([run_record(**changes)]), SHA)
        verified = exact_ci(FakeGitHub([run_record()]), SHA)
        self.assertEqual(verified, [{"id": 51, "source_sha": SHA, "conclusion": "success"}])

    def test_failed_latest_ci_attempt_cannot_reuse_an_older_green_run(self):
        gh = FakeGitHub([run_record(), run_record(run_attempt=2, conclusion="failure")])
        with self.assertRaisesRegex(ValueError, "not successful"):
            exact_ci(gh, SHA)
        self.assertEqual(gh.writes, [])

    def test_canceled_original_tag_run_is_rerun_without_new_tag_or_branch_dispatch(self):
        gh = FakeGitHub([run_record(head_branch="sdk-v2026.9.1", conclusion="cancelled")])
        resume_publish(gh, candidate())
        self.assertEqual(gh.writes, [("repos/joshrendek/manyforge/actions/runs/51/rerun", "POST", None)])

    def test_missing_tag_event_dispatches_only_original_tag_and_active_run_is_not_duplicated(self):
        gh = FakeGitHub()
        resume_publish(gh, candidate())
        self.assertEqual(gh.writes, [("repos/joshrendek/manyforge/actions/workflows/sdk-publish.yml/dispatches", "POST",
                                    {"ref": "sdk-v2026.9.1", "inputs": {"tag_ref": "refs/tags/sdk-v2026.9.1"}})])
        running = FakeGitHub([run_record(head_branch="sdk-v2026.9.1", status="in_progress", conclusion=None)])
        resume_publish(running, candidate())
        self.assertEqual(running.writes, [])

    def test_partial_draft_retry_accepts_only_identical_existing_asset_bytes(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "package.whl"
            path.write_bytes(b"tested immutable bytes")
            same = FakeGitHub(assets=[{"name": path.name, "id": 1}], content=path.read_bytes())
            upload_asset(same, {"id": 42}, path)
            self.assertEqual(same.writes, [])
            conflict = FakeGitHub(assets=[{"name": path.name, "id": 1}], content=b"different build")
            with self.assertRaisesRegex(ValueError, "checksum conflict"):
                upload_asset(conflict, {"id": 42}, path)
            self.assertEqual(conflict.writes, [])


if __name__ == "__main__":
    unittest.main()
