// Copyright (c) 2026 ManyForge contributors. SPDX-License-Identifier: MIT
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	mf "github.com/joshrendek/manyforge/sdk/go"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}
func check(ok bool, name string) {
	if !ok {
		panic(name)
	}
}
func noerr(err error) {
	if err != nil {
		panic(err)
	}
}
func api(err error, status int) *mf.APIError {
	var value *mf.APIError
	check(errors.As(err, &value), "expected typed API error")
	check(value.Status == status, "unexpected HTTP status")
	return value
}
func login(ctx context.Context, base, email, password string) mf.TokenPair {
	c := must(mf.NewClient(base))
	defer c.Close()
	return must(c.Auth.Login(ctx, mf.LoginRequest{Email: email, Password: password}))
}
func managed(ctx context.Context, base, email, password string) *mf.Client {
	pair := login(ctx, base, email, password)
	session := must(mf.SessionFromTokenPair(pair, nil))
	return must(mf.NewClient(base, mf.WithSession(session)))
}
func main() {
	check(len(os.Args) == 2, "fixture filename required")
	var f map[string]any
	noerr(json.Unmarshal(must(os.ReadFile(os.Args[1])), &f))
	field := func(key string) string {
		v, ok := f[key].(string)
		check(ok && v != "", "missing fixture field "+key)
		return v
	}
	ctx := context.Background()
	base := field("base_url")
	client := managed(ctx, base, field("email"), field("password"))
	defer client.Close()
	assertions := []string{}
	business := must(client.Businesses.Create(ctx, mf.BusinessCreateRequest{Name: "Go installed consumer"}))
	scope := client.Business(business.Id)
	setup := must(scope.Mailing.Setup.Get(ctx))
	setupBlocked, relayOnly := false, false
	for _, item := range setup.Checks {
		if item.Id == "outbound_enabled" {
			setupBlocked = item.Status == "blocked"
		}
		if item.Id == "smtp_relay" {
			relayOnly = len(item.RequiredFor) == 1 && item.RequiredFor[0] == "relay"
		}
	}
	check(setupBlocked && relayOnly, "provider-scoped outbound setup")
	contact := must(scope.Contacts.Create(ctx, mf.CreateContact{PrimaryEmail: "sdk-go-contact@example.test", DisplayName: mf.Value("Before")}))
	read := must(scope.Contacts.Get(ctx, contact.Id))
	check(read.PrimaryEmail == contact.PrimaryEmail, "contact read")
	updated := must(scope.Contacts.Update(ctx, contact.Id, mf.UpdateContact{DisplayName: mf.Value("After")}))
	name, _ := updated.DisplayName.Get()
	check(name == "After", "contact update")
	expected := map[string]bool{contact.Id: true}
	for i := range 3 {
		v := must(scope.Contacts.Create(ctx, mf.CreateContact{PrimaryEmail: fmt.Sprintf("sdk-go-%d@example.test", i)}))
		expected[v.Id] = true
	}
	seen := map[string]bool{}
	iterator := scope.Contacts.Iter(ctx, mf.BusinessContactsListParams{Limit: 1})
	for iterator.Next() {
		v := iterator.Current()
		check(!seen[v.Id] && expected[v.Id], "pagination uniqueness")
		check(v.TenantRootId == contact.TenantRootId, "pagination tenant")
		seen[v.Id] = true
	}
	noerr(iterator.Err())
	check(len(seen) == len(expected), "pagination completion")
	other := managed(ctx, base, field("other_email"), field("other_password"))
	defer other.Close()
	_, err := other.Business(business.Id).Contacts.Get(ctx, contact.Id)
	isolated := api(err, 404)
	_, err = other.Business(business.Id).Contacts.Get(ctx, "00000000-0000-4000-8000-000000000001")
	missing := api(err, 404)
	check(isolated.Code == "NOT_FOUND" && isolated.Code == missing.Code && isolated.RequestID != "" && missing.RequestID != "", "uniform RLS not found")
	assertions = append(assertions, "real login/business/CRM CRUD/limit-one iteration/RLS")
	tickets := client.Business(field("ticket_business_id")).Tickets
	ticket := must(tickets.Update(ctx, field("ticket_id"), mf.PatchTicket{Priority: mf.Value(mf.PatchTicketPriority("high"))}))
	assignee, _ := ticket.AssigneePrincipalId.Get()
	check(assignee == field("principal_id"), "omitted assignee preserved")
	ticket = must(tickets.Update(ctx, field("ticket_id"), mf.PatchTicket{AssigneePrincipalId: mf.Null[string]()}))
	check(ticket.AssigneePrincipalId.IsNull(), "explicit null clears")
	ticket = must(tickets.Update(ctx, field("ticket_id"), mf.PatchTicket{AssigneePrincipalId: mf.Value(field("principal_id"))}))
	assignee, _ = ticket.AssigneePrincipalId.Get()
	check(assignee == field("principal_id"), "value assigns")
	assertions = append(assertions, "real ticket absent/null/value")

	concurrentBase := field("session_base_url")
	pair := login(ctx, concurrentBase, field("email"), field("password"))
	var rotations atomic.Int32
	session := must(mf.SessionFromTokenPair(pair, func(context.Context, mf.TokenPair) error { rotations.Add(1); return nil }))
	concurrent := must(mf.NewClient(concurrentBase, mf.WithSession(session)))
	defer concurrent.Close()
	time.Sleep(time.Duration(pair.ExpiresIn) * time.Second)
	var group sync.WaitGroup
	failures := make(chan error, 8)
	for range 8 {
		group.Add(1)
		go func() { defer group.Done(); _, e := concurrent.Account.Get(ctx); failures <- e }()
	}
	group.Wait()
	close(failures)
	for e := range failures {
		noerr(e)
	}
	check(rotations.Load() == 1, "single real refresh")
	lossBase := field("lossy_base_url")
	lostPair := login(ctx, lossBase, field("email"), field("password"))
	lostSession := must(mf.SessionFromTokenPair(lostPair, nil))
	lost := must(mf.NewClient(lossBase, mf.WithSession(lostSession)))
	defer lost.Close()
	time.Sleep(time.Duration(lostPair.ExpiresIn) * time.Second)
	_, err = lost.Account.Get(ctx)
	var sessionErr *mf.SessionError
	check(errors.As(err, &sessionErr), "lost refresh session error")
	_, err = lost.Account.Get(ctx)
	check(errors.As(err, &sessionErr), "lost refresh never reused")
	assertions = append(assertions, "one concurrent real refresh/lost consumed refresh invalidation")

	board := must(scope.Feedback.Boards.Create(ctx, mf.BoardCreate{Name: "Go public", IsPublic: mf.Value(true)}))
	key := must(scope.Feedback.Keys.Create(ctx, board.Id, mf.IngestKeyCreate{}))
	secret, ok := key.Secret.Get()
	check(ok && secret != "", "feedback signing secret")
	feedback := must(mf.NewFeedbackClient(base, mf.WithPublishableKey(key.PublishableKey)))
	defer feedback.Close()
	signed := must(mf.NewSignedFeedbackClient(base, mf.WithPublishableKey(key.PublishableKey), mf.WithSigningSecret(secret)))
	defer signed.Close()
	anonymous := must(feedback.Posts.Create(ctx, mf.PublicSubmit{Title: "Go anonymous"}))
	check(!anonymous.IdentityVerified, "unsigned identity")
	body := mf.PublicSubmit{Title: "Go signed", AuthorIdentity: mf.Value("go@example.test"), IdempotencyKey: mf.Value("go-stable-idempotency")}
	first := must(signed.Posts.Create(ctx, body))
	check(first.IdentityVerified && !first.Deduped, "signed identity")
	duplicate := must(signed.Posts.Create(ctx, body))
	check(duplicate.Deduped && duplicate.Id == first.Id, "same bytes dedupe")
	body.Title = "Changed"
	_, err = signed.Posts.Create(ctx, body)
	api(err, 409)
	vote := must(feedback.Posts.Vote(ctx, anonymous.Id, mf.PublicVote{VoterIdentity: "go-voter"}))
	check(vote.Voted, "initial vote")
	vote = must(feedback.Posts.Vote(ctx, anonymous.Id, mf.PublicVote{VoterIdentity: "go-voter"}))
	check(!vote.Voted, "duplicate vote does not unvote")
	must(signed.Posts.List(ctx, mf.FeedbackPostsListParams{Author: mf.Value("go@example.test"), VoterIdentity: mf.Value("encoded +/&? punctuation")}))
	must(scope.Feedback.Keys.Revoke(ctx, key.Id))
	_, err = feedback.Posts.List(ctx, mf.FeedbackPostsListParams{})
	api(err, 401)
	assertions = append(assertions, "real feedback anonymous/signed/idempotency/conflict/vote/revocation")

	crash := must(scope.Telemetry.Clients.Create(ctx, mf.TelemetryClientCreate{TelemetryCrashClientCreate: &mf.TelemetryCrashClientCreate{Kind: "crash", Name: "Go crash"}}))
	telemetry := must(mf.NewTelemetryClient(base, mf.WithPublishableKey(crash.PublishableKey)))
	defer telemetry.Close()
	event := mf.CrashEvent{OccurredAt: mf.Value(time.Now().UTC()), Platform: mf.Value("go"), Signature: mf.Value("go-smoke"), Payload: mf.Value[interface{}](map[string]any{"counter": int64(9007199254740993)})}
	old := event
	old.OccurredAt = mf.Value(time.Now().Add(-8 * 24 * time.Hour).UTC())
	batch := mf.TelemetryIngestRequest{Crash: mf.Value([]mf.CrashEvent{event, old})}
	counts := must(telemetry.Ingest(ctx, batch))
	check(counts.Accepted == 1 && counts.Dropped == 1, "partial acceptance")
	_, err = telemetry.Ingest(ctx, mf.TelemetryIngestRequest{Crash: mf.Value(make([]mf.CrashEvent, 1001))})
	api(err, 400)
	required := must(scope.Telemetry.Clients.Create(ctx, mf.TelemetryClientCreate{TelemetryCrashClientCreate: &mf.TelemetryCrashClientCreate{Kind: "crash", Name: "Go signed crash", RequireSignature: mf.Value(true)}}))
	requiredSecret, _ := required.Secret.Get()
	unsignedRequired := must(mf.NewTelemetryClient(base, mf.WithPublishableKey(required.PublishableKey)))
	defer unsignedRequired.Close()
	_, err = unsignedRequired.Ingest(ctx, batch)
	api(err, 401)
	signedTelemetry := must(mf.NewSignedTelemetryClient(base, mf.WithPublishableKey(required.PublishableKey), mf.WithSigningSecret(requiredSecret)))
	defer signedTelemetry.Close()
	counts = must(signedTelemetry.Ingest(ctx, batch))
	check(counts.Accepted == 1 && counts.Dropped == 1, "required telemetry signature")
	badTelemetry := must(mf.NewSignedTelemetryClient(base, mf.WithPublishableKey(required.PublishableKey), mf.WithSigningSecret("wrong-issued-secret")))
	defer badTelemetry.Close()
	_, err = badTelemetry.Ingest(ctx, batch)
	api(err, 401)
	lossyTelemetry := must(mf.NewTelemetryClient(field("lossy_telemetry_base_url"), mf.WithPublishableKey(crash.PublishableKey)))
	defer lossyTelemetry.Close()
	_, err = lossyTelemetry.Ingest(ctx, mf.TelemetryIngestRequest{Crash: mf.Value([]mf.CrashEvent{event})})
	check(err != nil, "accepted lost telemetry response is not replayed")
	site := must(scope.Telemetry.Clients.Create(ctx, mf.TelemetryClientCreate{TelemetryAnalyticsClientCreate: &mf.TelemetryAnalyticsClientCreate{Kind: "analytics", Name: "Go analytics", AllowedOrigins: []string{field("allowed_origin")}}}))
	analytics := must(mf.NewAnalyticsClient(base, mf.WithPublishableKey(site.PublishableKey), mf.WithSourceOrigin(field("allowed_origin"))))
	defer analytics.Close()
	allowedEvent := "go_allowed_smoke"
	deniedEvent := "go_denied_smoke"
	noerr(analytics.Collect(ctx, mf.AnalyticsCollectRequest{N: mf.Value(allowedEvent), P: mf.Value("/go-smoke")}))
	denied := must(mf.NewAnalyticsClient(base, mf.WithPublishableKey(site.PublishableKey), mf.WithSourceOrigin(field("denied_origin"))))
	defer denied.Close()
	noerr(denied.Collect(ctx, mf.AnalyticsCollectRequest{N: mf.Value(deniedEvent), P: mf.Value("/go-denied")}))
	summary := must(scope.Analytics.Get(ctx, mf.BusinessAnalyticsGetParams{ClientId: site.Id, Days: mf.Value(int32(7))}))
	check(len(summary.From) == 10 && len(summary.To) == 10, "summary date-only")
	for _, day := range summary.Series {
		check(len(day.Date) == 10, "series date-only")
	}
	assertions = append(assertions, "real telemetry partial/bounds/signature/no-replay/analytics origin/date-only")

	list := must(scope.Mailing.Lists.Create(ctx, mf.ListInput{Name: "Go CSV", DoubleOptIn: mf.Value(false)}))
	imported := must(scope.Mailing.Subscribers.ImportCsv(ctx, list.Id, mf.BusinessMailingSubscribersImportCsvParams{ConsentAttested: true, SkipConfirmation: mf.Value(true), File: mf.Upload{Filename: "subscribers.csv", Reader: strings.NewReader("email,name\ngo-csv@example.test,Go CSV\n"), ContentType: "text/csv"}}))
	check(imported.Imported == 1, "real CSV import")
	stream := must(scope.Mailing.Subscribers.ExportCsv(ctx, list.Id, mf.WithTimeout(time.Minute)))
	csv := must(io.ReadAll(stream))
	noerr(stream.Close())
	check(bytes.Contains(csv, []byte("email")) && bytes.Contains(csv, []byte("go-csv@example.test")), "streamed CSV export")
	_, err = scope.Mailing.Subscribers.ImportCsv(ctx, list.Id, mf.BusinessMailingSubscribersImportCsvParams{ConsentAttested: true, SkipConfirmation: mf.Value(true), File: mf.Upload{Filename: "oversized.csv", Reader: strings.NewReader(strings.Repeat("x", 11<<20))}})
	var oversized *mf.APIError
	check(errors.As(err, &oversized) && (oversized.Status == 400 || oversized.Status == 413), "oversized import rejection")
	assertions = append(assertions, "real consent CSV import/stream export/oversize rejection")
	overlappingList := must(scope.Mailing.Lists.Create(ctx, mf.ListInput{Name: "Go reporting overlap", DoubleOptIn: mf.Value(false)}))
	must(scope.Mailing.Subscribers.ImportCsv(ctx, overlappingList.Id, mf.BusinessMailingSubscribersImportCsvParams{ConsentAttested: true, SkipConfirmation: mf.Value(true), File: mf.Upload{Filename: "duplicate.csv", Reader: strings.NewReader("email,name\ngo-csv@example.test,Go CSV\n"), ContentType: "text/csv"}}))
	report := must(client.Mailing.Reporting.Get(ctx, mf.RootMailingReportingGetParams{BusinessId: mf.Value(business.Id)}))
	check(report.BusinessCount == 1 && report.ActiveSubscribers == 1, "report deduplicates overlapping list membership")
	check(report.SubscriberNetAdditions == 1 && !report.SubscriberWindowComplete, "report exposes genuine partial history")
	_, hasOpenRate := report.OpenRate.Percent.Get()
	check(report.OpenRate.Denominator == 0 && !hasOpenRate, "empty cohort is not a measured zero rate")
	assertions = append(assertions, "real mailing report deduplication/history/null denominator")
	redirect := must(mf.NewClient(field("redirect_base_url"), mf.WithAccessToken("disposable-redirect-token")))
	defer redirect.Close()
	_, err = redirect.Account.Get(ctx)
	var redirectError *mf.APIError
	check(errors.As(err, &redirectError) && redirectError.Status >= 300 && redirectError.Status < 400, "redirect blocked")
	transportEdges()
	sessionWaiterEdges()
	assertions = append(assertions, "native transport cancellation/errors/redaction/credentials/signature/serialization/session edges")
	result := map[string]any{"business_id": business.Id, "contact_id": contact.Id, "analytics_client_id": site.Id, "analytics_event_name": allowedEvent, "analytics_denied_event_name": deniedEvent, "board_id": board.Id, "feedback_publishable_key": key.PublishableKey, "assertions": assertions}
	noerr(os.WriteFile(field("result_path"), must(json.MarshalIndent(result, "", "  ")), 0600))
}

