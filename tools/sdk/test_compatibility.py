"""Negative controls for released baselines and SDK metadata compatibility."""
import copy
import json
from pathlib import Path
import subprocess
import tempfile
import unittest

from compatibility import check_baseline, compare_metadata, discover_release, validate_ref


def document():
    return {"paths": {"/contacts": {"get": {
        "operationId": "ContactsList", "x-manyforge-resource": "contacts",
        "x-manyforge-method": "list", "x-manyforge-audience": "management",
        "x-manyforge-business-param": "id",
        "x-manyforge-pagination": {"cursor": "cursor", "limit": "limit", "items": "items", "next": "next_cursor"},
    }}}}


class MetadataCompatibilityTest(unittest.TestCase):
    def test_additive_operation_keeps_existing_client_contract(self):
        base = document()
        revision = copy.deepcopy(base)
        revision["paths"]["/companies"] = {"get": {**base["paths"]["/contacts"]["get"], "operationId": "CompaniesList", "x-manyforge-resource": "companies"}}
        self.assertEqual(compare_metadata(base, revision), [])

    def test_public_method_rename_is_breaking_across_calendar_boundary(self):
        base, revision = document(), document()
        base["info"] = {"version": "2026.12.4"}
        revision["info"] = {"version": "2027.1.1"}
        revision["paths"]["/contacts"]["get"]["x-manyforge-method"] = "search"
        self.assertTrue(compare_metadata(base, revision))

    def test_removed_operation_and_changed_credential_scope_are_breaking(self):
        base = document()
        self.assertTrue(compare_metadata(base, {"paths": {}}))
        revision = document()
        revision["paths"]["/contacts"]["get"]["x-manyforge-audience"] = "public"
        self.assertTrue(compare_metadata(base, revision))

    def test_git_inputs_reject_options_revision_expressions_and_path_injection(self):
        for ref in ("--help", "HEAD:api/openapi.yaml", "HEAD~1", "master..other", "a\nb", "refs/tags/../master"):
            with self.subTest(ref=ref), self.assertRaises(ValueError):
                validate_ref(ref)


class ReleasedBaselineTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        directory = Path(self.temporary.name)
        self.root, self.remote = directory / "work", directory / "origin.git"
        self.root.mkdir()
        self.command(directory, "init", "--bare", str(self.remote))
        self.command(self.root, "init")
        self.command(self.root, "config", "user.name", "SDK test")
        self.command(self.root, "config", "user.email", "sdk-test@example.invalid")
        self.command(self.root, "remote", "add", "origin", str(self.remote))
        self.commit_files({"initial.txt": "initial\n"})

    def command(self, root, *args):
        return subprocess.check_output(["git", *args], cwd=root, stderr=subprocess.STDOUT).decode().strip()

    def commit_files(self, files):
        for name, value in files.items():
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(value)
        self.command(self.root, "add", "--all")
        self.command(self.root, "commit", "-m", "fixture")
        return self.command(self.root, "rev-parse", "HEAD")

    def release(self, version="2026.9.1", go="1.202609.1"):
        sha = self.commit_files({
            "sdk/version.txt": version + "\n",
            "sdk/python/pyproject.toml": f'[project]\nversion = "{version}"\n',
            "sdk/typescript/package.json": json.dumps({"version": version}),
            "sdk/java/pom.xml": f'<project xmlns="http://maven.apache.org/POM/4.0.0"><version>{version}</version></project>',
            "sdk/go/version.go": f'package manyforge\nconst Version = "{go}"\n',
        })
        tag = "sdk-v" + version
        self.command(self.root, "tag", tag)
        self.command(self.root, "push", "origin", "refs/tags/" + tag)
        return sha

    def test_only_proven_empty_remote_allows_bootstrap(self):
        self.assertIsNone(discover_release(self.root))
        with self.assertRaises(RuntimeError):
            discover_release(self.root, "sdk-v2026.9.1")
        self.command(self.root, "remote", "set-url", "origin", str(self.remote / "unreachable"))
        with self.assertRaises(RuntimeError):
            discover_release(self.root)

    def test_missing_released_contract_does_not_become_bootstrap(self):
        sha = self.release()
        self.assertEqual(discover_release(self.root), sha)
        with self.assertRaises(RuntimeError):
            check_baseline(self.root, sha, Path("not-executed"), bootstrap=False)

    def test_explicit_branch_cannot_substitute_for_released_contract(self):
        with self.assertRaises(ValueError):
            discover_release(self.root, "master")

    def test_calendar_to_go_mapping_mismatch_fails(self):
        self.release(go="1.202610.1")
        with self.assertRaises(RuntimeError):
            discover_release(self.root)

    def test_tag_and_recorded_release_identity_must_match(self):
        self.release()
        self.command(self.root, "tag", "sdk-v2026.10.1")
        self.command(self.root, "push", "origin", "refs/tags/sdk-v2026.10.1")
        with self.assertRaises(RuntimeError):
            discover_release(self.root)

    def test_latest_tag_uses_numeric_calendar_order(self):
        self.release()
        latest = self.release("2026.10.1", "1.202610.1")
        self.assertEqual(discover_release(self.root), latest)
        with self.assertRaises(RuntimeError):
            discover_release(self.root, "sdk-v2026.9.1")


if __name__ == "__main__":
    unittest.main()
