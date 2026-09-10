import com.manyforge.sdk.*;
import com.manyforge.sdk.TelemetryClient;
import com.manyforge.sdk.ProtocolException;
import com.manyforge.sdk.models.*;
import com.manyforge.sdk.resources.RootBusinessesResource.*;
import com.manyforge.sdk.resources.BusinessContactsResource.*;
import com.manyforge.sdk.resources.BusinessTicketsResource.*;
import com.manyforge.sdk.resources.BusinessFeedbackBoardsResource.*;
import com.manyforge.sdk.resources.BusinessFeedbackKeysResource.*;
import com.manyforge.sdk.resources.FeedbackPostsResource.*;
import com.manyforge.sdk.resources.BusinessTelemetryClientsResource.*;
import com.manyforge.sdk.resources.TelemetryTelemetryResource.*;
import com.manyforge.sdk.resources.AnalyticsAnalyticsResource.*;
import com.manyforge.sdk.resources.BusinessAnalyticsResource.*;
import com.manyforge.sdk.resources.BusinessMailingListsResource.*;
import com.manyforge.sdk.resources.BusinessMailingSubscribersResource.*;
import com.manyforge.sdk.resources.MailingMailingResource.*;
import com.manyforge.sdk.resources.BusinessMailingKeysResource.*;
import com.manyforge.sdk.resources.MailingServerMailingSubscribersResource.*;
import com.manyforge.sdk.resources.BusinessAutomationsResource.*;
import com.manyforge.sdk.resources.BusinessAutomationsVersionsResource.*;
import com.manyforge.sdk.resources.BusinessAutomationsVersionsGraphResource.*;
import com.fasterxml.jackson.databind.*;
import com.fasterxml.jackson.databind.node.ObjectNode;
import com.sun.net.httpserver.*;
import java.io.*;
import java.net.*;
import java.net.http.*;
import java.nio.charset.StandardCharsets;
import java.nio.file.*;
import java.time.*;
import java.util.*;
import java.util.concurrent.*;
import java.util.concurrent.atomic.*;
import javax.crypto.Mac;
import javax.crypto.spec.SecretKeySpec;

/** Executed against the installed jar, outside the SDK source tree. No product HTTP calls bypass the SDK. */
public final class Consumer {
    static final ObjectMapper JSON = ModelJson.createMapper();
    static final List<String> assertions = new ArrayList<>();
    static JsonNode fixture;
    static String field(String name) { return fixture.path(name).asText(); }
    static void check(boolean condition, String message) { if (!condition) throw new AssertionError(message); }
    static void passed(String name) { assertions.add(name); }
    interface Action { void run() throws Exception; }
    static ManyForgeException api(int status, Action action) throws Exception {
        try { action.run(); } catch (ManyForgeException error) { check(error.status() == status, "HTTP status " + status); return error; }
        throw new AssertionError("Expected HTTP " + status);
    }
    static Throwable fails(Action action) throws Exception {
        try { action.run(); } catch (AssertionError error) { throw error; } catch (Throwable error) { return error; }
        throw new AssertionError("Expected failure");
    }
    static TokenPair login(String base, boolean other) {
        try (ManyForgeClient bootstrap = ManyForgeClient.builder().baseUrl(base).build()) {
            return bootstrap.auth().login(field(other ? "other_email" : "email"), field(other ? "other_password" : "password"));
        }
    }
    static ManyForgeClient client(String base, Session session) { return ManyForgeClient.builder().baseUrl(base).session(session).build(); }
    static TelemetryTelemetryIngestParams batch(List<CrashEvent> events) {
        return TelemetryTelemetryIngestParams.builder().telemetryIngestRequest(new TelemetryIngestRequest().crash(events)).build();
    }
    static CrashEvent crash(String signature, OffsetDateTime at) { return new CrashEvent().platform("java").signature(signature).occurredAt(at).payload(Map.of("sdk", "java")); }
    static void margin() throws InterruptedException { Thread.sleep(3100); }

