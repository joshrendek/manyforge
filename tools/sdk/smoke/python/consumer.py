"""Installed-package consumer: real product operations plus transport boundary checks."""
from __future__ import annotations

import asyncio
import csv
import hashlib
import hmac
import io
import json
import os
import sys
import threading
import time
from datetime import date, datetime, timedelta, timezone
from pathlib import Path
from uuid import UUID, uuid4

import httpx
from pydantic import TypeAdapter

from manyforge import (AnalyticsClient, AsyncManyForge, AsyncSession, FeedbackClient, ManyForge, ManyForgeError, ProtocolError, RequestOptions, Session, SessionError, TelemetryClient, TimeoutError, TransportError, Upload)
from manyforge.http import AsyncHTTPTransport, HTTPTransport
from manyforge.models.analytics_collect_request import AnalyticsCollectRequest
from manyforge.models.analytics_day_point import AnalyticsDayPoint
from manyforge.models.board_create import BoardCreate
from manyforge.models.business_create_request import BusinessCreateRequest
from manyforge.models.confirm_tenant_merge import ConfirmTenantMerge
from manyforge.models.crash_event import CrashEvent
from manyforge.models.create_contact import CreateContact
from manyforge.models.create_tenant_merge import CreateTenantMerge
from manyforge.models.ingest_key_create import IngestKeyCreate
from manyforge.models.list_input import ListInput
from manyforge.models.patch_ticket import PatchTicket
from manyforge.models.public_submit import PublicSubmit
from manyforge.models.public_submit_result import PublicSubmitResult
from manyforge.models.public_vote import PublicVote
from manyforge.models.telemetry_client_create import TelemetryClientCreate
from manyforge.models.telemetry_ingest_request import TelemetryIngestRequest
from manyforge.models.update_contact import UpdateContact
from manyforge.models.token_pair import TokenPair
from unittest.mock import patch
from manyforge.server import MailingServerClient, SignedFeedbackClient, SignedTelemetryClient
from manyforge.transport import QueryParameter, Request


def expect_api(call, status, code=None):
    try:
        call()
    except ManyForgeError as error:
        assert error.status == status, (error.status, status, error.code)
        if code is not None:
            assert error.code == code, (error.code, code)
        return error
    raise AssertionError(f"Expected HTTP {status}")


def expect_error(call, kind):
    try:
        call()
    except kind as error:
        return error
    raise AssertionError(f"Expected {kind.__name__}")


