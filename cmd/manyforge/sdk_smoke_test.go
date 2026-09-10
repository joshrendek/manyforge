//go:build integration && sdk_smoke

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/manyforge/manyforge/internal/account"
	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/tenancy"
)

// These tests deliberately execute an installed consumer and the production binary.
// Neither product endpoints nor authentication are implemented by this fixture.
func TestSDKSmoke(t *testing.T) {
	binary := sdkSmokeRequiredEnv(t, "MANYFORGE_SDK_SMOKE_BINARY")
	language := sdkSmokeRequiredEnv(t, "MANYFORGE_SDK_SMOKE_LANGUAGE")
	consumerDir := sdkSmokeRequiredEnv(t, "MANYFORGE_SDK_SMOKE_CONSUMER_DIR")
	if !filepath.IsAbs(binary) || !filepath.IsAbs(consumerDir) {
		t.Fatal("smoke binary and installed consumer paths must be absolute")
	}
	switch language {
	case "python", "typescript", "go", "java":
	default:
		t.Fatalf("unsupported smoke language %q", language)
	}
	var command []string
	if err := json.Unmarshal([]byte(sdkSmokeRequiredEnv(t, "MANYFORGE_SDK_SMOKE_CONSUMER_COMMAND")), &command); err != nil || len(command) == 0 {
		t.Fatal("installed consumer command must be a nonempty JSON argv array")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start isolated database: %v", err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 45*time.Second)
		defer stop()
		tdb.Close(cleanup)
	})

	password := "Disposable-sdk-" + uuid.NewString()
	email, otherEmail := "sdk-owner@example.test", "sdk-other@example.test"
	principal, other := sdkSmokeAccounts(t, ctx, tdb, email, otherEmail, password)
	business, err := (&tenancy.Service{DB: tdb.App}).CreateMasterBusiness(ctx, principal, "SDK assigned ticket fixture")
	if err != nil {
		t.Fatalf("create fixture business through tenancy service: %v", err)
	}
	ticket, requester := uuid.New(), uuid.New()
	if _, err := tdb.Super.Exec(ctx, `INSERT INTO requester (id,business_id,tenant_root_id,email,display_name,first_seen_at,last_seen_at,created_at,updated_at) VALUES ($1,$2,$2,'sdk-requester@example.test','SDK Requester',now(),now(),now(),now())`, requester, business.ID); err != nil {
		t.Fatalf("seed requester: %v", err)
	}
	if _, err := tdb.Super.Exec(ctx, `INSERT INTO ticket (id,business_id,tenant_root_id,requester_id,subject,status,priority,assignee_principal_id,reply_token,last_message_at,created_at,updated_at) VALUES ($1,$2,$2,$3,'SDK nullable assignment','open','normal',$4,$5,now(),now(),now())`, ticket, business.ID, requester, principal, "sdk-"+ticket.String()); err != nil {
		t.Fatalf("seed assigned ticket: %v", err)
	}

	browserDir := filepath.Join(consumerDir, "browser")
	if err := os.MkdirAll(browserDir, 0700); err != nil {
		t.Fatal(err)
	}
	browserFiles := http.NewServeMux()
	browserFiles.Handle("/browser/", http.StripPrefix("/browser", http.FileServer(http.Dir(browserDir))))
	allowed := httptest.NewServer(browserFiles)
	denied := httptest.NewServer(browserFiles)
	t.Cleanup(allowed.Close)
	t.Cleanup(denied.Close)
	base := sdkSmokeStartProcess(t, ctx, binary, tdb.AppDSN, allowed.URL, password)
	disabledBase := sdkSmokeStartProcess(t, ctx, binary, tdb.AppDSN, "", password)
	var refreshes, lostRefreshes, lostIngests, destinations atomic.Int64
	session := sdkSmokeProxy(t, base, "/api/v1/auth/refresh", &refreshes, false)
	lossy := sdkSmokeProxy(t, base, "/api/v1/auth/refresh", &lostRefreshes, true)
	lossyTelemetry := sdkSmokeProxy(t, base, "/api/v1/telemetry/ingest/", &lostIngests, true)
	peer := sdkSmokePeerProxy(t, base)
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinations.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(destination.Close)
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", allowed.URL)
		w.Header().Set("Vary", "Origin")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Redirect(w, r, destination.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirect.Close)

	resultPath := filepath.Join(consumerDir, "result.json")
	fixture := map[string]any{
		"base_url": base, "email": email, "password": password,
		"other_email": otherEmail, "other_password": password,
		"principal_id": principal, "other_principal_id": other,
		"ticket_business_id": business.ID, "ticket_id": ticket,
		"allowed_origin": allowed.URL, "denied_origin": denied.URL,
		"lossy_base_url": lossy.URL, "session_base_url": session.URL,
		"lossy_telemetry_base_url": lossyTelemetry.URL,
		"redirect_base_url":        redirect.URL, "disabled_cors_base_url": disabledBase,
		"consumer_dir": consumerDir, "result_path": resultPath,
		"access_token_ttl_seconds": 3, "ingest_rate_burst": 40, "ingest_rate_rps": 20,
		"telemetry_peer_base_url": peer.URL,
	}
	fixturePath := filepath.Join(consumerDir, "fixture.json")
	encoded, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixturePath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	// WriteFile closes the file before the consumer can read it.
	consumer := exec.CommandContext(ctx, command[0], append(command[1:], fixturePath)...)
	consumer.Dir = consumerDir
	consumer.Env = sdkSmokeConsumerEnv()
	var output sdkSmokeLog
	consumer.Stdout, consumer.Stderr = &output, &output
	if err := consumer.Run(); err != nil {
		t.Fatalf("installed %s consumer failed: %v\n%s", language, err, sdkSmokeSanitize(output.String(), password, tdb.AppDSN))
	}
	resultBytes, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("consumer did not write its asserted result: %v", err)
	}
	var result sdkSmokeResult
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatalf("consumer result JSON: %v", err)
	}
	if len(result.Assertions) == 0 {
		t.Fatal("consumer returned no exercised assertions")
	}
	if refreshes.Load() != 1 || lostRefreshes.Load() != 1 || lostIngests.Load() != 1 {
		t.Fatalf("expected one real concurrent refresh, one lossy refresh, and one lossy ingest; got %d/%d/%d", refreshes.Load(), lostRefreshes.Load(), lostIngests.Load())
	}
	if !lossy.Dropped.Load() || !lossyTelemetry.Dropped.Load() || !lossyTelemetry.Accepted.Load() {
		t.Fatal("loss fixtures must drop a real successful refresh and an accepted telemetry event")
	}
	if destinations.Load() != 0 {
		t.Fatalf("SDK followed redirect to destination %d times", destinations.Load())
	}
	sdkSmokeCheckDatabase(t, ctx, tdb, result, language)
	sdkSmokeOrigins(t, ctx, base, disabledBase, allowed.URL, denied.URL, result.FeedbackKey)
	t.Logf("Installed %s consumer passed %d assertions; database, real refresh/no-replay, redirect, and raw Origin checks passed", language, len(result.Assertions))
	if text := output.String(); text != "" {
		t.Log(sdkSmokeSanitize(text, password, tdb.AppDSN, result.FeedbackKey))
	}
}

