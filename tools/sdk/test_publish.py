"""Fake registry transports exercise safety and recovery, never public publication."""
from __future__ import annotations

import base64
import copy
import hashlib
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import zipfile

import publish
from release_manifest import COORDINATES, KINDS
from version import package_versions


SHA = "a" * 40
REF = "refs/tags/sdk-v2026.9.1"


class FakeGitHub:
    def __init__(self):
        self.retained = {}
        self.tags = {"sdk-v2026.9.1": SHA}
        self.writes = []

    def api(self, path, *, method="GET", value=None, **kwargs):
        if method != "GET":
            self.writes.append((path, value))
            return None
        if path.startswith("pulls/"):
            return {"merged": True, "merge_commit_sha": SHA, "base": {"ref": "master"}}
        if path.startswith("actions/runs/"):
            return {"head_sha": SHA, "conclusion": "success", "repository": {"full_name": publish.REPOSITORY}}
        raise AssertionError(path)

    def assets(self, release):
        return {name: {"name": name} for name in self.retained}

    def download(self, asset):
        return self.retained[asset["name"]]

    def immutable(self, release, name, value):
        data = publish.encoded(value)
        if name in self.retained and self.retained[name] != data:
            raise publish.Conflict("immutable receipt differs")
        self.retained[name] = data

    def tag(self, tag, sha, create=False):
        if tag in self.tags:
            if self.tags[tag] != sha:
                raise publish.Conflict("tag differs")
            return True
        if create:
            self.writes.append((tag, sha))
            self.tags[tag] = sha
            return True
        return False


class PublisherTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.github = FakeGitHub()
        records = []
        for target, kinds in KINDS.items():
            for kind in sorted(kinds):
                filename = f"{target}-{kind}.bin"
                payload = f"retained-{target}-{kind}".encode()
                if kind == "central-bundle":
                    stream = io.BytesIO()
                    with zipfile.ZipFile(stream, "w") as archive:
                        archive.writestr("artifact.jar", b"signed fixture")
                    payload = stream.getvalue()
                (self.directory / filename).write_bytes(payload)
                records.append({"filename": filename, "target": target, "kind": kind, "sha256": publish.digest(payload), "size": len(payload)})
        self.manifest = {"format": 1, "version": "2026.9.1", "package_versions": package_versions("2026.9.1"), "go_module_version": "v1.202609.1", "source_sha": SHA, "coordinates": COORDINATES, "release_pr": 105, "verification_runs": [{"id": 1, "source_sha": SHA, "conclusion": "success"}], "staging_run_id": 1, "staging_artifact_name": f"sdk-candidate-2026.9.1-{SHA}", "prepared_at": "2026-09-08T00:00:00Z", "rehearsal": False, "tools": {"fixture": "1"}, "artifacts": records, "go_module_sum": "h1:" + base64.b64encode(b"x" * 32).decode(), "contract_sha256": next(record["sha256"] for record in records if record["kind"] == "canonical-contract")}
        (self.directory / "sdk-manifest.json").write_bytes(publish.encoded(self.manifest))

    def publisher(self, manifest=None, ref=REF, source_sha=SHA):
        return publish.Publisher(self.github, manifest or self.manifest, self.directory, {"id": 1}, ref, source_sha)

    def test_wrong_go_mapping_rejected_before_writes(self):
        invalid = copy.deepcopy(self.manifest)
        invalid["package_versions"]["go"] = "1.202610.1"
        with self.assertRaises(ValueError):
            self.publisher(invalid)
        self.assertEqual(self.github.writes, [])
        self.assertEqual(self.github.retained, {})

    def test_moved_tag_cannot_publish_candidate(self):
        self.github.tags["sdk-v2026.9.1"] = "b" * 40
        with self.assertRaises(publish.Conflict):
            self.publisher()
        self.assertEqual(self.github.writes, [])

    def test_corrupted_retained_artifact_prevents_any_upload(self):
        (self.directory / self.manifest["artifacts"][0]["filename"]).write_bytes(b"correction")
        with self.assertRaises(ValueError):
            self.publisher()
        self.assertEqual(self.github.writes, [])

    def test_branch_dispatch_cannot_claim_tag_provenance(self):
        environment = {"GITHUB_REPOSITORY": publish.REPOSITORY, "GITHUB_REF": "refs/heads/master", "GITHUB_SHA": SHA, "SDK_RELEASE_READY": "true", "GITHUB_EVENT_NAME": "workflow_dispatch", "GITHUB_WORKFLOW_REF": f"{publish.REPOSITORY}/.github/workflows/sdk-publish.yml@refs/heads/master"}
        with self.assertRaises(publish.Conflict):
            publish.check_execution(REF, SHA, environment)

    def python_registry(self, visible):
        records = {record["filename"]: record for record in self.manifest["artifacts"] if record["target"] == "python"}
        def fetch(url, **kwargs):
            if url.startswith("https://pypi.org/"):
                return publish.encoded({"urls": [
                    {"filename": name, "digests": {"sha256": records[name]["sha256"]}, "url": "https://files.pythonhosted.org/" + name}
                    for name in visible
                ]})
            if url.startswith("https://files.pythonhosted.org/"):
                return (self.directory / url.rsplit("/", 1)[1]).read_bytes()
            return None
        return patch.object(publish, "request", side_effect=fetch)

    def test_prepare_checks_public_bytes_without_recording_python_attempts(self):
        publisher = self.publisher()
        with self.python_registry(set()):
            publisher.prepare()
        self.assertEqual(self.github.retained, {})

    def test_python_public_checksum_conflict_prevents_upload_intent(self):
        publisher = self.publisher()
        wheel, = publisher.records("python", {"wheel"})
        metadata = {"urls": [{"filename": wheel["filename"], "digests": {"sha256": "f" * 64}, "url": "https://files.pythonhosted.org/wheel"}]}
        with patch.object(publish, "request", return_value=publish.encoded(metadata)), self.assertRaises(publish.Conflict):
            publisher.prepare_python("wheel")
        self.assertEqual(self.github.retained, {})

    def test_python_lost_response_never_replays_missing_distribution(self):
        publisher = self.publisher()
        uploads = []
        with self.python_registry(set()):
            if publisher.prepare_python("wheel"):
                uploads.append("wheel")  # Fake action accepted bytes, response lost; metadata remains stale.
            with self.assertRaises(publish.Unavailable):
                if self.publisher().prepare_python("wheel"):
                    uploads.append("wheel")
        self.assertEqual(uploads, ["wheel"])
        self.assertIsNone(publisher.state("python", "intent-sdist"))

    def test_python_partial_release_stages_only_unattempted_sdist(self):
        publisher = self.publisher()
        wheel, = publisher.records("python", {"wheel"})
        sdist, = publisher.records("python", {"sdist"})
        uploads = []
        with self.python_registry(set()):
            self.assertTrue(publisher.prepare_python("wheel"))
        with self.python_registry({wheel["filename"]}):
            resumed = self.publisher()
            if resumed.prepare_python("wheel"):
                uploads.append("wheel")
            if resumed.prepare_python("sdist"):
                uploads.append("sdist")
        self.assertEqual(uploads, ["sdist"])
        staged = self.directory / "python-sdist-upload"
        self.assertEqual({item.name for item in staged.iterdir()}, {sdist["filename"]})
        self.assertEqual((staged / sdist["filename"]).read_bytes(), (self.directory / sdist["filename"]).read_bytes())
        self.assertEqual(list((self.directory / "python-wheel-upload").iterdir()), [])

    def test_python_prior_intent_with_different_bytes_halts_even_when_public(self):
        publisher = self.publisher()
        wheel, = publisher.records("python", {"wheel"})
        publisher.progress("python", "intent-wheel", artifact={**wheel, "sha256": "f" * 64})
        with self.python_registry({record["filename"] for record in publisher.records("python")}):
            with self.assertRaises(publish.Conflict):
                publisher.prepare_python("wheel")

    def test_python_both_public_complete_without_upload(self):
        publisher = self.publisher()
        with self.python_registry(set()):
            publisher.prepare_python("wheel")
            publisher.prepare_python("sdist")
        uploads = []
        with self.python_registry({record["filename"] for record in publisher.records("python")}):
            for kind in ("wheel", "sdist"):
                if self.publisher().prepare_python(kind):
                    uploads.append(kind)
        self.assertEqual(uploads, [])
        self.assertIsNotNone(publisher.state("python", "available"))

    def test_python_legacy_blanket_intent_cannot_authorize_reupload(self):
        publisher = self.publisher()
        publisher.progress("python", "intent")
        with self.python_registry(set()), self.assertRaises(publish.Unavailable):
            publisher.prepare_python("wheel")

    def test_existing_npm_checksum_conflict_stops_before_python_intent(self):
        publisher = self.publisher()
        metadata = {"dist": {"tarball": "https://registry.npmjs.org/artifact"}}
        def fetch(url, **kwargs):
            return b"other public bytes" if url.endswith("/artifact") else publish.encoded(metadata)
        with patch.object(publish, "request", side_effect=fetch), self.assertRaises(publish.Conflict):
            publisher.prepare()
        self.assertEqual(self.github.retained, {})

    def test_ambiguous_central_upload_is_not_replayed(self):
        publisher = self.publisher()
        with patch.object(publisher, "java_available", return_value=False), patch.object(publisher, "central_headers", return_value={}), patch.object(publish, "request", side_effect=publish.Unavailable("response lost")) as transport:
            with self.assertRaises(publish.Unavailable):
                publisher.publish_java()
            with self.assertRaises(publish.Unavailable):
                publisher.publish_java()
        self.assertEqual(transport.call_count, 1)
        self.assertIsNotNone(publisher.state("java", "intent"))
        self.assertIsNone(publisher.state("java", "deployment"))

    def test_central_recovery_polls_retained_deployment_without_upload(self):
        publisher = self.publisher()
        deployment = "12345678-1234-1234-1234-123456789012"
        publisher.progress("java", "intent")
        publisher.progress("java", "deployment", deployment_id=deployment)
        response = {"deploymentId": deployment, "deploymentName": publisher.central_name(), "deploymentState": "PUBLISHED"}
        with patch.object(publisher, "java_available", return_value=False), patch.object(publisher, "central_headers", return_value={}), patch.dict(os.environ, {}, clear=True), patch.object(publish, "request", return_value=publish.encoded(response)) as transport:
            publisher.publish_java()
        self.assertEqual(transport.call_args.args[0], publish.CENTRAL + "/status?id=" + deployment)
        self.assertEqual(transport.call_args.kwargs["method"], "POST")
        self.assertEqual(publisher.state("java", "uploaded")["deployment_id"], deployment)

    def test_partial_publication_cannot_undraft_or_relabel(self):
        publisher = self.publisher()
        with patch.object(publisher, "verify", side_effect=publish.Unavailable("Central indexing delayed")), self.assertRaises(publish.Unavailable):
            publisher.finalize()
        self.assertEqual(self.github.writes, [])
        self.assertNotIn("sdk-release-complete.json", self.github.retained)

    def test_existing_go_tag_cannot_move_to_corrected_source(self):
        publisher = self.publisher()
        self.github.tags["sdk/go/v1.202609.1"] = "b" * 40
        with self.assertRaises(publish.Conflict):
            publisher.publish_go()
        self.assertEqual(self.github.tags["sdk/go/v1.202609.1"], "b" * 40)
        self.assertEqual(self.github.writes, [])

    def test_npm_provenance_rejects_current_master_instead_of_tag_sha(self):
        publisher = self.publisher()
        record, = publisher.records("typescript")
        statement = {"subject": [{"digest": {"sha512": hashlib.sha512((self.directory / record["filename"]).read_bytes()).hexdigest()}}], "predicate": {"buildDefinition": {"externalParameters": {"workflow": {"path": ".github/workflows/sdk-publish.yml", "ref": REF, "repository": "https://github.com/" + publish.REPOSITORY}}, "resolvedDependencies": [{"uri": f"git+https://github.com/{publish.REPOSITORY}@{REF}", "digest": {"gitCommit": "b" * 40}}]}}}
        attestation = {"attestations": [{"bundle": {"dsseEnvelope": {"payload": base64.b64encode(publish.encoded(statement)).decode()}}}]}
        metadata = {"dist": {"attestations": {"url": "https://registry.npmjs.org/attestations"}}}
        with patch.object(publish, "request", return_value=publish.encoded(attestation)), self.assertRaises(publish.Conflict):
            publisher.provenance(metadata)


if __name__ == "__main__":
    unittest.main()