def supplemental(assertions):
    captured = []

    def exchange(request):
        captured.append(request)
        if request.url.path == "/error":
            return httpx.Response(418, headers={"X-Request-Id": "test-request"}, json={"code": "FUTURE_SERVER_CODE", "message": "secret-data", "details": {"reason": "future"}})
        if request.url.path == "/large":
            return httpx.Response(502, content=b"private-token" + b"x" * 70000)
        if request.url.path == "/invalid":
            return httpx.Response(200, content=b"<html>not a model</html>")
        if request.url.path == "/timeout":
            raise httpx.ReadTimeout("sensitive request", request=request)
        if request.method == "POST":
            return httpx.Response(200, content=request.content)
        return httpx.Response(200, json={"id": str(uuid4()), "deduped": False, "identity_verified": False, "status": "future-status", "title": "future", "vote_count": 0, "future_property": "preserved"})

    with httpx.Client(transport=httpx.MockTransport(exchange), auth=("username", "password"), headers={"Authorization": "secret", "Cookie": "session=secret"}, cookies={"session": "secret"}, params={"inherited": "secret"}) as injected:
        with FeedbackClient(base_url="https://example.test/", publishable_key="test-key", http_client=injected) as public:
            result = public._transport.request(Request(operation_id="boundary", method="GET", path="/model", response=TypeAdapter(PublicSubmitResult), authenticated=False))
            assert result.status.value == "future-status" and result.vote_count == 0
            assert result.model_extra["future_property"] == "preserved"
        assert not injected.is_closed
        assert "authorization" not in captured[-1].headers and "cookie" not in captured[-1].headers
        assert not captured[-1].url.query
        transport = HTTPTransport("https://example.test", http_client=injected)
        body = {"false": False, "zero": 0, "empty": [], "counter": 9007199254740993, "null": None}
        descriptor = Request(operation_id="echo", method="POST", path="/echo/{id}", path_parameters={"id": "a/b%? #"}, query=(QueryParameter("q", "a&b +/%"), QueryParameter("tags", ["x y", "z"])), body=body, authenticated=False, response=TypeAdapter(dict))
        assert transport.request(descriptor, request_options=RequestOptions(timeout=4)) == body
        assert captured[-1].url.raw_path == b"/echo/a%2Fb%25%3F%20%23?q=a%26b%20%2B%2F%25&tags=x%20y&tags=z"
        assert captured[-1].extensions["timeout"]["read"] == 4
        patch_request = Request(operation_id="presence", method="POST", path="/echo", body=PatchTicket(priority="low", tags=[]), authenticated=False, response=TypeAdapter(dict))
        assert transport.request(patch_request) == {"priority": "low", "tags": []}
        day = AnalyticsDayPoint(var_date=date(2026, 9, 1), pageviews=9007199254740993, visitors=0)
        echoed = transport.request(Request(operation_id="date", method="POST", path="/echo", body=day, authenticated=False, response=TypeAdapter(AnalyticsDayPoint)))
        assert echoed.var_date == date(2026, 9, 1) and echoed.pageviews == 9007199254740993
        for path, error_type in (("/error", ManyForgeError), ("/large", ManyForgeError), ("/invalid", ProtocolError), ("/timeout", TimeoutError)):
            before = len(captured)
            error = expect_error(lambda: transport.request(Request(operation_id="error", method="GET", path=path, authenticated=False, response=TypeAdapter(dict))), error_type)
            assert len(captured) == before + 1
            assert "secret-data" not in str(error) and "private-token" not in repr(error) and "sensitive request" not in str(error)
            if path == "/error":
                assert error.code == "FUTURE_SERVER_CODE" and error.message == "secret-data" and error.request_id == "test-request"
                assert error.details["details"]["reason"] == "future"
            if path == "/large":
                assert len(error.raw_body) == 65536 and error.truncated
    for url in ("/", "https://u:p@example.test", "https://example.test/api/v1", "https://example.test/path", "https://example.test?", "https://example.test#", "https://example.test\\evil"):
        expect_error(lambda: ManyForge(base_url=url), ValueError)
    expect_error(lambda: ManyForge(base_url="https://example.test", access_token="x", token_provider=lambda: "y"), ValueError)
    assertions.append("transport-presence-int64-date-unknown-enum-errors-ownership-url-encoding")

    # Verify signatures against final bytes, including encoded punctuation and query order.
    signed_requests = []
    def verify(request):
        signed_requests.append(request)
        family = "mailing" if "x-mailing-signature" in request.headers else "feedback"
        if family == "mailing":
            timestamp = request.headers["x-mailing-timestamp"]
            supplied = request.headers["x-mailing-signature"]
            target = request.url.raw_path.split(b"?", 1)[0]
        else:
            timestamp, supplied = request.headers["x-feedback-signature"].removeprefix("t=").split(",v1=")
            target = request.url.raw_path
        data = timestamp.encode() + b"." + request.method.encode() + b"." + target + b"." + request.content
        expected = hmac.new(b"mfs_entire-issued-secret", data, hashlib.sha256).hexdigest()
        return httpx.Response(204 if hmac.compare_digest(expected, supplied) else 401)
    with patch("manyforge.http.time.time", return_value=1800000000), httpx.Client(transport=httpx.MockTransport(verify)) as injected:
        for cls in (SignedFeedbackClient, MailingServerClient):
            with cls(base_url="https://example.test", publishable_key="k/%", signing_secret="mfs_entire-issued-secret", http_client=injected) as signed:
                for query in ((QueryParameter("a", "!/?+&"), QueryParameter("b", "2")), (QueryParameter("b", "2"), QueryParameter("a", "!/?+&"))):
                    signed._transport.request(Request(operation_id="signing", method="POST", path="/signed/{key}", path_parameters={"key": "k/%"}, query=query, body=PublicSubmit(title="stable", body="data"), response=TypeAdapter(type(None)), authenticated=False))
        assert signed_requests[0].headers["x-feedback-signature"] != signed_requests[1].headers["x-feedback-signature"]
        assert signed_requests[2].headers["x-mailing-signature"] == signed_requests[3].headers["x-mailing-signature"]
        original = signed_requests[0]
        tampered_body = httpx.Request(original.method, original.url, headers=original.headers, content=original.content + b" ")
        assert injected.send(tampered_body).status_code == 401
        tampered_query = httpx.Request(original.method, signed_requests[1].url, headers=original.headers, content=original.content)
        assert injected.send(tampered_query).status_code == 401
    assertions.append("exact-target-signing-query-order-tamper-rejected")

    refreshes = []
    def refresh_exchange(request):
        refreshes.append(request)
        assert request.url.path == "/api/v1/auth/refresh"
        return httpx.Response(200, json={"access_token": "new-access", "refresh_token": "new-refresh", "expires_in": 300})
    def failed_persistence(pair):
        assert pair.refresh_token == "new-refresh"
        raise OSError("secret storage diagnostic")
    expired = TokenPair(access_token="old-access", refresh_token="old-refresh", expires_in=3)
    object.__setattr__(expired, "_sdk_received_at", time.monotonic() - 10)
    with httpx.Client(transport=httpx.MockTransport(refresh_exchange)) as injected:
        owner = Session.from_token_pair(expired, on_rotate=failed_persistence)
        with ManyForge(base_url="https://example.test", session=owner, http_client=injected) as client:
            failure = expect_error(lambda: client.account.get(), SessionError)
            expect_error(lambda: client.account.get(), SessionError)
            assert "secret storage diagnostic" not in str(failure)
        assert len(refreshes) == 1
    assertions.append("persistence-failure-invalidates-owner-before-business-request")


