"""Release labels must not authorize code changes or mismatched release identities."""
import json
import unittest
from unittest.mock import patch

from release_signal import (
    RELEASE_BRANCH, RELEASE_FOOTER, REPOSITORY, SIGNAL, affects_sdk,
    configured_app_identity, validate_release_delta, validate_release_pr,
)


def release_pr(version="2026.10.1"):
    return {
        "user": {"login": "sdk-release[bot]", "id": 123, "type": "Bot"},
        "base": {"ref": "master", "repo": {"full_name": REPOSITORY}},
        "head": {"ref": RELEASE_BRANCH, "repo": {"full_name": REPOSITORY}},
        "labels": [{"name": "autorelease: pending"}],
        "title": f"chore(master): release sdk {version}",
        "body": f"Release automation\n---\n<details><summary>sdk: {version}</summary>\nNotes\n</details>\n---\n{RELEASE_FOOTER}",
    }


def release_files(old="2026.9.2", new="2026.10.1"):
    manifest = {"release": old, "generated": ["sdk/go/version.go"], "handwritten": ["sdk/go/transport.go"]}
    base = {
        "sdk/version.txt": (old + "\n").encode(),
        "sdk/generated-files.json": json.dumps(manifest).encode(),
        ".release-please-manifest.json": json.dumps({"sdk": old}).encode(),
        "sdk/go/version.go": b'const Version = "1.202609.2"\n',
        "sdk/go/transport.go": b"stable runtime",
    }
    generated = {"sdk/go/version.go": b'const Version = "1.202610.1"\n'}
    revision = {
        **base, **generated,
        "sdk/version.txt": (new + "\n").encode(),
        "sdk/generated-files.json": json.dumps({**manifest, "release": new}).encode(),
        ".release-please-manifest.json": json.dumps({"sdk": new}).encode(),
        "sdk/CHANGELOG.md": b"Release notes\n",
    }
    return base, revision, generated


class ReleaseIdentityTest(unittest.TestCase):
    def test_verified_component_version_and_author_are_accepted(self):
        validate_release_pr(release_pr(), "2026.10.1", ("sdk-release[bot]", 123))

    def test_forged_release_label_does_not_authorize_a_human_or_other_bot(self):
        pr = release_pr()
        pr["user"] = {"login": "contributor", "id": 999, "type": "User"}
        with self.assertRaises(ValueError):
            validate_release_pr(pr, "2026.10.1", ("sdk-release[bot]", 123))
        pr["user"] = {"login": "sdk-release[bot]", "id": 999, "type": "Bot"}
        with self.assertRaises(ValueError):
            validate_release_pr(pr, "2026.10.1", ("sdk-release[bot]", 123))

    def test_label_and_title_do_not_override_branch_or_component_metadata(self):
        pr = release_pr()
        pr["head"]["ref"] = "feature-with-forged-label"
        with self.assertRaises(ValueError):
            validate_release_pr(pr, "2026.10.1", ("sdk-release[bot]", 123))
        pr = release_pr()
        pr["body"] = pr["body"].replace("sdk: 2026.10.1", "sdk: 2026.9.2")
        with self.assertRaises(ValueError):
            validate_release_pr(pr, "2026.10.1", ("sdk-release[bot]", 123))

    def test_matching_branch_from_fork_is_not_a_release(self):
        pr = release_pr()
        pr["head"]["repo"]["full_name"] = "someone/manyforge"
        with self.assertRaises(ValueError):
            validate_release_pr(pr, "2026.10.1", ("sdk-release[bot]", 123))

    def test_unset_or_mismatched_configured_app_fails_closed(self):
        with self.assertRaises(ValueError):
            configured_app_identity({})
        with patch("release_signal.github_json", return_value={"slug": "sdk-release", "id": 456}):
            with self.assertRaises(ValueError):
                configured_app_identity({"SDK_RELEASE_APP_SLUG": "sdk-release", "SDK_RELEASE_APP_ID": "123"})


class VersionOnlyDeltaTest(unittest.TestCase):
    def test_deterministic_stamps_and_notes_allow_month_rollover(self):
        base, revision, generated = release_files()
        validate_release_delta(base, revision, generated, "2026.10.1")

    def test_initial_unpublished_candidate_can_keep_its_prepared_version(self):
        base, revision, generated = release_files(old="2026.10.1")
        base[".release-please-manifest.json"] = b"{}"
        base["sdk/go/version.go"] = generated["sdk/go/version.go"]
        validate_release_delta(base, revision, generated, "2026.10.1")

    def test_runtime_edit_disqualifies_otherwise_valid_release(self):
        base, revision, generated = release_files()
        revision["sdk/go/transport.go"] = b"changed transport behavior"
        with self.assertRaises(ValueError):
            validate_release_delta(base, revision, generated, "2026.10.1")

    def test_generated_method_edit_is_not_a_version_stamp(self):
        base, revision, generated = release_files()
        revision["sdk/go/version.go"] += b"func RenamedMethod() {}\n"
        with self.assertRaises(ValueError):
            validate_release_delta(base, revision, generated, "2026.10.1")

    def test_manifest_and_go_mapping_cannot_claim_different_release(self):
        base, revision, generated = release_files()
        revision[".release-please-manifest.json"] = b'{"sdk":"2026.11.1"}'
        with self.assertRaises(ValueError):
            validate_release_delta(base, revision, generated, "2026.10.1")
        base, revision, generated = release_files()
        revision["sdk/go/version.go"] = b'const Version = "1.202611.1"\n'
        with self.assertRaises(ValueError):
            validate_release_delta(base, revision, generated, "2026.10.1")

    def test_added_generated_ownership_cannot_launder_runtime_change(self):
        base, revision, generated = release_files()
        manifest = json.loads(revision["sdk/generated-files.json"])
        manifest["generated"].append("sdk/go/transport.go")
        revision["sdk/generated-files.json"] = json.dumps(manifest).encode()
        with self.assertRaises(ValueError):
            validate_release_delta(base, revision, generated, "2026.10.1")


class OrdinarySignalTest(unittest.TestCase):
    def test_feature_and_fix_signals_are_independent_of_calendar_version(self):
        self.assertTrue(SIGNAL.search("feat(sdk): add a compatible resource"))
        self.assertTrue(SIGNAL.search("Product change\n\nfix(sdk): correct declared response"))
        self.assertFalse(SIGNAL.search("feat: ordinary product feature"))
        self.assertTrue(affects_sdk({"api/openapi.yaml"}))
        self.assertFalse(affects_sdk({"web/src/app.ts", "internal/crm/service.go"}))


if __name__ == "__main__":
    unittest.main()