func sdkSmokeRequiredEnv(t *testing.T, key string) string {
	t.Helper()
	value := os.Getenv(key)
	if value == "" {
		t.Fatalf("%s is required; run tools/sdk/smoke.py --language <language>", key)
	}
	return value
}

func sdkSmokeAccounts(t *testing.T, ctx context.Context, tdb *testdb.TestDB, email, otherEmail, password string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ring, err := auth.NewKeyRing("manyforge", "manyforge-api", "sdk-fixture", priv, map[string]ed25519.PublicKey{"sdk-fixture": pub})
	if err != nil {
		t.Fatal(err)
	}
	svc := &account.Service{DB: tdb.App, Ring: ring, AccessTTL: time.Minute, RefreshTTL: time.Hour, TokenTTL: time.Hour}
	create := func(email string) uuid.UUID {
		_, token, err := svc.Signup(ctx, email, "SDK Fixture", password)
		if err != nil {
			t.Fatalf("fixture signup: %v", err)
		}
		if err := svc.VerifyEmail(ctx, token); err != nil {
			t.Fatalf("fixture verification: %v", err)
		}
		pair, err := svc.Login(ctx, email, password)
		if err != nil {
			t.Fatalf("fixture principal login: %v", err)
		}
		principal, err := ring.Parse(pair.Access)
		if err != nil {
			t.Fatal(err)
		}
		return principal
	}
	return create(email), create(otherEmail)
}