async def asynchronous_uploads(assertions):
    payload = b"email,name\r\n" + b"reader@example.invalid,Streaming\r\n" * 8192
    filename = 'contacts "quoted"\\name.csv'
    business_id, list_id = uuid4(), uuid4()

    class SlowReader(io.BytesIO):
        def __init__(self, stage):
            super().__init__(payload)
            self.stage = stage
            self.blocked = threading.Event()
            self.release = threading.Event()
            self.returned = threading.Event()
            self.rewinds = 0
            self.read_sizes = []
            self.threads = []

        def pause(self, stage):
            if self.stage == stage and not self.blocked.is_set():
                self.blocked.set()
                self.release.wait(5)
                self.returned.set()

        def seek(self, offset, whence=0):
            if whence == 0:
                self.rewinds += 1
            if whence == 2:
                self.pause("probe")
            elif self.rewinds == 2:
                self.pause("rewind")
            return super().seek(offset, whence)

        def read(self, size=-1):
            self.threads.append(threading.get_ident())
            self.read_sizes.append(size)
            self.pause("read")
            return super().read(size)

    class Capture(httpx.AsyncBaseTransport):
        def __init__(self, reader):
            self.reader = reader
            self.requests = []
            self.chunks = []
            self.completed = False
            self.first_data = asyncio.Event()
            self.resume = asyncio.Event()

        async def handle_async_request(self, request):
            self.requests.append(request)
            async for chunk in request.stream:
                self.chunks.append(chunk)
                if self.reader.read_sizes and not self.first_data.is_set():
                    self.first_data.set()
                    await self.resume.wait()
            self.completed = True
            return httpx.Response(200, json={"imported": 1, "skipped": 0, "errors": []})

    async def exercise(stage, cancel):
        reader = SlowReader(stage)
        capture = Capture(reader)
        # Independent watchdog makes the pre-fix event-loop stall fail rather
        # than hang the suite. Success/cancellation must happen before release.
        finished = threading.Event()
        def watchdog():
            if reader.blocked.wait(5):
                finished.wait(3)
            reader.release.set()
        watcher = threading.Thread(target=watchdog)
        watcher.start()
        task = None
        try:
            async with httpx.AsyncClient(transport=capture) as injected:
                async with AsyncManyForge(base_url="https://example.test", access_token="token", http_client=injected) as client:
                    task = asyncio.create_task(client.business(business_id).mailing.subscribers.import_csv(
                        lid=list_id, consent_attested=True, skip_confirmation=True,
                        file=Upload(filename, reader)))
                    assert await asyncio.to_thread(reader.blocked.wait, 5)
                    # Reaching this assertion while the producer is blocked is
                    # the heartbeat; the watchdog has not rescued a stalled loop.
                    assert not reader.release.is_set(), f"{stage} blocked the event loop"
                    if cancel:
                        task.cancel()
                        try:
                            await asyncio.wait_for(task, 0.5)
                        except asyncio.CancelledError:
                            pass
                        else:
                            raise AssertionError("Upload cancellation was swallowed")
                        assert not reader.release.is_set(), "Cancellation waited for the reader"
                        reads, sent = len(reader.read_sizes), list(capture.chunks)
                        reader.release.set()
                        assert await asyncio.to_thread(reader.returned.wait, 5)
                        await asyncio.sleep(0.02)
                        assert len(reader.read_sizes) == reads
                        assert capture.chunks == sent and not capture.completed
                        assert len(capture.requests) <= 1
                    else:
                        reader.release.set()
                        await asyncio.wait_for(capture.first_data.wait(), 5)
                        assert len(reader.read_sizes) == 1, "Upload was prefetched"
                        assert reader.tell() < len(payload), "Upload was fully buffered"
                        capture.resume.set()
                        result = await task
                        assert result.imported == 1 and result.errors == []
                        assert all(0 < size <= 65536 for size in reader.read_sizes)
                        assert threading.get_ident() not in reader.threads
                        request = capture.requests[0]
                        expected = httpx.Request("POST", request.url,
                            headers={"Content-Type": request.headers["content-type"]},
                            data={"consent_attested": "true", "skip_confirmation": "true"},
                            files={"file": (filename, payload, "text/csv")})
                        encoded = b"".join(capture.chunks)
                        assert encoded == expected.read()
                        assert int(request.headers["content-length"]) == len(encoded)
                assert not injected.is_closed
                assert not reader.closed
        finally:
            reader.release.set()
            capture.resume.set()
            finished.set()
            if task is not None and not task.done():
                task.cancel()
            if task is not None:
                await asyncio.gather(task, return_exceptions=True)
            await asyncio.to_thread(watcher.join)
            reader.close()

    # Probe, rewind and read are separate cancellation boundaries: cancellation
    # during seek must not allow the encoder to begin its next blocking read.
    for stage in ("probe", "read"):
        await exercise(stage, False)
    for stage in ("probe", "rewind", "read"):
        await exercise(stage, True)

    read_fd, write_fd = os.pipe()
    pipe = os.fdopen(read_fd, "rb")
    pipe_payload = b"email\r\npipe@example.invalid\r\n"
    os.write(write_fd, pipe_payload)
    os.close(write_fd)
    pipe_requests = []
    def receive_pipe(request):
        pipe_requests.append(request)
        return httpx.Response(200, json={"imported": 1, "skipped": 0, "errors": []})
    try:
        async with httpx.AsyncClient(transport=httpx.MockTransport(receive_pipe)) as injected:
            async with AsyncManyForge(base_url="https://example.test", access_token="token", http_client=injected) as client:
                await client.business(business_id).mailing.subscribers.import_csv(
                    lid=list_id, consent_attested=True, file=Upload("pipe.csv", pipe))
            assert not injected.is_closed and not pipe.closed
        assert pipe_payload in pipe_requests[0].content
        assert "content-length" not in pipe_requests[0].headers
        assert pipe_requests[0].headers["transfer-encoding"] == "chunked"
    finally:
        pipe.close()

    sync_reader = SlowReader(None)
    with httpx.Client(transport=httpx.MockTransport(receive_pipe)) as injected:
        with ManyForge(base_url="https://example.test", access_token="token", http_client=injected) as client:
            client.business(business_id).mailing.subscribers.import_csv(
                lid=list_id, consent_attested=True, file=Upload(filename, sync_reader))
        assert not injected.is_closed and not sync_reader.closed
    assert set(sync_reader.threads) == {threading.get_ident()}
    sync_reader.close()
    assertions.append("async-multipart-heartbeat-bounded-backpressure-cancellation-bytes-pipe-ownership-native-sync")


