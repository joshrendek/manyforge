"""Publish only retained, tag-bound SDK artifacts; never build or regenerate packages."""
from __future__ import annotations

import argparse
import base64
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time
from urllib.error import HTTPError, URLError
from urllib.parse import quote, urlencode, urlparse
from urllib.request import HTTPRedirectHandler, Request, build_opener
import uuid

from release_manifest import validate_manifest

REPOSITORY = "joshrendek/manyforge"
TARGETS = ("python", "typescript", "java", "go")
API = "https://api.github.com"
CENTRAL = "https://central.sonatype.com/api/v1/publisher"
MODULE = "github.com/joshrendek/manyforge/sdk/go"


class Conflict(RuntimeError):
    """An immutable identity or published byte mismatch; never retry as an upload."""


class Unavailable(RuntimeError):
    """An availability check may be repeated without republishing."""


class SafeRedirect(HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, msg, headers, newurl):
        if urlparse(newurl).scheme != "https":
            raise Conflict("Refusing non-HTTPS redirect")
        if request.get_method() not in ("GET", "HEAD"):
            raise Conflict("Refusing redirect of a publication request")
        redirected = super().redirect_request(request, fp, code, msg, headers, newurl)
        if redirected and urlparse(request.full_url).netloc != urlparse(newurl).netloc:
            redirected.remove_header("Authorization")
        return redirected


def request(url, *, method="GET", headers=None, data=None, missing=False):
    if urlparse(url).scheme != "https":
        raise Conflict("Only HTTPS registry and GitHub requests are permitted")
    try:
        with build_opener(SafeRedirect()).open(Request(url, data=data, headers=headers or {}, method=method), timeout=120) as response:
            return response.read()
    except HTTPError as error:
        if missing and error.code == 404:
            return None
        # Do not expose headers, credentials, bodies or a command containing credentials.
        if error.code in (404, 429, 500, 502, 503, 504):
            raise Unavailable(f"{urlparse(url).hostname} returned HTTP {error.code}") from None
        raise RuntimeError(f"{urlparse(url).hostname} returned HTTP {error.code}") from None
    except (URLError, TimeoutError, OSError):
        raise Unavailable(f"{urlparse(url).hostname} request outcome unavailable") from None


def encoded(value):
    return json.dumps(value, sort_keys=True, indent=2).encode() + b"\n"


def digest(data):
    return hashlib.sha256(data).hexdigest()