    public static void main(String[] args) throws Exception {
        if (args.length > 0 && args[0].equals("--boundary")) {
            boundaries(args.length > 1 ? args[1] : "all");
            System.out.println(String.join("\n", assertions));
            return;
        }
        fixture = JSON.readTree(Path.of(args[0]).toFile());
        String base = field("base_url");
        Session owner = Session.fromTokenPair(login(base, false));
        Map<String, Object> result = new LinkedHashMap<>();
        try (ManyForgeClient client = client(base, owner)) {
            var business = client.businesses().create(RootBusinessesCreateParams.builder().businessCreateRequest(new BusinessCreateRequest().name("Java SDK smoke")).build());
            var scope = client.business(business.getId());
            var setup = scope.mailing().setup().get();
            check(setup.getChecks().stream().anyMatch(item -> "outbound_enabled".equals(item.getId().getValue()) && "blocked".equals(item.getStatus().getValue())), "outbound setup disabled prerequisite");
            var relayCheck = setup.getChecks().stream().filter(item -> "smtp_relay".equals(item.getId().getValue())).findFirst().orElseThrow();
            check(relayCheck.getRequiredFor().size() == 1 && "relay".equals(relayCheck.getRequiredFor().get(0).getValue()), "relay-only SMTP prerequisite");
            passed("real_provider_scoped_outbound_setup");
            var contact = scope.contacts().create(BusinessContactsCreateParams.builder().createContact(new CreateContact().primaryEmail("java-contact@example.invalid").displayName("Before")).build());
            var readContact = scope.contacts().get(BusinessContactsGetParams.builder().cid(contact.getId()).build());
            check(readContact.getPrimaryEmail().equals("java-contact@example.invalid"), "contact primary email");
            var updated = scope.contacts().update(BusinessContactsUpdateParams.builder().cid(contact.getId()).updateContact(new UpdateContact().displayName("After")).build());
            check(updated.getDisplayName().equals("After"), "contact update");
            Set<UUID> expected = new HashSet<>(); expected.add(contact.getId());
            for (int i = 0; i < 3; i++) expected.add(scope.contacts().create(BusinessContactsCreateParams.builder().createContact(new CreateContact().primaryEmail("java-" + i + "@example.invalid")).build()).getId());
            Set<UUID> actual = new HashSet<>();
            for (Contact item : scope.contacts().iter(BusinessContactsListParams.builder().limit(1).build())) {
                check(item.getTenantRootId().equals(business.getTenantRootId()), "pagination tenant scope");
                check(actual.add(item.getId()), "pagination duplicate");
            }
            check(actual.equals(expected), "pagination exhausts exactly matching contacts");
            try (ManyForgeClient other = client(base, Session.fromTokenPair(login(base, true)))) {
                ManyForgeException hidden = api(404, () -> other.business(business.getId()).contacts().get(BusinessContactsGetParams.builder().cid(contact.getId()).build()));
                ManyForgeException absent = api(404, () -> other.business(business.getId()).contacts().get(BusinessContactsGetParams.builder().cid(UUID.randomUUID()).build()));
                check("NOT_FOUND".equals(hidden.code()) && Objects.equals(hidden.code(), absent.code()), "RLS nonexistence equivalence");
                check(hidden.requestId() != null && absent.requestId() != null, "error request IDs");
            }
            passed("real_crm_and_rls_pagination");
            result.put("business_id", business.getId()); result.put("contact_id", contact.getId());

            var automation = scope.automations().create(BusinessAutomationsCreateParams.builder().automationCreateInput(new AutomationCreateInput().name("Java null draft")).build());
            var version = scope.automations().versions().get(BusinessAutomationsVersionsGetParams.builder().aid(automation.getId()).vid(automation.getDraftVersionId()).build());
            var graph = new GraphInput().nodes(List.of(new NodeInput(new DraftNodeInput().id("wait-draft").kind("wait").config(null)))).edges(List.of());
            var saved = scope.automations().versions().graph().replace(BusinessAutomationsVersionsGraphReplaceParams.builder().aid(automation.getId()).vid(version.getId()).graphInput(graph).build());
            var loaded = scope.automations().versions().get(BusinessAutomationsVersionsGetParams.builder().aid(automation.getId()).vid(version.getId()).build());
            for (AutomationVersion draft : List.of(saved, loaded)) {
                DraftNode node = draft.getGraph().getNodes().get(0).asDraftNode();
                check(node.getId().equals("wait-draft") && node.configPresence().isPresent() && node.getConfig() == null, "real saved null draft remains a typed draft");
            }
            passed("real_automation_null_draft_save_read");

            var tickets = client.business(field("ticket_business_id")).tickets();
            UUID ticketId = UUID.fromString(field("ticket_id")), principal = UUID.fromString(field("principal_id"));
            tickets.update(TicketUpdateParams.builder().tid(ticketId).patchTicket(new PatchTicket().priority(new PatchTicket.PriorityEnum("high"))).build());
            check(principal.equals(tickets.get(TicketGetParams.builder().tid(ticketId).build()).getAssigneePrincipalId()), "omitted assignee preserved");
            tickets.update(TicketUpdateParams.builder().tid(ticketId).patchTicket(new PatchTicket().assigneePrincipalId(null)).build());
            check(tickets.get(TicketGetParams.builder().tid(ticketId).build()).getAssigneePrincipalId() == null, "null assignee clears");
            tickets.update(TicketUpdateParams.builder().tid(ticketId).patchTicket(new PatchTicket().assigneePrincipalId(principal)).build());
            Ticket ticket = tickets.get(TicketGetParams.builder().tid(ticketId).build());
            check(principal.equals(ticket.getAssigneePrincipalId()), "explicit assignee assigns");
            passed("real_nullable_ticket_patch");

            AtomicInteger rotations = new AtomicInteger();
            String sessionBase = field("session_base_url");
            CompletableFuture<Void> persist = new CompletableFuture<>();
            Session concurrent = Session.fromTokenPair(login(sessionBase, false), pair -> { rotations.incrementAndGet(); return persist; });
            try (ManyForgeClient rotating = client(sessionBase, concurrent)) {
                margin();
                List<CompletableFuture<?>> readers = new ArrayList<>();
                for (int i = 0; i < 8; i++) readers.add(rotating.account().getAsync());
                long until = System.nanoTime() + Duration.ofSeconds(10).toNanos();
                while (rotations.get() == 0 && System.nanoTime() < until) Thread.sleep(10);
                check(rotations.get() == 1, "one rotation callback");
                check(readers.stream().noneMatch(CompletableFuture::isDone), "persistence awaited");
                readers.get(0).cancel(true);
                persist.complete(null);
                CompletableFuture.allOf(readers.subList(1, readers.size()).toArray(CompletableFuture[]::new)).get(10, TimeUnit.SECONDS);
                check(rotations.get() == 1 && concurrent.isValid(), "cancelled waiter cannot cancel session peers");
            }
            passed("real_single_flight_refresh_and_awaited_persistence");
            String lossyBase = field("lossy_base_url");
            Session lossy = Session.fromTokenPair(login(lossyBase, false));
            try (ManyForgeClient losing = client(lossyBase, lossy)) {
                margin(); check(fails(() -> losing.account().get()) instanceof SessionException, "lossy refresh session error");
                check(!lossy.isValid(), "lossy session invalidated");
                check(fails(() -> losing.account().get()) instanceof SessionException, "consumed token never retried");
            }
            passed("real_consumed_refresh_response_loss");
            Session undurable = Session.fromTokenPair(login(base, false), pair -> CompletableFuture.failedFuture(new IOException("persistence unavailable")));
            try (ManyForgeClient losing = client(base, undurable)) {
                margin(); check(fails(() -> losing.account().get()) instanceof SessionException, "persistence failure");
                check(!undurable.isValid(), "undurable owner invalidated");
            }
            passed("real_persistence_failure_invalidates");

            var board = scope.feedback().boards().create(BusinessFeedbackBoardsCreateParams.builder().boardCreate(new BoardCreate().name("Java public feedback").isPublic(true)).build());
            var key = scope.feedback().keys().create(BusinessFeedbackKeysCreateParams.builder().bid(board.getId()).ingestKeyCreate(new IngestKeyCreate().label("Java smoke")).build());
            result.put("board_id", board.getId()); result.put("feedback_publishable_key", key.getPublishableKey());
            try (FeedbackClient publicClient = FeedbackClient.builder().baseUrl(base).publishableKey(key.getPublishableKey()).build();
                 SignedFeedbackClient signed = SignedFeedbackClient.builder().baseUrl(base).publishableKey(key.getPublishableKey()).signingSecret(key.getSecret()).build()) {
                PublicSubmit body = new PublicSubmit().title("Java unsigned").body("real fixture").authorIdentity("java@example.invalid").idempotencyKey(UUID.randomUUID().toString());
                var params = FeedbackPostsCreateParams.builder().publicSubmit(body).build();
                var post = publicClient.posts().create(params);
                check(Boolean.FALSE.equals(post.getIdentityVerified()), "unsigned identity");
                var duplicate = publicClient.posts().create(params);
                check(post.getId().equals(duplicate.getId()) && Boolean.TRUE.equals(duplicate.getDeduped()), "stable idempotency");
                api(409, () -> publicClient.posts().create(FeedbackPostsCreateParams.builder().publicSubmit(new PublicSubmit().title("Changed").body("real fixture").authorIdentity("java@example.invalid").idempotencyKey(body.getIdempotencyKey())).build()));
                var verified = signed.posts().create(FeedbackPostsCreateParams.builder().publicSubmit(new PublicSubmit().title("Java signed").authorIdentity("java+signed@example.invalid")).build());
                check(Boolean.TRUE.equals(verified.getIdentityVerified()), "signed identity");
                var vote = FeedbackPostsVoteParams.builder().postID(post.getId()).publicVote(new PublicVote().voterIdentity("java-voter@example.invalid")).build();
                check(Boolean.TRUE.equals(publicClient.posts().vote(vote).getVoted()), "first vote");
                check(Boolean.FALSE.equals(publicClient.posts().vote(vote).getVoted()), "duplicate vote does not unvote");
                signed.posts().list(FeedbackPostsListParams.builder().limit(10).author("java+signed@example.invalid").voterIdentity("voter&?=/% +").build());
                scope.feedback().keys().revoke(BusinessFeedbackKeysRevokeParams.builder().kid(key.getId()).build());
                api(401, () -> publicClient.posts().list());
                api(401, () -> signed.posts().list());
            }
            passed("real_feedback_signing_idempotency_votes_revocation");

            var telemetryKey = scope.telemetry().clients().create(BusinessTelemetryClientsCreateParams.builder().telemetryClientCreate(new TelemetryClientCreate(new TelemetryCrashClientCreate().kind(new TelemetryCrashClientCreate.KindEnum("crash")).name("Java crash").requireSignature(false))).build());
            CrashEvent current = crash("java-current", OffsetDateTime.now(ZoneOffset.UTC));
            try (TelemetryClient telemetry = TelemetryClient.builder().baseUrl(base).publishableKey(telemetryKey.getPublishableKey()).build()) {
                var accepted = telemetry.ingest(batch(List.of(current, crash("old", OffsetDateTime.now(ZoneOffset.UTC).minusDays(8)))));
                check(accepted.getAccepted() == 1 && accepted.getDropped() == 1, "partial telemetry acceptance");
                api(400, () -> telemetry.ingest(batch(Collections.nCopies(1001, current))));
            }
            var requiredKey = scope.telemetry().clients().create(BusinessTelemetryClientsCreateParams.builder().telemetryClientCreate(new TelemetryClientCreate(new TelemetryCrashClientCreate().kind(new TelemetryCrashClientCreate.KindEnum("crash")).name("Java signed crash").requireSignature(true))).build());
            try (TelemetryClient unsigned = TelemetryClient.builder().baseUrl(base).publishableKey(requiredKey.getPublishableKey()).build();
                 SignedTelemetryClient signed = SignedTelemetryClient.builder().baseUrl(base).publishableKey(requiredKey.getPublishableKey()).signingSecret(requiredKey.getSecret()).build();
                 SignedTelemetryClient wrong = SignedTelemetryClient.builder().baseUrl(base).publishableKey(requiredKey.getPublishableKey()).signingSecret("wrong-secret").build()) {
                api(401, () -> unsigned.ingest(batch(List.of(current))));
                api(401, () -> wrong.ingest(batch(List.of(current))));
                check(signed.ingest(batch(List.of(current))).getAccepted() == 1, "required signed telemetry");
            }
            try (TelemetryClient losing = TelemetryClient.builder().baseUrl(field("lossy_telemetry_base_url")).publishableKey(telemetryKey.getPublishableKey()).build()) {
                fails(() -> losing.ingest(batch(List.of(crash("java-lossy", OffsetDateTime.now(ZoneOffset.UTC))))));
            }
            passed("real_telemetry_partial_bounds_signatures_no_replay");

            var analyticsKey = scope.telemetry().clients().create(BusinessTelemetryClientsCreateParams.builder().telemetryClientCreate(new TelemetryClientCreate(new TelemetryAnalyticsClientCreate().kind(new TelemetryAnalyticsClientCreate.KindEnum("analytics")).name("Java analytics").allowedOrigins(Set.of(URI.create(field("allowed_origin")))))).build());
            String event = "java_allowed_" + UUID.randomUUID(), deniedEvent = "java_denied_" + UUID.randomUUID();
            try (AnalyticsClient analytics = AnalyticsClient.builder().baseUrl(base).publishableKey(analyticsKey.getPublishableKey()).sourceOrigin(field("allowed_origin")).build();
                 AnalyticsClient denied = AnalyticsClient.builder().baseUrl(base).publishableKey(analyticsKey.getPublishableKey()).sourceOrigin(field("denied_origin")).build()) {
                check(analytics.collect(AnalyticsAnalyticsCollectParams.builder().analyticsCollectRequest(new AnalyticsCollectRequest().n(event).p("/java-allowed")).build()) == null, "analytics makes no acceptance claim");
                check(denied.collect(AnalyticsAnalyticsCollectParams.builder().analyticsCollectRequest(new AnalyticsCollectRequest().n(deniedEvent).p("/java-denied")).build()) == null, "denied analytics also empty");
            }
            AnalyticsSummary summary = scope.analytics().get(BusinessAnalyticsGetParams.builder().clientId(analyticsKey.getId()).days(7).build());
            check(summary.getFrom() instanceof LocalDate && summary.getTo() instanceof LocalDate, "date-only summary");
            for (AnalyticsDayPoint point : summary.getSeries()) check(point.getDate() instanceof LocalDate, "date-only series");
            result.put("analytics_client_id", analyticsKey.getId()); result.put("analytics_event_name", event); result.put("analytics_denied_event_name", deniedEvent);
            passed("real_analytics_source_origin_and_dates");

            var mailingList = scope.mailing().lists().create(BusinessMailingListsCreateParams.builder().listInput(new ListInput().name("Java list").doubleOptIn(false)).build());
            byte[] csv = "email,first_name,last_name\njava-subscriber@example.invalid,Java,SDK\n".getBytes(StandardCharsets.UTF_8);
            AtomicBoolean uploadClosed = new AtomicBoolean();
            InputStream upload = new ByteArrayInputStream(csv) { public void close() { uploadClosed.set(true); } };
            ImportResult imported = scope.mailing().subscribers().importCsv(BusinessMailingSubscribersImportCsvParams.builder().lid(mailingList.getId()).file(new Upload("java.csv", upload)).consentAttested(true).skipConfirmation(true).build());
            check(imported.getImported() == 1 && imported.getSkipped() == 0, "typed CSV import");
            check(!uploadClosed.get(), "caller stream retained"); upload.close();
            try (InputStream exported = scope.mailing().subscribers().exportCsvAsync(BusinessMailingSubscribersExportCsvParams.builder().lid(mailingList.getId()).build(), new RequestOptions(Duration.ofSeconds(60))).get()) {
                String text = new String(exported.readAllBytes(), StandardCharsets.UTF_8);
                check(text.contains("email") && text.contains("java-subscriber@example.invalid"), "real CSV export stream");
            }
            byte[] oversized = new byte[(5 << 20) + 1]; Arrays.fill(oversized, (byte)'x');
            api(400, () -> scope.mailing().subscribers().importCsv(BusinessMailingSubscribersImportCsvParams.builder().lid(mailingList.getId()).file(Upload.bytes("too-big.csv", oversized)).consentAttested(true).skipConfirmation(true).build()));
            var mailingKey = scope.mailing().keys().create(BusinessMailingKeysCreateParams.builder().lid(mailingList.getId()).listKeyInput(new ListKeyInput().label("Java server")).build());
            try (MailingServerClient mailing = MailingServerClient.builder().baseUrl(base).publishableKey(mailingKey.getPublishableKey()).signingSecret(mailingKey.getSecret()).build()) {
                var subscriber = mailing.subscribers().create(MailingServerMailingSubscribersCreateParams.builder().s2SSubscriptionInput(new S2SSubscriptionInput().email("java-signed@example.invalid").skipConfirmation(true)).build());
                var subscribed = scope.mailing().subscribers().get(BusinessMailingSubscribersGetParams.builder().lid(mailingList.getId()).sid(subscriber.getSubscriberId()).build());
                check("java-signed@example.invalid".equals(subscribed.getEmail()), "server-signed mailing");
                mailing.subscribers().delete(MailingServerMailingSubscribersDeleteParams.builder().email("java-signed@example.invalid").build());
            }
            try (MailingClient mailing = MailingClient.builder().baseUrl(base).publishableKey(mailingKey.getPublishableKey()).build()) {
                check(Boolean.TRUE.equals(mailing.subscribe(MailingMailingSubscribeParams.builder().publicSubscriptionInput(new PublicSubscriptionInput().email("java-public@example.invalid")).build()).getAccepted().getValue()), "public mailing subscription accepted");
            }
            passed("real_csv_streaming_and_signed_mailing");
            supplemental(ticket, summary);
            boundaries("all");
        }
        try (FeedbackClient redirect = FeedbackClient.builder().baseUrl(field("redirect_base_url")).publishableKey("private-key").build()) {
            Throwable failure = fails(() -> redirect.posts().list());
            check(failure instanceof ManyForgeException, "redirect is an HTTP API error");
            ManyForgeException error = (ManyForgeException)failure;
            check(error.status() >= 300 && error.status() < 400, "redirect status retained");
            check(error.headers().firstValue("Location").isPresent(), "redirect metadata retained");
            check(!error.toString().contains("private-key"), "redirect errors do not expose keys");
        }
        passed("real_redirect_blocked");
        result.put("assertions", assertions);
        JSON.writeValue(Path.of(field("result_path")).toFile(), result);
    }