func transportEdges() {
	ctx := context.Background()
	_, err := mf.NewClient("http://localhost", mf.WithAccessToken("token"), mf.WithTokenProvider(func(context.Context) (string, error) { return "provider", nil }))
	check(err != nil, "credential modes mutually exclusive")
	_, err = mf.NewFeedbackClient("http://localhost", mf.WithPublishableKey("public"), mf.WithAccessToken("management"))
	check(err != nil, "public rejects management credentials")
	_, err = mf.NewFeedbackClient("http://localhost", mf.WithPublishableKey("public"), mf.WithSigningSecret("secret"))
	check(err != nil, "unsigned rejects signing secret")
	_, err = mf.NewSignedFeedbackClient("http://localhost", mf.WithPublishableKey("public"))
	check(err != nil, "signed requires signing secret")
	_, err = mf.NewAnalyticsClient("http://localhost", mf.WithPublishableKey("public"))
	check(err != nil, "server analytics requires source origin")
	for _, base := range []string{"https://example.test/api/v1", "https://u:p@example.test", "https://example.test/?q=x", "https://example.test/#x", "relative"} {
		_, err := mf.NewClient(base)
		check(err != nil, "invalid base rejected")
	}
	patch := must(json.Marshal(mf.PatchTicket{AssigneePrincipalId: mf.Null[string](), Tags: mf.Value([]string{})}))
	check(string(patch) == `{"assignee_principal_id":null,"tags":[]}`, "null and empty array wire")
	var state struct {
		False  mf.Optional[bool]   `json:"false,omitzero"`
		Zero   mf.Optional[int64]  `json:"zero,omitzero"`
		Absent mf.Optional[string] `json:"absent,omitzero"`
	}
	state.False = mf.Value(false)
	state.Zero = mf.Value(int64(0))
	check(string(must(json.Marshal(state))) == `{"false":false,"zero":0}`, "false zero absent wire")
	var enum mf.TicketPriority
	noerr(json.Unmarshal([]byte(`"future-priority"`), &enum))
	check(string(enum) == "future-priority", "unknown enum")
	var imported mf.ImportResult
	noerr(json.Unmarshal([]byte(`{"imported":9007199254740993,"skipped":0,"errors":[],"future":true}`), &imported))
	check(imported.Imported == 9007199254740993 && bytes.Contains(must(json.Marshal(imported)), []byte("9007199254740993")), "lossless int64 unknown fields")
	var date mf.Date
	noerr(json.Unmarshal([]byte(`"2026-09-07"`), &date))
	check(string(date) == "2026-09-07", "date round trip")
	check(json.Unmarshal([]byte(`"2026-02-30"`), &date) != nil, "invalid date rejected")
	var refreshes atomic.Int32
	var calls atomic.Int32
	var signatures atomic.Int32
	var original []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/api/v1/auth/refresh" {
			refreshes.Add(1)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"access_token":"rotated","refresh_token":"rotated-refresh","expires_in":300}`)
			return
		}
		if strings.Contains(r.URL.Path, "/mailing/s2s/") {
			body := must(io.ReadAll(r.Body))
			timestamp := r.Header.Get("X-Mailing-Timestamp")
			mac := hmac.New(sha256.New, []byte("entire-issued-secret"))
			io.WriteString(mac, timestamp+"."+r.Method+"."+r.URL.EscapedPath()+".")
			mac.Write(body)
			check(timestamp != "" && r.Header.Get("X-Mailing-Signature") == hex.EncodeToString(mac.Sum(nil)), "mailing separate headers and escaped path signature")
			check(r.Header.Get("Authorization") == "" && r.Header.Get("Cookie") == "", "mailing public credential isolation")
			io.WriteString(w, `{"created":true,"status":"future_status","subscriber_id":"subscriber","future":true}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/subscribers/import") {
			noerr(r.ParseMultipartForm(1 << 20))
			defer r.MultipartForm.RemoveAll()
			check(r.FormValue("consent_attested") == "true" && r.FormValue("skip_confirmation") == "false", "consent and explicit false multipart fields preserved")
			file, header, err := r.FormFile("file")
			noerr(err)
			defer file.Close()
			check(header.Filename == "data.csv" && string(must(io.ReadAll(file))) == "email\nfixture@example.test\n", "multipart filename and file bytes")
			io.WriteString(w, `{"imported":9007199254740993,"skipped":0,"errors":[],"unknown":true}`)
			return
		}
		if strings.Contains(r.URL.Path, "/telemetry/ingest/") {
			body := must(io.ReadAll(r.Body))
			check(bytes.Contains(body, []byte(`"counter":9007199254740993`)) && bytes.Contains(body, []byte(`"enabled":false`)) && bytes.Contains(body, []byte(`"zero":0`)) && bytes.Contains(body, []byte(`"items":[]`)), "native JSON lossless int64/false/zero/empty array")
			check(r.Header.Get("Authorization") == "" && r.Header.Get("Cookie") == "", "telemetry credential isolation")
			io.WriteString(w, `{"accepted":1,"dropped":0}`)
			return
		}
		if r.URL.Path == "/a/e" {
			var body map[string]json.RawMessage
			noerr(json.NewDecoder(r.Body).Decode(&body))
			check(string(body["k"]) == `"analytics-key"` && r.Header.Get("Origin") == "https://source.example", "analytics body key and explicit source origin")
			w.WriteHeader(204)
			return
		}
		if strings.Contains(r.URL.Path, "/feedback/public/") {
			check(r.Header.Get("Authorization") == "" && r.Header.Get("Cookie") == "", "public credential isolation")
			body := must(io.ReadAll(r.Body))
			signature := r.Header.Get("X-Feedback-Signature")
			if signature != "" {
				parts := strings.Split(signature, ",")
				timestamp := strings.TrimPrefix(parts[0], "t=")
				mac := hmac.New(sha256.New, []byte("entire-issued-secret"))
				io.WriteString(mac, timestamp+"."+r.Method+"."+r.RequestURI+".")
				mac.Write(body)
				check(parts[1] == "v1="+hex.EncodeToString(mac.Sum(nil)), "exact target/body signature")
				signatures.Add(1)
			}
			if r.Method == "POST" {
				if original == nil {
					original = append([]byte(nil), body...)
				} else {
					check(bytes.Equal(original, body), "stable feedback bytes")
				}
				io.WriteString(w, `{"id":"post","title":"stable","status":"open","vote_count":0,"deduped":false,"identity_verified":true}`)
			} else {
				check(strings.Contains(r.RequestURI, "key%2Fpunctuation"), "path escaped once")
				io.WriteString(w, `{"items":[]}`)
			}
			return
		}
		switch r.Header.Get("Authorization") {
		case "Bearer large":
			w.WriteHeader(502)
			io.WriteString(w, strings.Repeat("credential-sensitive", 5000))
			return
		case "Bearer future":
			w.WriteHeader(422)
			io.WriteString(w, `{"code":"FUTURE_SERVER_CODE","message":"new error","details":{"field":"value"}}`)
			return
		case "Bearer cancel":
			// Let net/http observe the peer closing after this request body.
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
			return
		case "Bearer malformed":
			io.WriteString(w, `{"not":"an account"}`)
			return
		}
		w.Header().Set("X-Request-Id", "fixture-request")
		w.WriteHeader(401)
		io.WriteString(w, `{"code":"REAUTHENTICATION_FAILED","message":"credential-sensitive","future":{"reason":true}}`)
	}))
	defer server.Close()
	jar := must(cookiejar.New(nil))
	origin := must(url.Parse(server.URL))
	jar.SetCookies(origin, []*http.Cookie{{Name: "session", Value: "must-not-send"}})
	injected := &http.Client{Jar: jar}
	public := must(mf.NewSignedFeedbackClient(server.URL, mf.WithPublishableKey("key/punctuation"), mf.WithSigningSecret("entire-issued-secret"), mf.WithHTTPClient(injected)))
	defer public.Close()
	must(public.Posts.List(ctx, mf.FeedbackPostsListParams{Author: mf.Value("a +/?&"), VoterIdentity: mf.Value("z +/?&")}))
	body := mf.PublicSubmit{Title: "stable", Body: mf.Value("<& exact >"), IdempotencyKey: mf.Value("stable")}
	must(public.Posts.Create(ctx, body))
	must(public.Posts.Create(ctx, body))
	check(signatures.Load() == 3, "all exact signatures verified")
	check(injected.Jar == jar && injected.CheckRedirect == nil, "caller client unchanged")
	mailing := must(mf.NewMailingServerClient(server.URL, mf.WithPublishableKey("mail/key"), mf.WithSigningSecret("entire-issued-secret"), mf.WithHTTPClient(injected)))
	defer mailing.Close()
	subscriber := must(mailing.Mailing.Subscribers.Create(ctx, mf.S2SSubscriptionInput{Email: "fixture@example.test", SkipConfirmation: mf.Value(false)}))
	check(subscriber.Status == "future_status", "unknown response enum accepted")
	wire := must(mf.NewClient(server.URL, mf.WithAccessToken("wire")))
	defer wire.Close()
	largeImport := must(wire.Business("wire").Mailing.Subscribers.ImportCsv(ctx, "list", mf.BusinessMailingSubscribersImportCsvParams{ConsentAttested: true, SkipConfirmation: mf.Value(false), File: mf.Upload{Filename: "data.csv", Reader: strings.NewReader("email\nfixture@example.test\n")}}))
	check(largeImport.Imported == 9007199254740993, "native response int64 is lossless")
	telemetry := must(mf.NewTelemetryClient(server.URL, mf.WithPublishableKey("telemetry-key"), mf.WithHTTPClient(injected)))
	defer telemetry.Close()
	must(telemetry.Ingest(ctx, mf.TelemetryIngestRequest{Crash: mf.Value([]mf.CrashEvent{{Payload: mf.Value[interface{}](map[string]any{"counter": int64(9007199254740993), "enabled": false, "zero": 0, "items": []string{}})}})}))
	analytics := must(mf.NewAnalyticsClient(server.URL, mf.WithPublishableKey("analytics-key"), mf.WithSourceOrigin("https://source.example"), mf.WithHTTPClient(injected)))
	defer analytics.Close()
	noerr(analytics.Collect(ctx, mf.AnalyticsCollectRequest{N: mf.Value("native")}))
	fixed := must(mf.NewClient(server.URL, mf.WithAccessToken("large")))
	defer fixed.Close()
	_, err = fixed.Account.Get(ctx)
	bounded := api(err, 502)
	check(len(bounded.RawBody) == 65536 && bounded.Truncated && !strings.Contains(bounded.Error(), "credential"), "bounded redacted API error")
	invalid := must(mf.NewClient(server.URL, mf.WithAccessToken("malformed")))
	defer invalid.Close()
	_, err = invalid.Account.Get(ctx)
	var invalidResponse *mf.InvalidResponseError
	check(errors.As(err, &invalidResponse), "invalid success payload distinct")
	cancellable := must(mf.NewClient(server.URL, mf.WithAccessToken("cancel")))
	defer cancellable.Close()
	before := calls.Load()
	_, err = cancellable.Businesses.Create(ctx, mf.BusinessCreateRequest{Name: "timeout mutation"}, mf.WithTimeout(30*time.Millisecond))
	check(errors.Is(err, context.DeadlineExceeded), "native timeout identity")
	check(calls.Load() == before+1, "timeout mutation no replay")
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	before = calls.Load()
	_, err = cancellable.Account.Get(cancelled)
	check(errors.Is(err, context.Canceled) && calls.Load() == before, "native pre-send cancellation")
	session := must(mf.SessionFromTokenPair(mf.TokenPair{AccessToken: "valid", RefreshToken: "refresh", ExpiresIn: 300}, nil))
	owner := must(mf.NewClient(server.URL, mf.WithSession(session)))
	defer owner.Close()
	_, err = owner.Account.Get(ctx)
	stepup := api(err, 401)
	check(stepup.Code == "REAUTHENTICATION_FAILED" && stepup.RequestID == "fixture-request" && refreshes.Load() == 0, "business 401 not refreshed")
	providerCalls := 0
	provider := must(mf.NewClient(server.URL, mf.WithTokenProvider(func(context.Context) (string, error) { providerCalls++; return "provider", nil })))
	defer provider.Close()
	_, err = provider.Account.Get(ctx)
	api(err, 401)
	_, err = provider.Account.Get(ctx)
	api(err, 401)
	check(providerCalls == 2, "provider evaluated per request")
	future := must(mf.NewClient(server.URL, mf.WithAccessToken("future")))
	defer future.Close()
	_, err = future.Account.Get(ctx)
	check(api(err, 422).Code == "FUTURE_SERVER_CODE", "unknown server code preserved")
	failed := must(mf.SessionFromTokenPair(mf.TokenPair{AccessToken: "old", RefreshToken: "old-refresh", ExpiresIn: 1}, func(context.Context, mf.TokenPair) error { return errors.New("persistence failed") }))
	failedOwner := must(mf.NewClient(server.URL, mf.WithSession(failed)))
	defer failedOwner.Close()
	time.Sleep(time.Second)
	_, err = failedOwner.Account.Get(ctx)
	var sessionError *mf.SessionError
	check(errors.As(err, &sessionError), "persistence invalidates session")
	_, err = failedOwner.Account.Get(ctx)
	check(errors.As(err, &sessionError) && refreshes.Load() == 1, "persistence failure never reuses token")
}