// Only toolchain/local runtime settings are inherited by consumers; application
// credentials, proxy settings, and import overrides cannot leak into their calls.
func sdkSmokeConsumerEnv() []string {
	var env []string
	for _, key := range []string{"PATH", "HOME", "TMPDIR", "TMP", "TEMP", "SYSTEMROOT", "JAVA_HOME", "GOCACHE", "GOMODCACHE", "GOPATH", "PLAYWRIGHT_BROWSERS_PATH", "LANG", "LC_ALL"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return append(env, "GOWORK=off", "PYTHONNOUSERSITE=1")
}

func sdkSmokeStartProcess(t *testing.T, ctx context.Context, binary, dsn, allowedOrigin, password string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	base := "http://" + addr
	blobDir := t.TempDir()
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "TMPDIR=" + os.TempDir(),
		"MANYFORGE_ADDR=" + addr, "MANYFORGE_DATABASE_URL=" + dsn,
		"MANYFORGE_ENVIRONMENT=development", "MANYFORGE_OUTBOUND_MAIL_DISABLED=true",
		"MANYFORGE_SMTP_ADDR=", "MANYFORGE_SMTP_HOST=", "MANYFORGE_SANDBOX_MODE=off",
		"MANYFORGE_BLOB_URL=file://" + filepath.ToSlash(blobDir),
		"MANYFORGE_PUBLIC_BASE_URL=" + base, "MANYFORGE_PUBLIC_INGEST_ALLOWED_ORIGINS=" + allowedOrigin,
		"MANYFORGE_ACCESS_TOKEN_TTL=3s", "MANYFORGE_RATELIMIT_RPS=100", "MANYFORGE_RATELIMIT_BURST=200",
		"MANYFORGE_INGEST_RATE_RPS=20", "MANYFORGE_INGEST_RATE_BURST=40",
		// Trust fixture loopback peers for controlled client-IP attribution.
		// Origin allowlisting remains independent of all forwarding headers.
		"MANYFORGE_TRUSTED_PROXY_CIDR=127.0.0.1/32",
	}
	var secrets []string
	for _, key := range []string{"DKIM", "MCP", "AI", "CONNECTOR", "GITHUB_APP", "FEEDBACK", "MAILING"} {
		material := make([]byte, 32)
		if _, err := rand.Read(material); err != nil {
			_ = listener.Close()
			t.Fatal(err)
		}
		secret := base64.StdEncoding.EncodeToString(material)
		secrets = append(secrets, secret)
		env = append(env, "MANYFORGE_"+key+"_MASTER_KEY="+secret)
	}
	cmd := exec.Command(binary)
	cmd.Env = env
	cmd.Dir = t.TempDir()
	var logs sdkSmokeLog
	cmd.Stdout, cmd.Stderr = &logs, &logs
	// The production binary does not accept socket activation. Reserve the port
	// until immediately before Start; readiness also checks process liveness.
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start actual manyforge process: %v", err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		select {
		case <-done:
		default:
			_ = cmd.Process.Signal(os.Interrupt)
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
		}
		if t.Failed() {
			t.Logf("sanitized actual-server logs:\n%s", sdkSmokeSanitize(logs.String(), append(secrets, dsn, password)...))
		}
	})
	client := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("context expired waiting for actual server readiness")
		case <-done:
			t.Fatalf("actual manyforge exited before ready: %v", waitErr)
		case <-deadline.C:
			t.Fatal("actual manyforge did not reach /readyz")
		case <-tick.C:
			response, err := client.Get(base + "/readyz")
			if err == nil {
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if response.StatusCode == http.StatusOK {
					return base
				}
			}
		}
	}
}