    static void supplemental(Ticket realTicket, AnalyticsSummary realSummary) throws Exception {
        try (Wire wire = new Wire()) {
            ObjectNode ticket = JSON.valueToTree(realTicket);
            ticket.put("message_count", 9007199254740993L); ticket.put("priority", "future-priority"); ticket.put("future_field", true);
            wire.body.set(JSON.writeValueAsBytes(ticket));
            try (ManyForgeClient client = ManyForgeClient.builder().baseUrl(wire.base).accessToken("owned-token").build()) {
                var tickets = client.business(UUID.randomUUID()).tickets();
                Ticket decoded = tickets.get(TicketGetParams.builder().tid(UUID.randomUUID()).build());
                check(decoded.getMessageCount() == 9007199254740993L && "future-priority".equals(decoded.getPriority().getValue()), "lossless int64 and unknown enum");
                tickets.update(TicketUpdateParams.builder().tid(UUID.randomUUID()).patchTicket(new PatchTicket().tags(List.of())).build());
                JsonNode payload = JSON.readTree(wire.requestBody.get());
                check(payload.get("tags").isArray() && payload.get("tags").isEmpty() && !payload.has("assignee_principal_id"), "empty array and omission on wire");
                tickets.update(TicketUpdateParams.builder().tid(UUID.randomUUID()).patchTicket(new PatchTicket().assigneePrincipalId(null)).build());
                check(JSON.readTree(wire.requestBody.get()).get("assignee_principal_id").isNull(), "null on wire");
                wire.body.set(JSON.writeValueAsBytes(realSummary));
                AnalyticsSummary summary = client.business(UUID.randomUUID()).analytics().get(BusinessAnalyticsGetParams.builder().clientId(UUID.randomUUID()).days(0).build());
                check(wire.target.get().contains("days=0") && summary.getFrom().equals(realSummary.getFrom()), "zero query/date response");
            }
            wire.status.set(204); wire.body.set(new byte[0]);
            try (AnalyticsClient analytics = AnalyticsClient.builder().baseUrl(wire.base).publishableKey("body-key").sourceOrigin("https://source.example").build()) {
                analytics.collect(AnalyticsAnalyticsCollectParams.builder().analyticsCollectRequest(new AnalyticsCollectRequest().d(Map.of("count", 9007199254740993L, "zero", 0, "enabled", false, "empty", List.of()))).build());
                JsonNode payload = JSON.readTree(wire.requestBody.get());
                check(payload.path("k").asText().equals("body-key"), "constructor key inserted after model serialization");
                check(payload.path("d").path("count").longValue() == 9007199254740993L, "int64 numeric wire token");
                check(payload.path("d").path("zero").asInt(-1) == 0 && payload.path("d").path("enabled").isBoolean() && !payload.path("d").path("enabled").asBoolean() && payload.path("d").path("empty").isEmpty(), "zero false empty values on wire");
            }
            passed("wire_presence_dates_unknown_enums_lossless_int64");

            wire.status.set(401); wire.body.set("{\"code\":\"REAUTHENTICATION_FAILED\",\"message\":\"secret message\"}".getBytes(StandardCharsets.UTF_8));
            AtomicInteger rotations = new AtomicInteger();
            Session session = Session.fromTokenPair(new TokenPair().accessToken("fresh-token").refreshToken("refresh-secret").expiresIn(300), pair -> { rotations.incrementAndGet(); return CompletableFuture.completedFuture(null); });
            int before = wire.requests.get();
            try (ManyForgeClient client = client(wire.base, session); FeedbackClient publicClient = FeedbackClient.builder().baseUrl(wire.base).publishableKey("key").build()) {
                ManyForgeException error = api(401, () -> client.account().get());
                check("REAUTHENTICATION_FAILED".equals(error.code()) && "secret message".equals(error.serverMessage()), "server errors retained");
                check(!error.toString().contains("secret"), "safe exception format");
                api(401, () -> publicClient.posts().list());
                check(rotations.get() == 0 && wire.requests.get() == before + 2, "401 has no refresh or replay");
                check(wire.authorization.get() == null && wire.cookie.get() == null, "public requests contain no management credentials");
                wire.status.set(502); wire.body.set(("<html>private response" + "x".repeat(70000)).getBytes(StandardCharsets.UTF_8));
                error = api(502, () -> client.account().get());
                check(error.rawBody().length == 65536 && error.truncated() && error.details() == null, "bounded HTML proxy error");
                wire.status.set(400); wire.body.set("{\"code\":\"FUTURE_VALID_CODE\",\"message\":\"new\",\"details\":{\"field\":1}}".getBytes(StandardCharsets.UTF_8));
                check("FUTURE_VALID_CODE".equals(api(400, () -> client.account().get()).code()), "unknown error code");
                wire.status.set(200); wire.body.set("not-json".getBytes(StandardCharsets.UTF_8));
                check(fails(() -> client.account().get()) instanceof ProtocolException, "invalid success separated from API error");
            }
            passed("wire_error_bounds_and_no_401_replay");

            wire.status.set(204); wire.body.set(new byte[0]); wire.delayMillis.set(2000);
            try (AnalyticsClient analytics = AnalyticsClient.builder().baseUrl(wire.base).publishableKey("key").sourceOrigin("https://source.example").build()) {
                var params = AnalyticsAnalyticsCollectParams.builder().analyticsCollectRequest(new AnalyticsCollectRequest().n("cancel")).build();
                int initial = wire.requests.get();
                CompletableFuture<Void> cancelled = analytics.collectAsync(params);
                wire.awaitRequests(initial + 1); cancelled.cancel(true);
                check(cancelled.isCancelled(), "native future cancellation");
                Throwable failure = fails(() -> analytics.collectAsync(params, new RequestOptions(Duration.ofMillis(75))).get());
                check(failure instanceof ExecutionException && failure.getCause() instanceof HttpTimeoutException, "native timeout retained");
                Thread.sleep(2500);
                check(wire.requests.get() == initial + 2, "timeout/cancel sends are never replayed");
            }
            wire.delayMillis.set(0);
            passed("wire_async_cancellation_timeout_no_mutation_replay");
            wire.status.set(200); wire.body.set("email\nstream@example.invalid\n".getBytes(StandardCharsets.UTF_8));
            try (ManyForgeClient client = ManyForgeClient.builder().baseUrl(wire.base).accessToken("token").build()) {
                var subscribers = client.business(UUID.randomUUID()).mailing().subscribers();
                var params = BusinessMailingSubscribersExportCsvParams.builder().lid(UUID.randomUUID()).build();
                try (InputStream warm = subscribers.exportCsv(params)) { warm.readAllBytes(); }
                wire.delayBodyMillis.set(2000);
                try (InputStream stream = subscribers.exportCsvAsync(params, new RequestOptions(Duration.ofSeconds(1))).get()) {
                    check(stream.read() == 'e', "stream prefix arrives before its deadline");
                    check(fails(() -> stream.readAllBytes()) instanceof HttpTimeoutException, "stream timeout applies after response headers");
                }
            }
            Thread.sleep(2200); wire.delayBodyMillis.set(0);
            passed("wire_stream_timeout_after_headers");

            wire.status.set(200); wire.body.set("{\"items\":[]}".getBytes(StandardCharsets.UTF_8));
            String secret = "mfs_entire-issued-secret";
            wire.signingSecret.set(secret);
            try (SignedFeedbackClient signed = SignedFeedbackClient.builder().baseUrl(wire.base).publishableKey("key/%?+ ").signingSecret(secret).build()) {
                signed.posts().list(FeedbackPostsListParams.builder().limit(0).voterIdentity("a&b?/%+ ").author("author+&").build());
                check(wire.target.get().contains("key%2F%25%3F%2B%20"), "path encoded exactly once");
                check(wire.target.get().endsWith("limit=0&voter_identity=a%26b%3F%2F%25%2B%20&author=author%2B%26"), "ordered encoded query");
                String header = wire.signature.get(); String timestamp = header.substring(2, header.indexOf(','));
                String canonical = timestamp + ".GET." + wire.target.get() + ".";
                check(header.endsWith(hmac(secret, canonical.getBytes(StandardCharsets.UTF_8))), "exact target HMAC");
                check(!header.endsWith(hmac(secret, (timestamp + ".GET." + wire.target.get().replace("limit=0&", "") + ".").getBytes(StandardCharsets.UTF_8))), "query tampering invalidates signature");
                String originalTarget = wire.target.get();
                String reorderedTarget = originalTarget.replace("limit=0&", "") + "&limit=0";
                check(HttpClient.newHttpClient().send(HttpRequest.newBuilder(URI.create(wire.base + reorderedTarget)).header("X-Feedback-Signature", header).build(), HttpResponse.BodyHandlers.discarding()).statusCode() == 401, "reordered query with original signature rejected by verifier");
            }
            wire.signingSecret.set(null);
            passed("wire_once_encoding_and_exact_query_signing");
            fails(() -> ManyForgeClient.builder().baseUrl(wire.base + "/api/v1").build());
            fails(() -> ManyForgeClient.builder().baseUrl("https://user:pass@example.test").build());
            fails(() -> ManyForgeClient.builder().baseUrl(wire.base).accessToken("x").session(session).build());
            fails(() -> FeedbackClient.builder().baseUrl(wire.base).publishableKey("key").httpClient(HttpClient.newBuilder().cookieHandler(new CookieManager()).build()).build());
            fails(() -> FeedbackClient.builder().baseUrl(wire.base).publishableKey("key").httpClient(HttpClient.newBuilder().followRedirects(HttpClient.Redirect.ALWAYS).build()).build());
            HttpClient injected = HttpClient.newBuilder().followRedirects(HttpClient.Redirect.NEVER).build();
            try (FeedbackClient ignored = FeedbackClient.builder().baseUrl(wire.base).publishableKey("key").httpClient(injected).build()) {}
            check(injected.send(HttpRequest.newBuilder(URI.create(wire.base)).build(), HttpResponse.BodyHandlers.discarding()).statusCode() == 200, "injected client remains usable");
            passed("url_credential_exclusivity_injected_pool_ownership");
        }
    }
    static void boundaries(String scenario) throws Exception {
        if (scenario.equals("all") || scenario.equals("draft")) {
            Node node = JSON.readValue("{\"id\":\"wait-draft\",\"kind\":\"wait\",\"config\":null}", Node.class);
            check(node.asDraftNode().configPresence().isPresent() && node.asDraftNode().getConfig() == null, "unconstrained draft null decodes");
            check(fails(() -> JSON.readValue("{\"id\":null,\"kind\":\"wait\",\"config\":null}", Node.class)) instanceof IOException, "typed nonnullable draft id stays strict");
            check(!ModelJson.matches("ValidNode", JSON.readTree("{\"id\":\"wait-draft\",\"kind\":\"wait\",\"config\":null}")), "null config does not flatten typed node union");
            passed("boundary_unconstrained_null_and_strict_typed_fields");
        }
        if (scenario.equals("all") || scenario.equals("credentials")) {
            try (Wire wire = new Wire()) {
                String secret = "private-bearer-marker\r\nleaked";
                for (int mode = 0; mode < 3; mode++) {
                    var builder = ManyForgeClient.builder().baseUrl(wire.base);
                    if (mode == 0) builder.accessToken(secret);
                    else if (mode == 1) builder.tokenProvider(() -> CompletableFuture.completedFuture(secret));
                    else builder.session(Session.fromTokenPair(new TokenPair().accessToken(secret).refreshToken("refresh").expiresIn(300)));
                    try (ManyForgeClient client = builder.build()) {
                        for (boolean async : List.of(false, true)) {
                            Throwable failure = fails(() -> { if (async) client.account().getAsync().get(); else client.account().get(); });
                            StringWriter text = new StringWriter(); failure.printStackTrace(new PrintWriter(text));
                            check(!text.toString().contains("private-bearer-marker") && !text.toString().contains("leaked"), "credential absent from default stack trace and causes");
                            Throwable cause = failure instanceof ExecutionException ? failure.getCause() : failure;
                            check(cause instanceof IllegalArgumentException, "malformed bearer rejected before native header builder");
                        }
                    }
                }
                check(wire.requests.get() == 0, "malformed credentials never reach HTTP");
            }
            passed("boundary_fixed_provider_session_credential_redaction");
        }
        if (scenario.equals("all") || scenario.equals("session-origin")) {
            for (boolean expired : List.of(false, true)) {
                try (Wire first = new Wire(); Wire second = new Wire()) {
                    first.status.set(401); second.status.set(401);
                    byte[] rejected = "{\"code\":\"UNAUTHORIZED\",\"message\":\"expected fixture rejection\"}".getBytes(StandardCharsets.UTF_8);
                    first.body.set(rejected); second.body.set(rejected);
                    Session session = Session.fromTokenPair(new TokenPair().accessToken("owned-access")
                        .refreshToken("owned-refresh").expiresIn(expired ? 1 : 300));
                    try (ManyForgeClient owner = client(first.base, session); ManyForgeClient other = client(second.base, session)) {
                        api(401, () -> owner.account().get());
                        check(first.requests.get() == 1 && "Bearer owned-access".equals(first.authorization.get()), "session first used at its owner origin");
                        if (expired) Thread.sleep(1100);
                        for (boolean async : List.of(false, true)) {
                            Throwable failure = fails(() -> { if (async) other.account().getAsync().get(); else other.account().get(); });
                            Throwable cause = failure instanceof ExecutionException ? failure.getCause() : failure;
                            check(cause instanceof IllegalArgumentException, "cross-origin session rejected before token access or rotation");
                        }
                        check(second.requests.get() == 0, "neither access nor refresh credentials reach another origin");
                        check(session.isValid(), "origin mismatch does not invalidate the owning session");
                    }
                }
            }
            passed("boundary_session_origin_ownership");
        }
        if (scenario.equals("all") || scenario.equals("provider")) {
            try (Wire wire = new Wire()) {
                wire.status.set(204);
                for (boolean async : List.of(false, true)) {
                    CompletableFuture<String> token = new CompletableFuture<>();
                    try (ManyForgeClient client = ManyForgeClient.builder().baseUrl(wire.base).tokenProvider(() -> token).build()) {
                        var params = RootBusinessesCreateParams.builder().businessCreateRequest(new BusinessCreateRequest().name("must not send")).build();
                        RequestOptions options = new RequestOptions(Duration.ofMillis(100));
                        ExecutorService caller = Executors.newSingleThreadExecutor();
                        try {
                            Future<Throwable> outcome = caller.submit(() -> fails(() -> {
                                if (async) client.businesses().createAsync(params, options).get();
                                else client.businesses().create(params, options);
                            }));
                            Throwable failure = outcome.get(2, TimeUnit.SECONDS);
                            if (failure instanceof ExecutionException) failure = failure.getCause();
                            check(failure instanceof HttpTimeoutException, "credential wait consumes call timeout");
                            token.complete("late-token");
                            check(wire.requests.get() == 0, "late provider cannot start timed-out mutation");
                        } finally { caller.shutdownNow(); }
                    } finally { token.complete("cleanup-token"); }
                }
            }
            passed("boundary_blocking_async_provider_deadlines");
        }
        if (scenario.equals("all") || scenario.equals("remaining")) {
            for (boolean streaming : List.of(false, true)) {
                try (Wire wire = new Wire()) {
                    wire.status.set(streaming ? 200 : 204);
                    wire.body.set("email\nlate@example.invalid\n".getBytes(StandardCharsets.UTF_8));
                    if (streaming) wire.delayBodyMillis.set(350); else wire.delayMillis.set(350);
                    CompletableFuture<String> token = new CompletableFuture<>();
                    try (ManyForgeClient client = ManyForgeClient.builder().baseUrl(wire.base).tokenProvider(() -> token).build()) {
                        RequestOptions options = new RequestOptions(Duration.ofMillis(500));
                        CompletableFuture<?> exchange = streaming
                            ? client.business(UUID.randomUUID()).mailing().subscribers().exportCsvAsync(BusinessMailingSubscribersExportCsvParams.builder().lid(UUID.randomUUID()).build(), options)
                            : client.businesses().createAsync(RootBusinessesCreateParams.builder().businessCreateRequest(new BusinessCreateRequest().name("deadline")).build(), options);
                        Thread.sleep(300);
                        token.complete("late-token");
                        Throwable failure = fails(() -> {
                            Object value = exchange.get(2, TimeUnit.SECONDS);
                            if (value instanceof InputStream stream) try (stream) { stream.readAllBytes(); }
                        });
                        if (failure instanceof ExecutionException) failure = failure.getCause();
                        check(failure instanceof HttpTimeoutException, "HTTP and stream inherit only time remaining after credentials");
                        check(wire.requests.get() == 1, "remaining-deadline exchange sends once");
                    }
                }
            }
            passed("boundary_remaining_http_and_stream_deadline");
        }
        if (scenario.equals("all") || scenario.equals("persistence")) {
            try (Wire wire = new Wire()) {
                wire.body.set(JSON.writeValueAsBytes(new TokenPair().accessToken("rotated").refreshToken("new-refresh").expiresIn(300)));
                CompletableFuture<Void> persisted = new CompletableFuture<>();
                CountDownLatch rotating = new CountDownLatch(1);
                AtomicInteger rotations = new AtomicInteger();
                Session owner = Session.fromTokenPair(new TokenPair().accessToken("old").refreshToken("old-refresh").expiresIn(1), pair -> {
                    rotations.incrementAndGet(); rotating.countDown(); return persisted;
                });
                try (ManyForgeClient client = client(wire.base, owner)) {
                    Thread.sleep(1000);
                    var params = RootBusinessesCreateParams.builder().businessCreateRequest(new BusinessCreateRequest().name("shared waiter")).build();
                    CompletableFuture<?> survivor = client.businesses().createAsync(params, new RequestOptions(Duration.ofSeconds(5)));
                    check(rotating.await(2, TimeUnit.SECONDS), "refresh reaches persistence");
                    CompletableFuture<?> cancelled = client.businesses().createAsync(params);
                    cancelled.cancel(true);
                    Throwable async = fails(() -> client.businesses().createAsync(params, new RequestOptions(Duration.ofMillis(100))).get(2, TimeUnit.SECONDS));
                    check(async instanceof ExecutionException && async.getCause() instanceof HttpTimeoutException, "async persistence deadline");
                    ExecutorService caller = Executors.newSingleThreadExecutor();
                    try {
                        Throwable blocking = caller.submit(() -> fails(() -> client.businesses().create(params, new RequestOptions(Duration.ofMillis(100))))).get(2, TimeUnit.SECONDS);
                        check(blocking instanceof HttpTimeoutException, "blocking persistence deadline");
                    } finally { caller.shutdownNow(); }
                    check(wire.requests.get() == 1 && !survivor.isDone(), "expired persistence waiters send no mutation");
                    wire.status.set(204); wire.body.set(new byte[0]);
                    persisted.complete(null);
                    survivor.get(2, TimeUnit.SECONDS);
                    check(owner.isValid() && rotations.get() == 1 && wire.requests.get() == 2 && cancelled.isCancelled(), "independent survivor retains shared rotation, only it sends");
                } finally { persisted.complete(null); }
            }
            passed("boundary_persistence_deadlines_preserve_shared_rotation");
        }
    }
    static String hmac(String secret, byte[] bytes) throws Exception {
        Mac mac = Mac.getInstance("HmacSHA256"); mac.init(new SecretKeySpec(secret.getBytes(StandardCharsets.UTF_8), "HmacSHA256"));
        return HexFormat.of().formatHex(mac.doFinal(bytes));
    }
    static final class Wire implements AutoCloseable {
        final HttpServer server;
        final ExecutorService executor = Executors.newCachedThreadPool();
        final String base;
        final AtomicInteger status = new AtomicInteger(200), requests = new AtomicInteger(), delayMillis = new AtomicInteger(), delayBodyMillis = new AtomicInteger();
        final AtomicReference<byte[]> body = new AtomicReference<>(new byte[0]), requestBody = new AtomicReference<>();
        final AtomicReference<String> target = new AtomicReference<>(), signature = new AtomicReference<>(), authorization = new AtomicReference<>(), cookie = new AtomicReference<>();
        final AtomicReference<String> signingSecret = new AtomicReference<>();
        Wire() throws IOException {
            server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
            base = "http://127.0.0.1:" + server.getAddress().getPort();
            server.setExecutor(executor);
            server.createContext("/", exchange -> {
                try (exchange) {
                    requestBody.set(exchange.getRequestBody().readAllBytes()); target.set(exchange.getRequestURI().toASCIIString());
                    signature.set(exchange.getRequestHeaders().getFirst("X-Feedback-Signature"));
                    authorization.set(exchange.getRequestHeaders().getFirst("Authorization")); cookie.set(exchange.getRequestHeaders().getFirst("Cookie"));
                    requests.incrementAndGet();
                    if (signingSecret.get() != null) {
                        String header = signature.get();
                        boolean valid = false;
                        try {
                            String timestamp = header.substring(2, header.indexOf(','));
                            ByteArrayOutputStream canonical = new ByteArrayOutputStream();
                            canonical.writeBytes((timestamp + "." + exchange.getRequestMethod() + "." + target.get() + ".").getBytes(StandardCharsets.UTF_8));
                            canonical.writeBytes(requestBody.get());
                            valid = header.equals("t=" + timestamp + ",v1=" + hmac(signingSecret.get(), canonical.toByteArray()));
                        } catch (Exception malformed) {}
                        if (!valid) { exchange.sendResponseHeaders(401, -1); return; }
                    }
                    try { Thread.sleep(delayMillis.get()); } catch (InterruptedException interrupted) { Thread.currentThread().interrupt(); }
                    byte[] response = body.get();
                    exchange.getResponseHeaders().set("Content-Type", "application/json"); exchange.getResponseHeaders().set("X-Request-Id", "java-wire-request");
                    exchange.sendResponseHeaders(status.get(), status.get() == 204 ? -1 : response.length);
                    if (status.get() != 204) {
                        int pause = delayBodyMillis.get();
                        if (pause > 0 && response.length > 0) {
                            exchange.getResponseBody().write(response, 0, 1);
                            exchange.getResponseBody().flush();
                            try { Thread.sleep(pause); } catch (InterruptedException interrupted) { Thread.currentThread().interrupt(); }
                            exchange.getResponseBody().write(response, 1, response.length - 1);
                        } else {
                            exchange.getResponseBody().write(response);
                        }
                    }
                } catch (IOException cancelled) { /* A cancelled client is expected to close its connection. */ }
            });
            server.start();
        }
        void awaitRequests(int count) throws InterruptedException {
            long until = System.nanoTime() + Duration.ofSeconds(10).toNanos();
            while (requests.get() < count && System.nanoTime() < until) Thread.sleep(10);
            check(requests.get() >= count, "request reached wire fixture");
        }
        public void close() { server.stop(0); executor.shutdownNow(); }
    }
}