async def asynchronous(fixture, assertions):
    await asynchronous_uploads(assertions)
    rotations = []
    async def rotated(pair):
        await asyncio.sleep(0.01)
        rotations.append(pair)
    async with AsyncManyForge(base_url=fixture["session_base_url"]) as bootstrap:
        pair = await bootstrap.auth.login(fixture["email"], fixture["password"])
    session = AsyncSession.from_token_pair(pair, on_rotate=rotated)
    await asyncio.sleep(float(fixture["access_token_ttl_seconds"]) + 0.05)
    async with AsyncManyForge(base_url=fixture["session_base_url"], session=session) as client:
        accounts = await asyncio.gather(*(client.account.get() for _ in range(8)))
        assert len(rotations) == 1
        assert all(account.email == fixture["email"] and account.id == accounts[0].id for account in accounts)
        assert rotations[0].refresh_token != pair.refresh_token
    assertions.append("async-real-session-single-flight-awaited-rotation")

    started = asyncio.Event()
    calls = []
    async def stalled(request):
        calls.append(request)
        started.set()
        await asyncio.sleep(60)
        return httpx.Response(204)
    async with httpx.AsyncClient(transport=httpx.MockTransport(stalled)) as injected:
        transport = AsyncHTTPTransport("https://example.test", http_client=injected)
        task = asyncio.create_task(transport.request(Request(operation_id="cancel", method="POST", path="/mutation", body={"value": 1}, response=TypeAdapter(type(None)), authenticated=False)))
        await started.wait()
        task.cancel()
        try:
            await task
        except asyncio.CancelledError:
            pass
        else:
            raise AssertionError("Cancellation was swallowed")
        await asyncio.sleep(0.02)
        assert len(calls) == 1
        await transport.aclose()
        assert not injected.is_closed
    assertions.append("native-async-cancellation-without-mutation-replay")