type sdkSmokeFaultProxy struct {
	*httptest.Server
	Dropped  atomic.Bool
	Accepted atomic.Bool
}

func sdkSmokeProxy(t *testing.T, base, countedPath string, count *atomic.Int64, lose bool) *sdkSmokeFaultProxy {
	t.Helper()
	target, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, ResponseHeaderTimeout: 30 * time.Second}
	proxy.Transport = transport
	t.Cleanup(transport.CloseIdleConnections)
	state := &sdkSmokeFaultProxy{}
	proxy.ModifyResponse = func(response *http.Response) error {
		if strings.HasPrefix(response.Request.URL.Path, countedPath) && lose && response.StatusCode >= 200 && response.StatusCode < 300 && state.Dropped.CompareAndSwap(false, true) {
			// Drain the committed response before dropping the downstream connection:
			// the token/event was really consumed by the application, not a mock.
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 65537))
			if strings.Contains(countedPath, "/telemetry/ingest/") && readErr == nil {
				var result struct {
					Accepted int `json:"accepted"`
				}
				if json.Unmarshal(body, &result) == nil && result.Accepted > 0 {
					state.Accepted.Store(true)
				}
			}
			_ = response.Body.Close()
			return errors.New("fixture drops committed response")
		}
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		connection, _, hijackErr := w.(http.Hijacker).Hijack()
		if hijackErr == nil {
			_ = connection.Close()
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, countedPath) {
			count.Add(1)
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	state.Server = server
	return state
}

// A controlled local proxy exercises a distinct client-IP budget through the
// application's existing trusted-proxy policy, without requiring host aliases.
func sdkSmokePeerProxy(t *testing.T, base string) *httptest.Server {
	t.Helper()
	target, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{Proxy: nil}
	t.Cleanup(transport.CloseIdleConnections)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = transport
	director := proxy.Director
	proxy.Director = func(request *http.Request) {
		director(request)
		request.Header.Del("Forwarded")
		request.Header.Set("X-Forwarded-For", "192.0.2.2")
	}
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	return server
}

type sdkSmokeResult struct {
	BusinessID           string   `json:"business_id"`
	ContactID            string   `json:"contact_id"`
	AnalyticsClientID    string   `json:"analytics_client_id"`
	AnalyticsEvent       string   `json:"analytics_event_name"`
	AnalyticsDeniedEvent string   `json:"analytics_denied_event_name"`
	BoardID              string   `json:"board_id"`
	FeedbackKey          string   `json:"feedback_publishable_key"`
	DeniedFeedbackTitles []string `json:"denied_feedback_titles"`
	Assertions           []string `json:"assertions"`
}