def command(argv, cwd, environment=None):
    result = subprocess.run(argv, cwd=cwd, env=environment, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if result.returncode:
        raise RuntimeError(f"{Path(argv[0]).name} failed (exit {result.returncode}); output withheld to protect credentials")
    return result.stdout


def tag_name(ref):
    if not ref.startswith("refs/tags/sdk-v"):
        raise Conflict("Recovery requires the original refs/tags/sdk-vYYYY.M.N ref, not a branch")
    return ref.removeprefix("refs/tags/")


def check_execution(ref, source_sha, environment):
    tag_name(ref)
    if environment.get("GITHUB_REPOSITORY") != REPOSITORY:
        raise Conflict("Publication is restricted to the approved repository")
    if environment.get("GITHUB_REF") != ref or environment.get("GITHUB_SHA") != source_sha:
        raise Conflict("Run the original tag workflow; checkout overrides do not bind OIDC provenance")
    expected_workflow = f"{REPOSITORY}/.github/workflows/sdk-publish.yml@{ref}"
    if environment.get("GITHUB_WORKFLOW_REF") != expected_workflow:
        raise Conflict("Publishing identity must be sdk-publish.yml at the original tag ref")
    if environment.get("GITHUB_EVENT_NAME") not in ("push", "workflow_dispatch"):
        raise Conflict("Unsupported publication event")
    if environment.get("SDK_RELEASE_READY") != "true":
        raise RuntimeError("Publication blocked: maintainer must verify exact PyPI/npm/Central ownership, configure sdk-release trusted publishers (including direct npm publish), GitHub App and Central secrets, then attest SDK_RELEASE_READY=true")


class GitHub:
    def __init__(self, token):
        if not token:
            raise RuntimeError("Publication blocked: GitHub App token is unavailable")
        self.headers = {"Authorization": f"Bearer {token}", "Accept": "application/vnd.github+json", "Content-Type": "application/json", "X-GitHub-Api-Version": "2022-11-28"}

    def api(self, path, *, method="GET", value=None, missing=False):
        data = request(f"{API}/repos/{REPOSITORY}/{path}", method=method, headers=self.headers, data=encoded(value) if value is not None else None, missing=missing)
        return json.loads(data) if data else None

    def pages(self, path):
        values = []
        for page in range(1, 10001):
            batch = self.api(f"{path}{'&' if '?' in path else '?'}per_page=100&page={page}")
            values.extend(batch)
            if len(batch) < 100:
                return values
        raise RuntimeError("GitHub pagination exceeded safety bound")

    def release(self, tag):
        matches = [value for value in self.pages("releases") if value["tag_name"] == tag]
        if len(matches) != 1:
            raise Conflict("Exactly one retained draft or completed release is required")
        return matches[0]

    def assets(self, release):
        assets = self.pages(f"releases/{release['id']}/assets")
        if len({asset['name'] for asset in assets}) != len(assets):
            raise Conflict("Duplicate retained asset names")
        return {asset["name"]: asset for asset in assets}

    def download(self, asset):
        return request(f"{API}/repos/{REPOSITORY}/releases/assets/{asset['id']}", headers={**self.headers, "Accept": "application/octet-stream"})

    def immutable(self, release, name, value):
        data = encoded(value)
        existing = self.assets(release).get(name)
        if existing:
            if self.download(existing) != data:
                raise Conflict(f"Retained progress conflicts: {name}")
            return
        request(f"https://uploads.github.com/repos/{REPOSITORY}/releases/{release['id']}/assets?{urlencode({'name': name})}", method="POST", headers={**self.headers, "Content-Type": "application/json"}, data=data)

    def tag(self, tag, sha, create=False):
        value = self.api(f"git/ref/tags/{quote(tag, safe='/')}", missing=True)
        if value:
            if value["object"]["type"] != "commit" or value["object"]["sha"] != sha:
                raise Conflict(f"Immutable lightweight tag differs: {tag}")
            return True
        if create:
            self.api("git/refs", method="POST", value={"ref": f"refs/tags/{tag}", "sha": sha})
            return self.tag(tag, sha)
        return False


class Publisher:
    def __init__(self, github, manifest, directory, release, ref, source_sha):
        validate_manifest(manifest, directory=directory, tag=tag_name(ref), source_sha=source_sha, publishing=True)
        self.github, self.manifest, self.directory, self.release = github, manifest, directory, release
        self.ref = ref
        self.identity = {"version": manifest["version"], "source_sha": manifest["source_sha"], "manifest_sha256": digest((directory / "sdk-manifest.json").read_bytes())}
        if "sdk-release-superseded.json" in github.assets(release) or "INCOMPLETE / SUPERSEDED" in (release.get("body") or ""):
            raise Conflict("Candidate was superseded; a corrected source requires its new reviewed version")
        if not github.tag(tag_name(ref), source_sha):
            raise Conflict("Original umbrella tag does not exist")
        pr = github.api(f"pulls/{manifest['release_pr']}")
        if not pr.get("merged") or pr.get("merge_commit_sha") != source_sha or pr["base"]["ref"] != "master":
            raise Conflict("Manifest must identify the reviewed, merged release PR revision")
        for verified in manifest["verification_runs"]:
            run = github.api(f"actions/runs/{verified['id']}")
            if run.get("head_sha") != source_sha or run.get("conclusion") != "success" or run.get("repository", {}).get("full_name") != REPOSITORY:
                raise Conflict("Recorded verification run does not verify the candidate SHA")

    @classmethod
    def download(cls, github, directory, ref, source_sha):
        release = github.release(tag_name(ref))
        assets = github.assets(release)
        if "sdk-manifest.json" not in assets:
            raise Conflict("Retained immutable manifest missing; never rebuild in publication")
        raw = github.download(assets["sdk-manifest.json"])
        manifest = json.loads(raw)
        validate_manifest(manifest, tag=tag_name(ref), source_sha=source_sha, publishing=True)
        directory.mkdir(parents=True, exist_ok=True)
        (directory / "sdk-manifest.json").write_bytes(raw)
        for record in manifest["artifacts"]:
            if record["filename"] not in assets:
                raise Conflict(f"Retained artifact missing: {record['filename']}")
            (directory / record["filename"]).write_bytes(github.download(assets[record["filename"]]))
        return cls(github, manifest, directory, release, ref, source_sha)

    def records(self, target, kinds=None):
        return [record for record in self.manifest["artifacts"] if record["target"] == target and (kinds is None or record["kind"] in kinds)]

    def progress(self, target, phase, **details):
        value = {**self.identity, "target": target, "phase": phase, **details}
        self.github.immutable(self.release, f"sdk-progress-{target}-{phase}.json", value)

    def state(self, target, phase):
        asset = self.github.assets(self.release).get(f"sdk-progress-{target}-{phase}.json")
        if not asset:
            return None
        value = json.loads(self.github.download(asset))
        if any(value.get(key) != expected for key, expected in self.identity.items()) or value.get("target") != target or value.get("phase") != phase:
            raise Conflict("Progress belongs to different immutable candidate")
        return value

    def compare(self, record, data):
        if len(data) != record["size"] or digest(data) != record["sha256"]:
            raise Conflict(f"Published checksum conflicts: {record['filename']}; publish corrections under a new reviewed version")

    def python_missing(self):
        version = self.manifest["version"]
        body = request(f"https://pypi.org/pypi/manyforge/{version}/json", missing=True)
        urls = json.loads(body)["urls"] if body else []
        expected = {record["filename"]: record for record in self.records("python")}
        if any(item["filename"] not in expected for item in urls):
            raise Conflict("PyPI version contains unexpected distribution files")
        found = set()
        for item in urls:
            record = expected[item["filename"]]
            if item["digests"]["sha256"] != record["sha256"]:
                raise Conflict("PyPI immutable digest mismatch")
            if urlparse(item["url"]).hostname != "files.pythonhosted.org":
                raise Conflict("Unexpected PyPI artifact origin")
            self.compare(record, request(item["url"]))
            found.add(item["filename"])
        return [record for name, record in expected.items() if name not in found]

    def npm_metadata(self):
        raw = request(f"https://registry.npmjs.org/@manyforge%2Fsdk/{self.manifest['version']}", missing=True)
        if raw is None:
            return None
        metadata = json.loads(raw)
        url = metadata["dist"]["tarball"]
        if urlparse(url).hostname != "registry.npmjs.org":
            raise Conflict("Unexpected npm tarball origin")
        record, = self.records("typescript", {"npm"})
        # npm's legacy dist metadata can spell an HTTP URL; fetch only HTTPS.
        self.compare(record, request(urlparse(url)._replace(scheme="https").geturl()))
        return metadata

    def java_available(self):
        present = []
        for record in self.records("java", {"jar", "pom", "sources", "javadoc"}):
            data = request(f"https://repo.maven.apache.org/maven2/com/manyforge/manyforge-sdk/{self.manifest['version']}/{record['filename']}", missing=True)
            present.append(data is not None)
            if data is not None:
                self.compare(record, data)
        return all(present)

    def python_intents(self):
        intents = {}
        for record in self.records("python"):
            prior = self.state("python", "intent-" + record["kind"])
            if prior is not None and prior.get("artifact") != record:
                raise Conflict(f"PyPI attempt identifies different retained bytes: {record['filename']}")
            intents[record["kind"]] = prior
        # Old blanket intents cannot prove which distribution was attempted.
        # Retained historical state remains conservative, never an upload permit.
        return intents, self.state("python", "intent")

    def prepare(self):
        # Find any existing immutable conflict across targets before the first upload.
        self.npm_metadata()
        self.java_available()
        self.github.tag("sdk/go/" + self.manifest["go_module_version"], self.manifest["source_sha"])
        self.python_intents()
        self.python_missing()

    def prepare_python(self, kind):
        if kind not in ("wheel", "sdist"):
            raise ValueError("PyPI preparation requires one wheel or sdist")
        destination = self.directory / f"python-{kind}-upload"
        if destination.exists():
            shutil.rmtree(destination)
        destination.mkdir()
        intents, legacy_intent = self.python_intents()
        missing = self.python_missing()
        if not missing:
            self.progress("python", "available")
        record, = self.records("python", {kind})
        if record not in missing:
            return False
        if intents[kind] is not None or legacy_intent is not None:
            raise Unavailable(f"PyPI {kind} upload outcome is ambiguous: wait for public visibility or reconcile the original attempt; missing registry metadata is not proof of no upload")
        shutil.copyfile(self.directory / record["filename"], destination / record["filename"])
        # The workflow invokes exactly one OIDC action immediately after this
        # durable intent. A failed/cancelled action never marks the other file.
        self.progress("python", "intent-" + kind, artifact=record)
        return True

    def publish_npm(self):
        if self.npm_metadata() is not None:
            self.progress("typescript", "available")
            return
        if self.state("typescript", "intent"):
            raise Unavailable("npm upload was already attempted; wait for visibility or reconcile the failed original run, never blindly republish")
        environment = {key: value for key, value in os.environ.items() if not key.upper().startswith("NPM_CONFIG_")}
        for key in ("NODE_AUTH_TOKEN", "NPM_TOKEN", "NPM_AUTH_TOKEN", "NPM_ID_TOKEN"):
            environment.pop(key, None)
        bootstrap = environment.pop("SDK_NPM_BOOTSTRAP_TOKEN", "")
        authorization = environment.pop("SDK_NPM_BOOTSTRAP_TAG", "")
        if bootstrap:
            if authorization != self.ref:
                raise Conflict("Bootstrap credential requires explicit maintainer authorization for this original tag")
            package = request("https://registry.npmjs.org/@manyforge%2Fsdk", missing=True)
            if package is not None:
                raise Conflict("Bootstrap is allowed only for first package creation; configure OIDC and revoke the credential")
            prior = [item for item in self.github.pages("git/matching-refs/tags/sdk-v") if item["ref"] != self.ref]
            if prior:
                raise Conflict("Bootstrap is restricted to the first SDK tag")
        elif not environment.get("ACTIONS_ID_TOKEN_REQUEST_URL") or not environment.get("ACTIONS_ID_TOKEN_REQUEST_TOKEN"):
            raise RuntimeError("npm trusted publishing requires GitHub-hosted OIDC; no token fallback")
        node = command(["node", "--version"], self.directory).strip()
        npm = command(["npm", "--version"], self.directory).strip()
        if not node.startswith("v24.") or tuple(int(part) for part in npm.split(".")) < (11, 5, 1):
            raise RuntimeError("Publication requires Node 24 and npm >=11.5.1")
        record, = self.records("typescript", {"npm"})
        with tempfile.TemporaryDirectory(prefix="sdk-npm-publish-") as temporary:
            stage = Path(temporary)
            config = stage / "npmrc"
            config.write_text("registry=https://registry.npmjs.org/\n" + ("//registry.npmjs.org/:_authToken=${NODE_AUTH_TOKEN}\n" if bootstrap else ""))
            config.chmod(0o600)
            environment["NPM_CONFIG_USERCONFIG"] = str(config)
            environment["NPM_CONFIG_GLOBALCONFIG"] = str(stage / "global-npmrc")
            if bootstrap:
                environment["NODE_AUTH_TOKEN"] = bootstrap
                # This credential is admitted only by the explicit first-tag gate
                # above. Ordinary runs contain no token for npm to fall back to.
            self.progress("typescript", "intent", authentication="bootstrap" if bootstrap else "oidc")
            command(["npm", "publish", str(self.directory / record["filename"]), "--access", "public", "--provenance", "--tag", "latest", "--ignore-scripts", "--fetch-retries", "0", "--registry", "https://registry.npmjs.org/"], stage, environment)
            self.progress("typescript", "uploaded")

    def central_headers(self):
        username = os.environ.get("MAVEN_CENTRAL_USERNAME")
        password = os.environ.get("MAVEN_CENTRAL_PASSWORD")
        if not username or not password:
            raise RuntimeError("Publication blocked: Central Portal user-token credentials unavailable")
        token = base64.b64encode(f"{username}:{password}".encode()).decode()
        return {"Authorization": f"Bearer {token}"}

    def publish_java(self):
        if self.java_available():
            self.progress("java", "available")
            return
        headers = self.central_headers()
        recovery = os.environ.get("SDK_CENTRAL_RECOVERY_DEPLOYMENT_ID")
        if recovery:
            self.record_central(recovery)
        deployed = self.state("java", "deployment")
        record, = self.records("java", {"central-bundle"})
        if not deployed:
            if self.state("java", "intent"):
                raise Unavailable("Central upload outcome is ambiguous: recover its deployment ID from the original run/Portal with record-central-deployment; do not upload again")
            boundary = "manyforge-" + uuid.uuid4().hex
            body = (f'--{boundary}\r\nContent-Disposition: form-data; name="bundle"; filename="{record["filename"]}"\r\nContent-Type: application/octet-stream\r\n\r\n'.encode() + (self.directory / record["filename"]).read_bytes() + f"\r\n--{boundary}--\r\n".encode())
            self.progress("java", "intent")
            deployment = request(f"{CENTRAL}/upload?{urlencode({'publishingType': 'AUTOMATIC', 'name': self.central_name()})}", method="POST", headers={**headers, "Content-Type": f"multipart/form-data; boundary={boundary}"}, data=body).decode().strip()
            deployment = str(uuid.UUID(deployment))
            # This non-secret recovery receipt survives failure to persist the asset.
            print(f"Central deployment ID: {deployment}", flush=True)
            self.progress("java", "deployment", deployment_id=deployment)
            deployed = {"deployment_id": deployment}
        self.poll_central(deployed["deployment_id"])

    def central_name(self):
        return f"manyforge-sdk-{self.manifest['version']}-{self.manifest['source_sha']}"

    def central_status(self, deployment):
        deployment = str(uuid.UUID(deployment))
        value = json.loads(request(f"{CENTRAL}/status?{urlencode({'id': deployment})}", method="POST", headers=self.central_headers(), data=b""))
        if value.get("deploymentId") != deployment or value.get("deploymentName") != self.central_name():
            raise Conflict("Central deployment identity differs from retained candidate")
        return value

    def record_central(self, deployment):
        if not self.state("java", "intent"):
            raise Conflict("No original Central upload intent exists")
        self.central_status(deployment)
        self.progress("java", "deployment", deployment_id=str(uuid.UUID(deployment)))

    def poll_central(self, deployment):
        for attempt in range(60):
            state = self.central_status(deployment).get("deploymentState")
            if state == "PUBLISHED":
                self.progress("java", "uploaded", deployment_id=deployment)
                return
            if state == "FAILED":
                raise Conflict("Central validation failed; retained deployment must be reviewed, never automatically reuploaded")
            if state not in ("PENDING", "VALIDATING", "VALIDATED", "PUBLISHING"):
                raise Conflict("Unknown Central deployment state")
            time.sleep(30)
        raise Unavailable("Central deployment remains pending; resume polling its retained ID")

    def publish_go(self):
        tag = "sdk/go/" + self.manifest["go_module_version"]
        self.progress("go", "intent")
        self.github.tag(tag, self.manifest["source_sha"], create=True)
        self.progress("go", "uploaded")

    def public_environment(self, home):
        allowed = ("PATH", "JAVA_HOME", "GOROOT", "SSL_CERT_FILE", "SSL_CERT_DIR", "SYSTEMROOT")
        environment = {key: os.environ[key] for key in allowed if key in os.environ}
        environment.update({"HOME": str(home), "PIP_CONFIG_FILE": os.devnull, "PIP_NO_CACHE_DIR": "1", "GOWORK": "off", "GOPROXY": "https://proxy.golang.org", "GOSUMDB": "sum.golang.org", "GOPRIVATE": "", "GONOSUMDB": "", "GONOPROXY": "", "GOENV": "off", "GOMODCACHE": str(home / "gomodcache"), "GOCACHE": str(home / "gocache"), "NPM_CONFIG_USERCONFIG": str(home / "npmrc"), "NPM_CONFIG_GLOBALCONFIG": str(home / "global-npmrc"), "NPM_CONFIG_CACHE": str(home / "npm-cache"), "NPM_CONFIG_REGISTRY": "https://registry.npmjs.org/"})
        return environment

    def provenance(self, metadata):
        url = metadata.get("dist", {}).get("attestations", {}).get("url")
        if not url or urlparse(url).hostname != "registry.npmjs.org":
            raise Unavailable("npm provenance is not yet visible")
        attestations = json.loads(request(url)).get("attestations", [])
        record, = self.records("typescript", {"npm"})
        sha512 = hashlib.sha512((self.directory / record["filename"]).read_bytes()).hexdigest()
        for attestation in attestations:
            envelope = attestation.get("bundle", {}).get("dsseEnvelope", {})
            if not envelope.get("payload"):
                continue
            statement = json.loads(base64.b64decode(envelope["payload"]))
            if not any(subject.get("digest", {}).get("sha512") == sha512 for subject in statement.get("subject", [])):
                continue
            predicate = statement.get("predicate", {})
            definition = predicate.get("buildDefinition", {})
            dependencies = definition.get("resolvedDependencies", [])
            old_source = predicate.get("invocation", {}).get("configSource", {})
            sources = dependencies + ([old_source] if old_source else [])
            expected_uri = f"git+https://github.com/{REPOSITORY}@{self.ref}"
            source_ok = any(source.get("uri") == expected_uri and self.manifest["source_sha"] in (source.get("digest", {}).get("gitCommit"), source.get("digest", {}).get("sha1")) for source in sources)
            workflow = definition.get("externalParameters", {}).get("workflow", {})
            workflow_ok = workflow.get("path") == ".github/workflows/sdk-publish.yml" and workflow.get("ref") == self.ref and workflow.get("repository") == f"https://github.com/{REPOSITORY}"
            if not workflow:
                workflow_ok = old_source.get("entryPoint") == ".github/workflows/sdk-publish.yml"
            if source_ok and workflow_ok:
                return
        raise Conflict("npm provenance does not bind the retained tarball to the original SDK tag/source/workflow")

    def verify(self):
        # Read-only registry availability retries are deliberately separate from upload.
        if self.python_missing():
            raise Unavailable("PyPI distributions are not all visible")
        npm = self.npm_metadata()
        if npm is None or not self.java_available():
            raise Unavailable("npm/Central artifacts are not all indexed")
        self.provenance(npm)
        if not self.github.tag("sdk/go/" + self.manifest["go_module_version"], self.manifest["source_sha"]):
            raise Unavailable("Go module tag missing")
        with tempfile.TemporaryDirectory(prefix="sdk-public-consumers-") as temporary:
            home = Path(temporary)
            environment = self.public_environment(home)
            version = self.manifest["version"]
            command([sys.executable, "-m", "venv", str(home / "venv")], home, environment)
            python = str(home / "venv/bin/python")
            command([python, "-m", "pip", "install", "--index-url", "https://pypi.org/simple", f"manyforge=={version}"], home, environment)
            command([python, "-c", "from manyforge import ManyForge, AsyncManyForge"], home, environment)
            (home / "package.json").write_text('{"private":true}')
            command(["npm", "install", "--save-exact", "--ignore-scripts", f"@manyforge/sdk@{version}"], home, environment)
            # npm verifies registry signatures AND Sigstore provenance cryptographically.
            command(["npm", "audit", "signatures"], home, environment)
            command(["node", "--input-type=module", "-e", "import {ManyForge} from '@manyforge/sdk'; import {FeedbackClient} from '@manyforge/sdk/public'; if (!ManyForge || !FeedbackClient) process.exit(1)"], home, environment)
            command(["node", "-e", "const {ManyForge}=require('@manyforge/sdk'); if (!ManyForge) process.exit(1)"], home, environment)
            module_version = self.manifest["go_module_version"]
            resolved = json.loads(command(["go", "mod", "download", "-json", f"{MODULE}@{module_version}"], home, environment))
            if resolved.get("Sum") != self.manifest["go_module_sum"]:
                raise Conflict("Go proxy module h1 differs from the tested canonical module zip")
            command(["go", "mod", "init", "example.com/sdk-release-consumer"], home, environment)
            command(["go", "get", f"{MODULE}@{module_version}"], home, environment)
            (home / "main.go").write_text(f'package main\nimport manyforge "{MODULE}"\nfunc main() {{ _, err := manyforge.NewClient("https://example.invalid"); if err != nil {{ panic(err) }} }}\n')
            command(["go", "run", "."], home, environment)
            repository = home / "maven-repository"
            (home / "pom.xml").write_text(f'<project xmlns="http://maven.apache.org/POM/4.0.0"><modelVersion>4.0.0</modelVersion><groupId>smoke</groupId><artifactId>consumer</artifactId><version>1</version><dependencies><dependency><groupId>com.manyforge</groupId><artifactId>manyforge-sdk</artifactId><version>{version}</version></dependency></dependencies></project>')
            settings = home / "settings.xml"
            settings.write_text('<settings xmlns="http://maven.apache.org/SETTINGS/1.2.0"><mirrors><mirror><id>public-central</id><mirrorOf>*</mirrorOf><url>https://repo.maven.apache.org/maven2</url></mirror></mirrors></settings>')
            command(["mvn", "-B", "-s", str(settings), "-gs", str(settings), f"-Dmaven.repo.local={repository}", "org.apache.maven.plugins:maven-dependency-plugin:3.8.1:build-classpath", "-Dmdep.outputFile=classpath.txt"], home, environment)
            classpath = (home / "classpath.txt").read_text().strip()
            (home / "Consumer.java").write_text('import com.manyforge.sdk.ManyForgeClient; public class Consumer { public static void main(String[] args) { try (var client = ManyForgeClient.builder().baseUrl("https://example.invalid").build()) { if (client.auth() == null) throw new AssertionError(); } } }')
            command(["javac", "--release", "17", "-cp", classpath, "Consumer.java"], home, environment)
            command(["java", "-cp", str(home) + os.pathsep + classpath, "Consumer"], home, environment)
        for target in TARGETS:
            self.progress(target, "verified")

    def finalize(self):
        self.verify()
        for target in TARGETS:
            if not self.state(target, "verified"):
                raise Conflict("Cannot complete while a target lacks public consumer verification")
        complete_name = "sdk-release-complete.json"
        asset = self.github.assets(self.release).get(complete_name)
        if asset:
            complete = json.loads(self.github.download(asset))
            if any(complete.get(key) != expected for key, expected in self.identity.items()):
                raise Conflict("Completed release identity differs")
        else:
            self.github.immutable(self.release, complete_name, {**self.identity, "status": "complete", "published_at": datetime.now(timezone.utc).isoformat(), "targets": list(TARGETS)})
        self.github.api(f"releases/{self.release['id']}", method="PATCH", value={"draft": False})
        issue = f"issues/{self.manifest['release_pr']}"
        self.github.api(f"{issue}/labels", method="POST", value={"labels": ["autorelease: tagged"]})
        labels = self.github.api(f"{issue}/labels")
        if any(label["name"] == "autorelease: pending" for label in labels):
            self.github.api(f"{issue}/labels/{quote('autorelease: pending', safe='')}", method="DELETE")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("prepare", "prepare-python", "publish", "verify", "finalize", "record-central-deployment"))
    parser.add_argument("--tag", required=True)
    parser.add_argument("--source-sha", required=True)
    parser.add_argument("--directory", type=Path, required=True)
    parser.add_argument("--target", choices=("typescript", "java", "go"))
    parser.add_argument("--distribution", choices=("wheel", "sdist"))
    parser.add_argument("--deployment-id")
    args = parser.parse_args()
    check_execution(args.tag, args.source_sha, os.environ)
    github = GitHub(os.environ.get("GH_TOKEN"))
    directory = args.directory.resolve()
    if args.action == "prepare":
        publisher = Publisher.download(github, directory, args.tag, args.source_sha)
        publisher.prepare()
        return
    manifest = json.loads((directory / "sdk-manifest.json").read_text())
    publisher = Publisher(github, manifest, directory, github.release(tag_name(args.tag)), args.tag, args.source_sha)
    if args.action == "prepare-python":
        if not args.distribution:
            parser.error("prepare-python requires --distribution")
        missing = publisher.prepare_python(args.distribution)
        if os.environ.get("GITHUB_OUTPUT"):
            with open(os.environ["GITHUB_OUTPUT"], "a") as output:
                output.write(f"python_missing={str(missing).lower()}\n")
    elif args.action == "publish":
        if not args.target:
            parser.error("publish requires --target")
        {"typescript": publisher.publish_npm, "java": publisher.publish_java, "go": publisher.publish_go}[args.target]()
    elif args.action == "record-central-deployment":
        if not args.deployment_id:
            parser.error("record-central-deployment requires --deployment-id")
        publisher.record_central(args.deployment_id)
    else:
        for attempt in range(20):
            try:
                getattr(publisher, args.action)()
                return
            except Unavailable:
                if attempt == 19:
                    raise
                time.sleep(30)


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, ValueError) as error:
        print(f"SDK release remains incomplete: {error}", file=sys.stderr)
        raise SystemExit(1)