def main():
    fixture = json.loads(Path(sys.argv[1]).read_text())
    assertions = []
    supplemental(assertions)
    with ManyForge(base_url=fixture["base_url"]) as bootstrap:
        pair = bootstrap.auth.login(fixture["email"], fixture["password"])
        other_pair = bootstrap.auth.login(fixture["other_email"], fixture["other_password"])
    rotations = []
    session = Session.from_token_pair(pair, on_rotate=rotations.append)
    with ManyForge(base_url=fixture["base_url"], session=session) as client, ManyForge(base_url=fixture["base_url"], session=Session.from_token_pair(other_pair)) as other:
        business = client.businesses.create(business_create_request=BusinessCreateRequest(name="Python SDK smoke"))
        scope = client.business(business.id)
        setup_checks = {item.id.value: item for item in scope.mailing.setup.get().checks}
        assert setup_checks["outbound_enabled"].status.value == "blocked"
        assert setup_checks["smtp_relay"].required_for == ["relay"]
        assertions.append("real-provider-scoped-outbound-setup")
        first = scope.contacts.create(create_contact=CreateContact(primary_email="python-contact@example.invalid", display_name="Original"))
        read_back = scope.contacts.get(cid=first.id)
        assert read_back.id == first.id and read_back.primary_email == "python-contact@example.invalid"
        updated = scope.contacts.update(cid=first.id, update_contact=UpdateContact(display_name="Updated Python"))
        assert updated.display_name == "Updated Python"
        expected = {first.id}
        for index in range(3):
            expected.add(scope.contacts.create(create_contact=CreateContact(primary_email=f"python-{index}@example.invalid", display_name=f"Contact {index}")).id)
        contacts = list(scope.contacts.iter(limit=1))
        assert {contact.id for contact in contacts} == expected and len(contacts) == len(expected)
        assert all(contact.tenant_root_id == business.tenant_root_id for contact in contacts)
        hidden = expect_api(lambda: other.business(business.id).contacts.get(cid=first.id), 404, "NOT_FOUND")
        missing = expect_api(lambda: other.business(business.id).contacts.get(cid=uuid4()), 404, "NOT_FOUND")
        assert hidden.request_id and missing.request_id and hidden.details == missing.details
        assertions.append("real-crm-create-read-update-pagination-tenant-rls-not-found")

        ticket_scope = client.business(fixture["ticket_business_id"])
        ticket_id = UUID(fixture["ticket_id"])
        owner_id = UUID(fixture["principal_id"])
        ticket = ticket_scope.tickets.update(tid=ticket_id, patch_ticket=PatchTicket(priority="high"))
        assert ticket.priority.value == "high" and ticket.assignee_principal_id == owner_id
        ticket = ticket_scope.tickets.update(tid=ticket_id, patch_ticket=PatchTicket(assignee_principal_id=None))
        assert ticket.assignee_principal_id is None
        ticket = ticket_scope.tickets.update(tid=ticket_id, patch_ticket=PatchTicket(assignee_principal_id=owner_id))
        assert ticket.assignee_principal_id == owner_id
        assertions.append("real-ticket-absent-null-value-patch")

        board = scope.feedback.boards.create(board_create=BoardCreate(name="SDK board", is_public=True))
        key = scope.feedback.keys.create(bid=board.id, ingest_key_create=IngestKeyCreate(label="Python smoke"))
        with FeedbackClient(base_url=fixture["base_url"], publishable_key=key.publishable_key) as public, SignedFeedbackClient(base_url=fixture["base_url"], publishable_key=key.publishable_key, signing_secret=key.secret) as signed:
            anonymous = public.posts.create(public_submit=PublicSubmit(title="Python anonymous", author_identity="anonymous-python"))
            assert anonymous.identity_verified is False
            submitted = PublicSubmit(title="Python signed", body="same bytes", author_identity="signed-python", idempotency_key=str(uuid4()))
            signed_post = signed.posts.create(public_submit=submitted)
            duplicate = signed.posts.create(public_submit=submitted)
            assert signed_post.identity_verified and not signed_post.deduped
            assert duplicate.id == signed_post.id and duplicate.deduped
            expect_api(lambda: signed.posts.create(public_submit=submitted.model_copy(update={"body": "changed"})), 409)
            vote = public.posts.vote(post_id=anonymous.id, public_vote=PublicVote(voter_identity="python-voter"))
            repeated = public.posts.vote(post_id=anonymous.id, public_vote=PublicVote(voter_identity="python-voter"))
            assert vote.voted and not repeated.voted and repeated.vote_count == vote.vote_count
            listing = public.posts.list(limit=20)
            assert anonymous.id in {post.id for post in listing.items}
            scope.feedback.keys.revoke(kid=key.id)
            revoked = expect_api(lambda: public.posts.list(), 401)
            with FeedbackClient(base_url=fixture["base_url"], publishable_key="invalid") as unknown:
                invalid = expect_api(lambda: unknown.posts.list(), 401)
            assert revoked.details == invalid.details
        assertions.append("real-feedback-anonymous-signature-idempotency-conflict-vote-revocation")

        crash = scope.telemetry.clients.create(telemetry_client_create=TelemetryClientCreate.model_validate({"kind": "crash", "name": "Python crash"}))
        current = CrashEvent(occurred_at=datetime.now(timezone.utc), platform="python", signature="sdk-smoke", payload={"count": 9007199254740993})
        stale = current.model_copy(update={"occurred_at": datetime.now(timezone.utc) - timedelta(days=30)})
        with TelemetryClient(base_url=fixture["base_url"], publishable_key=crash.publishable_key) as telemetry:
            ingestion = telemetry.ingest(telemetry_ingest_request=TelemetryIngestRequest(crash=[current, stale]))
            assert ingestion.accepted == 1 and ingestion.dropped == 1
            expect_api(lambda: telemetry.ingest(telemetry_ingest_request=TelemetryIngestRequest(crash=[current] * 1001)), 400)
        protected = scope.telemetry.clients.create(telemetry_client_create=TelemetryClientCreate.model_validate({"kind": "crash", "name": "Python signed crash", "require_signature": True}))
        with TelemetryClient(base_url=fixture["base_url"], publishable_key=protected.publishable_key) as unsigned:
            expect_api(lambda: unsigned.ingest(telemetry_ingest_request=TelemetryIngestRequest(crash=[current])), 401)
        with SignedTelemetryClient(base_url=fixture["base_url"], publishable_key=protected.publishable_key, signing_secret=protected.secret) as signed:
            assert signed.ingest(telemetry_ingest_request=TelemetryIngestRequest(crash=[current])).accepted == 1
        with SignedTelemetryClient(base_url=fixture["base_url"], publishable_key=protected.publishable_key, signing_secret="incorrect-secret") as wrong:
            expect_api(lambda: wrong.ingest(telemetry_ingest_request=TelemetryIngestRequest(crash=[current])), 401)
        with TelemetryClient(base_url=fixture["lossy_telemetry_base_url"], publishable_key=crash.publishable_key) as lossy:
            expect_error(lambda: lossy.ingest(telemetry_ingest_request=TelemetryIngestRequest(crash=[current])), TransportError)
        assertions.append("real-telemetry-partial-batch-bound-signature-loss-without-replay")

        analytics = scope.telemetry.clients.create(telemetry_client_create=TelemetryClientCreate.model_validate({"kind": "analytics", "name": "Python analytics", "allowed_origins": [fixture["allowed_origin"]]}))
        event_name = "python_allowed_" + uuid4().hex
        denied_event_name = "python_denied_" + uuid4().hex
        with AnalyticsClient(base_url=fixture["base_url"], publishable_key=analytics.publishable_key, source_origin=fixture["allowed_origin"]) as collector:
            assert collector.collect(analytics_collect_request=AnalyticsCollectRequest(n=event_name, p="/python-smoke")) is None
        with AnalyticsClient(base_url=fixture["base_url"], publishable_key=analytics.publishable_key, source_origin=fixture["denied_origin"]) as collector:
            assert collector.collect(analytics_collect_request=AnalyticsCollectRequest(n=denied_event_name, p="/python-denied")) is None
        summary = scope.analytics.get(client_id=analytics.id, days=7)
        assert type(summary.var_from) is date and type(summary.to) is date
        assert all(type(point.var_date) is date for point in summary.series)
        assert summary.to >= summary.var_from
        assertions.append("real-analytics-source-origin-attempts-and-date-only-summary")

        mailing_list = scope.mailing.lists.create(list_input=ListInput(name="Python import", double_opt_in=False))
        subscriber = "python-subscriber@example.invalid"
        csv_bytes = f"email,first_name,last_name\n{subscriber},Python,Smoke\n".encode()
        imported = scope.mailing.subscribers.import_csv(lid=mailing_list.id, consent_attested=True, skip_confirmation=True, file=Upload("subscribers.csv", io.BytesIO(csv_bytes)))
        assert imported.imported == 1 and imported.skipped == 0
        with scope.mailing.subscribers.export_csv(lid=mailing_list.id, request_options=RequestOptions(timeout=60)) as stream:
            exported = stream.read(8) + stream.read()
            assert "text/csv" in stream.headers["content-type"]
        rows = list(csv.DictReader(io.StringIO(exported.decode())))
        assert [row["email"] for row in rows] == [subscriber]
        oversized = b"email,first_name\npython-large@example.invalid," + b"x" * (5 * 1024 * 1024)
        expect_api(lambda: scope.mailing.subscribers.import_csv(lid=mailing_list.id, consent_attested=True, skip_confirmation=True, file=Upload("too-large.csv", oversized)), 400)
        assertions.append("real-consented-csv-import-closeable-export-oversized-rejection")

        duplicate_list = scope.mailing.lists.create(list_input=ListInput(name="Python reporting overlap", double_opt_in=False))
        scope.mailing.subscribers.import_csv(lid=duplicate_list.id, consent_attested=True, skip_confirmation=True, file=Upload("duplicate.csv", csv_bytes))
        report = client.analytics.mailing(business_id=business.id)
        assert report.business_count == 1 and report.active_subscribers == 1
        assert report.subscriber_net_additions == 1 and not report.subscriber_window_complete
        assert report.window_start <= report.subscriber_window_start <= report.as_of
        assert report.open_rate.denominator == 0 and report.open_rate.percent is None
        expect_api(lambda: other.analytics.mailing(business_id=business.id), 404, "NOT_FOUND")
        assertions.append("real-mailing-reporting-deduplicated-history-null-rate-and-isolation")

        # Password step-up is an application 401, never a signal to rotate/replay.
        source = client.businesses.create(business_create_request=BusinessCreateRequest(name="Python merge source"))
        destination = client.businesses.create(business_create_request=BusinessCreateRequest(name="Python merge destination"))
        merge = client.tenant_merges.create(id=source.id, idempotency_key=str(uuid4()), create_tenant_merge=CreateTenantMerge(destination_parent_id=destination.id))
        with ManyForge(base_url=fixture["base_url"]) as bootstrap:
            step_pair = bootstrap.auth.login(fixture["email"], fixture["password"])
        step_rotations = []
        with ManyForge(base_url=fixture["base_url"], session=Session.from_token_pair(step_pair, on_rotate=step_rotations.append)) as stepup:
            expect_api(lambda: stepup.tenant_merges.confirm(operation_id=merge.id, confirm_tenant_merge=ConfirmTenantMerge(source_name=source.name, destination_name=destination.name, password="deliberately-wrong")), 401, "REAUTHENTICATION_FAILED")
            assert step_rotations == []
        assertions.append("real-stepup-401-does-not-refresh")

    with ManyForge(base_url=fixture["lossy_base_url"]) as bootstrap:
        lost_pair = bootstrap.auth.login(fixture["email"], fixture["password"])
    lost_session = Session.from_token_pair(lost_pair)
    time.sleep(float(fixture["access_token_ttl_seconds"]) + 0.05)
    with ManyForge(base_url=fixture["lossy_base_url"], session=lost_session) as lossy:
        expect_error(lambda: lossy.account.get(), SessionError)
        expect_error(lambda: lossy.account.get(), SessionError)
    assertions.append("real-consumed-refresh-loss-invalidates-without-retry")

    with FeedbackClient(base_url=fixture["redirect_base_url"], publishable_key="redirect") as redirect:
        error = expect_error(lambda: redirect.posts.list(), ManyForgeError)
        assert 300 <= error.status < 400 and "location" in error.headers
    assertions.append("real-native-redirect-blocked")
    with asyncio.Runner() as runner:
        runner.run(asynchronous(fixture, assertions))
    Path(fixture["result_path"]).write_text(json.dumps({"business_id": str(business.id), "contact_id": str(first.id), "analytics_client_id": str(analytics.id), "analytics_event_name": event_name, "analytics_denied_event_name": denied_event_name, "board_id": str(board.id), "feedback_publishable_key": key.publishable_key, "assertions": assertions}, indent=2))


if __name__ == "__main__":
    main()