func sdkSmokeCheckDatabase(t *testing.T, ctx context.Context, tdb *testdb.TestDB, result sdkSmokeResult, language string) {
	t.Helper()
	for name, value := range map[string]string{"business_id": result.BusinessID, "contact_id": result.ContactID, "analytics_client_id": result.AnalyticsClientID, "board_id": result.BoardID} {
		if _, err := uuid.Parse(value); err != nil {
			t.Fatalf("consumer result requires valid %s", name)
		}
	}
	if result.AnalyticsEvent == "" || result.AnalyticsDeniedEvent == "" || result.AnalyticsEvent == result.AnalyticsDeniedEvent || result.FeedbackKey == "" {
		t.Fatal("consumer result requires distinct analytics event names and feedback publishable key")
	}
	var exists bool
	if err := tdb.Super.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM contact WHERE id=$1 AND tenant_root_id=$2)`, result.ContactID, result.BusinessID).Scan(&exists); err != nil || !exists {
		t.Fatalf("SDK contact missing from its tenant: exists=%v err=%v", exists, err)
	}
	for _, event := range []struct {
		name     string
		expected int
	}{{result.AnalyticsEvent, 1}, {result.AnalyticsDeniedEvent, 0}} {
		var count int
		if err := tdb.Super.QueryRow(ctx, `SELECT count(*) FROM analytics_event WHERE client_id=$1 AND name=$2`, result.AnalyticsClientID, event.name).Scan(&count); err != nil {
			t.Fatalf("analytics database assertion: %v", err)
		}
		if count != event.expected {
			t.Fatalf("analytics origin storage count=%d want=%d", count, event.expected)
		}
	}
	if language == "typescript" && len(result.DeniedFeedbackTitles) < 2 {
		t.Fatal("browser consumer must identify denied JSON and simple POST attempts")
	}
	for _, title := range result.DeniedFeedbackTitles {
		if title == "" {
			t.Fatal("empty denied feedback marker")
		}
		var count int
		if err := tdb.Super.QueryRow(ctx, `SELECT count(*) FROM feedback_post WHERE board_id=$1 AND title=$2`, result.BoardID, title).Scan(&count); err != nil || count != 0 {
			t.Fatalf("denied-origin feedback mutated database: count=%d err=%v", count, err)
		}
	}
}

func sdkSmokeOrigins(t *testing.T, ctx context.Context, base, disabledBase, allowed, denied, key string) {
	t.Helper()
	// Browser scenarios deliberately exhaust the shared bucket. Let its documented
	// burst replenish before the unrelated raw protocol cases.
	select {
	case <-ctx.Done():
		t.Fatal("context expired before raw Origin checks")
	case <-time.After(3 * time.Second):
	}
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request := func(base, method, path string, headers http.Header, status int, cors bool, code string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, method, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header = headers
		response, err := client.Do(req)
		if err != nil {
			t.Fatalf("raw Origin request: %v", err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, 65537))
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != status {
			t.Fatalf("raw Origin %s expected %d got %d", method, status, response.StatusCode)
		}
		expectedOrigin := ""
		if cors {
			expectedOrigin = allowed
		}
		// Analytics intentionally retains its pre-existing wildcard policy.
		if path == "/a/e" {
			expectedOrigin = "*"
		}
		if response.Header.Get("Access-Control-Allow-Origin") != expectedOrigin {
			t.Fatal("raw Origin response changed its endpoint-specific ACAO policy")
		}
		if response.Header.Get("Access-Control-Allow-Credentials") != "" {
			t.Fatal("public CORS enabled credentials")
		}
		if code != "" {
			var payload struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(body, &payload); err != nil || payload.Code != code {
				t.Fatalf("raw Origin wrong error code, expected %s", code)
			}
		}
		if cors && method == "OPTIONS" && status == http.StatusNoContent {
			if len(body) != 0 || response.Header.Get("Access-Control-Max-Age") != "300" || response.Header.Get("Access-Control-Allow-Headers") != "Content-Type" {
				t.Fatal("preflight body/cache/header contract")
			}
			expectedMethods := "GET, POST"
			if strings.HasSuffix(path, "/votes") || strings.Contains(path, "/telemetry/ingest/") {
				expectedMethods = "POST"
			}
			if response.Header.Get("Access-Control-Allow-Methods") != expectedMethods {
				t.Fatal("preflight advertised wrong route methods")
			}
			vary := strings.ToLower(strings.Join(response.Header.Values("Vary"), ","))
			for _, name := range []string{"origin", "access-control-request-method", "access-control-request-headers"} {
				if !strings.Contains(vary, name) {
					t.Fatalf("preflight Vary missing %s", name)
				}
			}
		}
	}
	path := "/api/v1/feedback/public/" + url.PathEscape(key) + "/posts"
	preflight := func(origin string) http.Header {
		return http.Header{"Origin": {origin}, "Access-Control-Request-Method": {"POST"}, "Access-Control-Request-Headers": {"content-type"}}
	}
	for _, route := range []string{path, "/api/v1/feedback/public/invalid-key/posts", path + "/" + uuid.NewString() + "/votes", "/api/v1/telemetry/ingest/invalid-key"} {
		request(base, "OPTIONS", route, preflight(allowed), 204, true, "")
	}
	for _, origins := range [][]string{{denied}, {"null"}, {"*"}, {allowed, allowed}, {allowed + ", " + denied}, {allowed + "/path"}, {"https://user:password@example.test"}, {"not an origin"}} {
		headers := preflight(allowed)
		headers["Origin"] = origins
		request(base, "OPTIONS", path, headers, 403, false, "ORIGIN_NOT_ALLOWED")
		request(base, "POST", path, http.Header{"Origin": origins, "Content-Type": {"text/plain"}}, 403, false, "ORIGIN_NOT_ALLOWED")
	}
	for _, change := range []http.Header{
		{"Access-Control-Request-Method": {"POST", "POST"}},
		{"Access-Control-Request-Method": {"POST, GET"}},
		{"Access-Control-Request-Method": {"DELETE"}},
		{"Access-Control-Request-Method": {"post"}},
		{"Access-Control-Request-Method": {}},
		{"Access-Control-Request-Headers": {"Authorization"}},
		{"Access-Control-Request-Headers": {"X-Feedback-Signature"}},
		{"Access-Control-Request-Headers": {"Content-Type,"}},
	} {
		headers := preflight(allowed)
		for name, values := range change {
			headers[name] = values
		}
		request(base, "OPTIONS", path, headers, 403, true, "CORS_NOT_ALLOWED")
	}
	request(base, "OPTIONS", path, http.Header{"Access-Control-Request-Method": {"POST"}}, 403, false, "ORIGIN_NOT_ALLOWED")
	request(disabledBase, "OPTIONS", path, preflight(allowed), 403, false, "CORS_NOT_ALLOWED")
	request(disabledBase, "GET", "/api/v1/feedback/public/invalid-key/posts", nil, 401, false, "")
	request(base, "GET", "/api/v1/feedback/public/invalid-key/posts", http.Header{"Origin": {allowed}}, 401, true, "")
	request(base, "POST", "/a/e", http.Header{"Origin": {denied}, "Content-Type": {"text/plain"}}, 204, false, "")
}

// Bound captured subprocess output; never print raw payloads or path credentials.
type sdkSmokeLog struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (log *sdkSmokeLog) Write(data []byte) (int, error) {
	log.mu.Lock()
	defer log.mu.Unlock()
	length := len(data)
	remaining := (1 << 20) - log.data.Len()
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		_, _ = log.data.Write(data)
	}
	return length, nil
}
func (log *sdkSmokeLog) String() string {
	log.mu.Lock()
	defer log.mu.Unlock()
	return log.data.String()
}
func sdkSmokeSanitize(text string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	for _, pattern := range []string{
		`(?i)(?:bearer\s+)[^\s"<]+`,
		`eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`,
		`(?i)(?:access_token|refresh_token|signing_secret|publishable_key|password|token)["\s:=]+[^\s,}"<]+`,
		`/(?:feedback/public|telemetry/ingest)/[^/\s"?]+`,
		`postgres(?:ql)?://[^\s"<]+`,
	} {
		text = regexp.MustCompile(pattern).ReplaceAllString(text, "[redacted]")
	}
	return text
}