func sessionWaiterEdges() {
	ctx := context.Background()
	var refreshes atomic.Int32
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/refresh" {
			refreshes.Add(1)
			io.WriteString(w, `{"access_token":"replacement","refresh_token":"replacement-refresh","expires_in":300}`)
			return
		}
		reads.Add(1)
		check(r.Header.Get("Authorization") == "Bearer replacement", "read uses replacement token")
		io.WriteString(w, `{"items":[],"next_cursor":null}`)
	}))
	defer server.Close()
	rotating := make(chan struct{})
	persisted := make(chan struct{})
	session := must(mf.SessionFromTokenPair(mf.TokenPair{AccessToken: "old", RefreshToken: "old-refresh", ExpiresIn: 1}, func(context.Context, mf.TokenPair) error { close(rotating); <-persisted; return nil }))
	owner := must(mf.NewClient(server.URL, mf.WithSession(session)))
	defer owner.Close()
	time.Sleep(time.Second)
	finished := make(chan error, 1)
	go func() {
		_, err := owner.Business("scope").Contacts.List(ctx, mf.BusinessContactsListParams{})
		finished <- err
	}()
	select {
	case <-rotating:
	case <-time.After(5 * time.Second):
		panic("refresh did not reach persistence")
	}
	waiter, cancel := context.WithCancel(ctx)
	cancel()
	_, err := owner.Business("scope").Contacts.List(waiter, mf.BusinessContactsListParams{})
	check(errors.Is(err, context.Canceled), "cancelled session waiter")
	check(reads.Load() == 0 && refreshes.Load() == 1, "no read released before durable rotation")
	timedWaiter, cancelWaiter := context.WithTimeout(ctx, 30*time.Millisecond)
	_, err = owner.Business("scope").Contacts.List(timedWaiter, mf.BusinessContactsListParams{})
	cancelWaiter()
	check(errors.Is(err, context.DeadlineExceeded), "pending session waiter native cancellation")
	close(persisted)
	noerr(<-finished)
	check(reads.Load() == 1 && refreshes.Load() == 1, "waiter cancellation does not cancel rotation")
	pageCalls := 0
	pagination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pageCalls++
		check(r.URL.Query().Get("limit") == "1", "pagination preserves limit")
		check(strings.Contains(r.URL.EscapedPath(), "scope%2Fpunctuation"), "pagination preserves escaped scope")
		io.WriteString(w, `{"items":[],"next_cursor":"repeated"}`)
	}))
	defer pagination.Close()
	paged := must(mf.NewClient(pagination.URL, mf.WithAccessToken("token")))
	defer paged.Close()
	iterator := paged.Business("scope/punctuation").Contacts.Iter(ctx, mf.BusinessContactsListParams{Limit: 1})
	check(pageCalls == 0, "iterator is lazy")
	check(!iterator.Next(), "empty repeated page is not an item")
	var paginationError *mf.PaginationError
	check(errors.As(iterator.Err(), &paginationError) && pageCalls == 2, "repeated cursor stops")
}
