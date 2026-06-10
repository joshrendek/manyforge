//go:build integration

package connectors

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/events"
	"github.com/manyforge/manyforge/internal/platform/netsafe"
	"github.com/manyforge/manyforge/internal/platform/secrets"
	"github.com/manyforge/manyforge/internal/ticketing"
)

type connSeed struct {
	businessID  uuid.UUID
	principalID uuid.UUID
}

func newTestSealer(t *testing.T) *crypto.Sealer {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("key: %v", err)
	}
	s, err := crypto.NewSealer(key)
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	return s
}

func seedConnectorTenant(ctx context.Context, t *testing.T, tdb *testdb.TestDB) connSeed {
	t.Helper()

	var ownerRole uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		"SELECT id FROM role WHERE tenant_root_id IS NULL AND key='owner'").Scan(&ownerRole); err != nil {
		t.Fatalf("preset owner role: %v", err)
	}

	masterID := uuid.New()
	agentID := uuid.New()
	benignRoleID := uuid.New()
	ownerAcctID := uuid.New()
	ownerHumanID := uuid.New()
	ownerEmail := "conn-owner-" + masterID.String() + "@x.test"

	tx, err := tdb.Super.Begin(ctx)
	if err != nil {
		t.Fatalf("begin seed: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	stmts := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO account (id,email,display_name,status,created_at,updated_at,email_verified_at) VALUES ($1,$2,'Owner','active',now(),now(),now())`,
			[]any{ownerAcctID, ownerEmail}},
		{`INSERT INTO principal (id,kind,account_id,created_at) VALUES ($1,'human',$2,now())`,
			[]any{ownerHumanID, ownerAcctID}},
		{`INSERT INTO business (id,parent_id,tenant_root_id,name,status,created_at,updated_at) VALUES ($1,NULL,$1,'ConnCo','active',now(),now())`,
			[]any{masterID}},
		{`INSERT INTO business_closure (ancestor_id,descendant_id,depth,tenant_root_id) VALUES ($1,$1,0,$1)`,
			[]any{masterID}},
		{`INSERT INTO principal (id,kind,home_business_id,tenant_root_id,created_at) VALUES ($1,'agent',$2,$2,now())`,
			[]any{agentID, masterID}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`,
			[]any{ownerHumanID, masterID, ownerRole}},
		{`INSERT INTO role (id,tenant_root_id,key,name,is_locked,created_at) VALUES ($1,$2,'connector-read','ConnectorRead',false,now())`,
			[]any{benignRoleID, masterID}},
		{`INSERT INTO role_permission (role_id,permission_key) VALUES ($1,'business.read')`,
			[]any{benignRoleID}},
		{`INSERT INTO membership (principal_id,business_id,tenant_root_id,role_id,granted_at) VALUES ($1,$2,$2,$3,now())`,
			[]any{agentID, masterID, benignRoleID}},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed exec: %v\nSQL: %s", err, s.sql)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	return connSeed{businessID: masterID, principalID: agentID}
}

// outboundSeed is what seedOutboundConnector returns: the shared sealer + a Registry
// with the real jira factory registered, plus the native message/op ids the dispatcher
// test asserts the write-back against.
type outboundSeed struct {
	Sealer      *crypto.Sealer
	Registry    *Registry
	ConnectorID uuid.UUID
	TicketID    uuid.UUID
	MessageID   uuid.UUID
	OpID        uuid.UUID
}

// registerStubJira builds a Registry with a stub jira factory that issues REAL
// netsafe (SSRF-safe) HTTP requests honoring rc.AllowPrivateBaseURL — shared by the
// outbound seed helpers so the comment/create paths exercise the same dial gate. The
// connectors/jira package can't be imported here (it imports connectors → import cycle),
// so httpStubConnector reproduces the comment/create POST against the stub.
func registerStubJira(svc *Service) *Registry {
	reg := NewRegistry(svc)
	reg.Register("jira", func(rc ResolvedConnector) (TicketingConnector, error) {
		if rc.BaseURL == "" {
			return nil, fmt.Errorf("stub jira factory: base_url is required")
		}
		if rc.Credential.Email == "" || rc.Credential.APIToken == "" {
			return nil, fmt.Errorf("stub jira factory: email and api_token are required")
		}
		return &httpStubConnector{
			httpClient: netsafe.NewClientWithOptions(30*time.Second, netsafe.Options{
				AllowLoopback: rc.AllowPrivateBaseURL,
				AllowPrivate:  rc.AllowPrivateBaseURL,
			}),
			baseURL:  rc.BaseURL,
			email:    rc.Credential.Email,
			apiToken: rc.Credential.APIToken,
		}, nil
	})
	return reg
}

// seedOutboundConnector builds the full fixture for the OutboundDispatcher comment path:
//   - a jira connector whose base_url is the httptest stub, allow_private_base_url=true
//     (httptest binds to 127.0.0.1, so the netsafe SSRF client must be told to allow it),
//     its credential sealed via a real crypto.Sealer (shared with the dispatcher so unseal
//     succeeds),
//   - a connector-linked native ticket (external_id "JIRA-7") via the 0042 inbound DEFINER,
//   - a pending outbound ticket_message (external_id NULL) awaiting dispatch,
//   - a pending connector_outbound_op (op_type 'comment') for that message.
//
// It reuses US3 building blocks (newTestSealer/secrets.Vault/Service.Create/jiraInput/
// syncIssueSQL) rather than inventing parallel infrastructure.
func seedOutboundConnector(t *testing.T, ctx context.Context, tdb *testdb.TestDB, seed connSeed, baseURL string) outboundSeed {
	t.Helper()

	// Shared sealer: Service.Create seals the credential; the dispatcher opens it.
	sealer := newTestSealer(t)
	svc := &Service{DB: tdb.App, Vault: secrets.NewVault(sealer), Verify: nil}

	in := jiraInput()
	in.BaseURL = baseURL
	in.AllowPrivateBaseURL = true // httptest is 127.0.0.1
	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, in)
	if err != nil {
		t.Fatalf("seedOutboundConnector: create connector: %v", err)
	}

	// Register a factory that builds a REAL netsafe (SSRF-safe) HTTP client honoring
	// rc.AllowPrivateBaseURL — exercising the same dial path the production jira factory uses.
	reg := registerStubJira(svc)

	// Connector-linked native ticket with external_id (principal-less inbound DEFINER).
	var ticketID uuid.UUID
	if err := tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, syncIssueSQL,
			connID, "JIRA-7", baseURL+"/browse/JIRA-7", "Outbound test issue",
			"open", "normal", "reporter@example.com", "Reporter",
			time.Now().UTC().Add(-time.Minute), []byte(`{"key":"JIRA-7"}`),
		).Scan(&ticketID)
	}); err != nil {
		t.Fatalf("seedOutboundConnector: seed ticket: %v", err)
	}

	// Pending outbound message (external_id NULL). direction='outbound' requires a
	// non-NULL author_principal_id (ticket_message CHECK), so attribute it to the agent.
	var msgID uuid.UUID
	if err := tdb.Super.QueryRow(ctx, `
		INSERT INTO ticket_message
			(ticket_id, business_id, tenant_root_id, direction, author_principal_id, message_id, body_text)
		VALUES ($1,$2,$2,'outbound',$3,'m-out-disp','please retry the login')
		RETURNING id`,
		ticketID, seed.businessID, seed.principalID).Scan(&msgID); err != nil {
		t.Fatalf("seedOutboundConnector: seed message: %v", err)
	}

	// Pending comment op for that message.
	var opID uuid.UUID
	if err := tdb.Super.QueryRow(ctx, `
		INSERT INTO connector_outbound_op
			(business_id, tenant_root_id, connector_id, ticket_id, message_id, op_type, body)
		VALUES ($1,$1,$2,$3,$4,'comment','please retry the login') RETURNING id`,
		seed.businessID, connID, ticketID, msgID).Scan(&opID); err != nil {
		t.Fatalf("seedOutboundConnector: enqueue op: %v", err)
	}

	return outboundSeed{
		Sealer:      sealer,
		Registry:    reg,
		ConnectorID: connID,
		TicketID:    ticketID,
		MessageID:   msgID,
		OpID:        opID,
	}
}

// outboundCreateSeed is what seedOutboundCreate returns: everything the create-issue
// dispatcher round-trip test needs. It exposes the *Service (so the test can call the
// escalation method EnqueueOutboundCreateIssue under a real principal), the tenancy ids,
// the UNLINKED native ticket id, the target connector id, plus the shared Sealer + Registry
// used to build the OutboundDispatcher.
type outboundCreateSeed struct {
	Service     *Service
	Sealer      *crypto.Sealer
	Registry    *Registry
	PrincipalID uuid.UUID
	BusinessID  uuid.UUID
	ConnectorID uuid.UUID
	TicketID    uuid.UUID
}

// seedOutboundCreate builds the fixture for the OutboundDispatcher create-issue path. It
// mirrors seedOutboundConnector (same shared sealer / Service.Create / stub jira factory
// registered through the SSRF netsafe client) but differs in two ways the create path needs:
//   - the connector's config is '{"project_key":"SUP","issue_type":"Task"}' so dispatchCreate
//     has the project_key/issue_type it reads off the connector to draft the external issue,
//   - the native ticket is UNLINKED (connector_id NULL, external_id NULL) — the as-yet-
//     unescalated ticket that EnqueueOutboundCreateIssue links by enqueuing a create_issue op.
//
// It seeds its own tenant (it does not take a connSeed) so the test can call it with just
// (t, ctx, tdb, baseURL), matching seedOutboundConnector's reuse of US3 building blocks.
func seedOutboundCreate(t *testing.T, ctx context.Context, tdb *testdb.TestDB, baseURL string) outboundCreateSeed {
	t.Helper()

	tenant := seedConnectorTenant(ctx, t, tdb)

	// Shared sealer: Service.Create seals the credential; the dispatcher opens it.
	sealer := newTestSealer(t)
	svc := &Service{DB: tdb.App, Vault: secrets.NewVault(sealer), Verify: nil}

	in := jiraInput()
	in.BaseURL = baseURL
	in.AllowPrivateBaseURL = true // httptest is 127.0.0.1
	// dispatchCreate reads project_key + issue_type off the connector config to draft the
	// external issue; without these the create op fails "config missing project_key/issue_type".
	in.Config = map[string]any{"project_key": "SUP", "issue_type": "Task"}
	connID, err := svc.Create(ctx, tenant.principalID, tenant.businessID, in)
	if err != nil {
		t.Fatalf("seedOutboundCreate: create connector: %v", err)
	}

	// Same stub jira factory as seedOutboundConnector: a REAL netsafe (SSRF-safe) client
	// honoring rc.AllowPrivateBaseURL, so the create POST genuinely traverses the dial gate
	// against the 127.0.0.1 httptest stub.
	reg := registerStubJira(svc)

	// An UNLINKED native ticket (connector_id NULL, external_id NULL). The ticket FK chain
	// requires a requester row, so seed both via Super (RLS bypassed by the superuser seed
	// role). ticket_connector_external_chk allows connector_id NULL with external_id NULL.
	var requesterID, ticketID uuid.UUID
	if err := tdb.Super.QueryRow(ctx, `
		INSERT INTO requester (id, business_id, tenant_root_id, email, display_name,
		                       first_seen_at, last_seen_at, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, $1, $2, 'Native Reporter', now(), now(), now(), now())
		RETURNING id`,
		tenant.businessID, "native-"+connID.String()+"@x.test").Scan(&requesterID); err != nil {
		t.Fatalf("seedOutboundCreate: seed requester: %v", err)
	}
	if err := tdb.Super.QueryRow(ctx, `
		INSERT INTO ticket (id, business_id, tenant_root_id, requester_id, subject, status, priority,
		                    reply_token, last_message_at, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, $1, $2, 'Please escalate me', 'open', 'normal',
		        $3, now(), now(), now())
		RETURNING id`,
		tenant.businessID, requesterID, "native-reply-"+connID.String()).Scan(&ticketID); err != nil {
		t.Fatalf("seedOutboundCreate: seed ticket: %v", err)
	}

	return outboundCreateSeed{
		Service:     svc,
		Sealer:      sealer,
		Registry:    reg,
		PrincipalID: tenant.principalID,
		BusinessID:  tenant.businessID,
		ConnectorID: connID,
		TicketID:    ticketID,
	}
}

// httpStubConnector is a minimal TicketingConnector that issues REAL HTTP requests through
// a netsafe SSRF client (so the dispatcher comment-path test genuinely exercises the
// SSRF dial gate against the 127.0.0.1 httptest stub). Only PostComment/CreateIssue are
// implemented; the rest satisfy the interface.
type httpStubConnector struct {
	httpClient *http.Client
	baseURL    string
	email      string
	apiToken   string
}

var _ TicketingConnector = (*httpStubConnector)(nil)

func (c *httpStubConnector) post(ctx context.Context, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.email, c.apiToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("stub connector: upstream status %d", resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (c *httpStubConnector) PostComment(ctx context.Context, externalID, body string) (ExternalComment, error) {
	var resp struct {
		ID     string `json:"id"`
		Author struct {
			DisplayName string `json:"displayName"`
		} `json:"author"`
	}
	if err := c.post(ctx, "/rest/api/3/issue/"+externalID+"/comment",
		map[string]any{"body": body}, &resp); err != nil {
		return ExternalComment{}, err
	}
	return ExternalComment{ExternalID: resp.ID, Author: resp.Author.DisplayName, Body: body}, nil
}

func (c *httpStubConnector) CreateIssue(ctx context.Context, draft ExternalIssueDraft) (ExternalIssue, error) {
	var resp struct {
		Key string `json:"key"`
	}
	if err := c.post(ctx, "/rest/api/3/issue",
		map[string]any{"fields": map[string]any{
			"project":   map[string]any{"key": draft.ProjectKey},
			"issuetype": map[string]any{"name": draft.IssueType},
			"summary":   draft.Summary,
		}}, &resp); err != nil {
		return ExternalIssue{}, err
	}
	return ExternalIssue{ExternalID: resp.Key, URL: c.baseURL + "/browse/" + resp.Key, Title: draft.Summary}, nil
}

func (c *httpStubConnector) FetchIssue(_ context.Context, _ string) (ExternalIssue, error) {
	return ExternalIssue{}, nil
}
func (c *httpStubConnector) TransitionStatus(_ context.Context, _, _ string) error { return nil }
func (c *httpStubConnector) ListUpdatedSince(_ context.Context, _ time.Time) ([]string, error) {
	return nil, nil
}
func (c *httpStubConnector) VerifyWebhook(_ http.Header, _ []byte) error { return nil }
func (c *httpStubConnector) DecodeWebhook(_ []byte) (WebhookEvent, error) {
	return WebhookEvent{}, nil
}

// ---------------------------------------------------------------------------
// Bidirectional round-trip scaffolding (US4 T8)
// ---------------------------------------------------------------------------

// roundTripConnector is the single TicketingConnector the round-trip env registers under
// "jira". It bridges BOTH halves of the loop through one factory:
//   - inbound webhook: the embedded hmacFakeConnector supplies the real HMAC VerifyWebhook +
//     DecodeWebhook, so the public webhook handler exercises the genuine cryptographic path;
//   - inbound sync: FetchIssue returns a populated ExternalIssue (with a ReporterEmail so
//     sync_inbound_external_issue creates the requester that Reply later resolves);
//   - outbound: PostComment issues a REAL HTTP POST through the netsafe SSRF-safe client
//     to the httptest stub (like httpStubConnector), so the dispatcher genuinely traverses
//     the dial gate against 127.0.0.1.
//
// It composes the two existing test connectors rather than re-deriving their logic: it
// embeds hmacFakeConnector (inheriting VerifyWebhook/DecodeWebhook/TransitionStatus/
// ListUpdatedSince for free; set its secret via the embedded field) and delegates the
// outbound calls to httpStubConnector, overriding only FetchIssue/PostComment/CreateIssue.
type roundTripConnector struct {
	hmacFakeConnector                    // inbound HMAC verify/decode (set .secret)
	issue             ExternalIssue      // canned issue returned by FetchIssue (inbound sync)
	post              *httpStubConnector // real SSRF POST path (outbound comment/create)
}

var _ TicketingConnector = (*roundTripConnector)(nil)

func (c *roundTripConnector) FetchIssue(_ context.Context, externalID string) (ExternalIssue, error) {
	iss := c.issue
	if iss.ExternalID == "" {
		iss.ExternalID = externalID
	}
	return iss, nil
}

func (c *roundTripConnector) PostComment(ctx context.Context, externalID, body string) (ExternalComment, error) {
	return c.post.PostComment(ctx, externalID, body)
}
func (c *roundTripConnector) CreateIssue(ctx context.Context, draft ExternalIssueDraft) (ExternalIssue, error) {
	return c.post.CreateIssue(ctx, draft)
}

// connectorRoundTripEnv bundles everything the bidirectional loop needs behind one stub:
// the connectors.Service (+ shared sealer) that owns the connector, the Registry whose
// "jira" factory returns the roundTripConnector, the wired inbound subscriber + outbound
// dispatcher, the genuine ticketing.Service producer, the webhook handler, and the tenancy
// ids. seedFullConnectorEnv composes it from the Task 2/4 helpers (shared sealer / stub
// jira factory via the SSRF netsafe client) plus the US3 inbound webhook + subscriber path.
type connectorRoundTripEnv struct {
	tdb           *testdb.TestDB
	WebhookSecret string

	Service    *Service
	Registry   *Registry
	Subscriber *InboundSyncSubscriber
	Dispatcher *OutboundDispatcher
	Ticketing  *ticketing.Service
	Webhook    *WebhookHandler

	ConnectorID uuid.UUID
	BusinessID  uuid.UUID
	PrincipalID uuid.UUID
}

// seedFullConnectorEnv builds the round-trip fixture. baseURL is the httptest Jira stub URL
// (127.0.0.1) so the connector is created with allow_private_base_url=true and the netsafe
// SSRF client is told to allow loopback — the same trust path seedOutboundConnector uses.
func seedFullConnectorEnv(t *testing.T, ctx context.Context, tdb *testdb.TestDB, seed connSeed, baseURL string) *connectorRoundTripEnv {
	t.Helper()

	webhookSecret := "rt-webhook-secret-" + uuid.NewString()

	// Shared sealer across Service / subscriber / dispatcher / webhook handler so the
	// credential sealed by Service.Create can be opened by every consumer.
	sealer := newTestSealer(t)
	svc := &Service{DB: tdb.App, Vault: secrets.NewVault(sealer), Verify: nil}

	in := jiraInput()
	in.BaseURL = baseURL
	in.AllowPrivateBaseURL = true // httptest is 127.0.0.1
	in.WebhookSecret = webhookSecret
	connID, err := svc.Create(ctx, seed.principalID, seed.businessID, in)
	if err != nil {
		t.Fatalf("seedFullConnectorEnv: create connector: %v", err)
	}

	// The canned external issue the inbound sync upserts into a native ticket. A non-empty
	// ReporterEmail is required so sync_inbound_external_issue creates the requester that
	// ticketing.Service.Reply later resolves as the recipient.
	issue := ExternalIssue{
		URL:           baseURL + "/browse/JIRA-RT",
		Title:         "Round-trip issue",
		Status:        "In Progress",
		Priority:      "Normal",
		ReporterEmail: "rt-reporter@acme.test",
		ReporterName:  "RT Reporter",
		UpdatedAt:     time.Now().UTC().Add(-time.Minute),
	}

	// One factory, one connector type — both inbound (webhook handler + subscriber) and
	// outbound (dispatcher) resolve "jira" through it. The outbound POST path is a REAL
	// netsafe SSRF client honoring rc.AllowPrivateBaseURL (mirrors registerStubJira).
	reg := NewRegistry(svc)
	reg.Register("jira", func(rc ResolvedConnector) (TicketingConnector, error) {
		if rc.BaseURL == "" {
			return nil, fmt.Errorf("round-trip jira factory: base_url is required")
		}
		return &roundTripConnector{
			hmacFakeConnector: hmacFakeConnector{secret: rc.Credential.WebhookSecret},
			issue:             issue,
			post: &httpStubConnector{
				httpClient: netsafe.NewClientWithOptions(30*time.Second, netsafe.Options{
					AllowLoopback: rc.AllowPrivateBaseURL,
					AllowPrivate:  rc.AllowPrivateBaseURL,
				}),
				baseURL:  rc.BaseURL,
				email:    rc.Credential.Email,
				apiToken: rc.Credential.APIToken,
			},
		}, nil
	})

	wh := NewWebhookHandler(tdb.App, sealer, reg, slog.Default())

	return &connectorRoundTripEnv{
		tdb:           tdb,
		WebhookSecret: webhookSecret,
		Service:       svc,
		Registry:      reg,
		Subscriber:    &InboundSyncSubscriber{DB: tdb.App, Sealer: sealer, Registry: reg, Logger: slog.Default()},
		Dispatcher:    &OutboundDispatcher{DB: tdb.App, Sealer: sealer, Registry: reg, Logger: slog.Default(), Batch: 10},
		Ticketing:     &ticketing.Service{DB: tdb.App, SystemDomain: "inbound.localhost"},
		Webhook:       wh,
		ConnectorID:   connID,
		BusinessID:    seed.businessID,
		PrincipalID:   seed.principalID,
	}
}

// deliverWebhook POSTs a signed Jira webhook to the public handler (the genuine US3 inbound
// entry point), asserting the handler accepted it (202) and wrote the connector.inbound.sync
// outbox event. ts must be unique per call (it is the delivery-id discriminator).
func (e *connectorRoundTripEnv) deliverWebhook(t *testing.T, ctx context.Context, issueKey string, ts int64) {
	t.Helper()
	body := webhookPayload(issueKey, ts)
	sig := hmacHeader(e.WebhookSecret, body)

	r := chi.NewRouter()
	e.Webhook.PublicRoutes(r)
	path := fmt.Sprintf("/connectors/jira/%s/webhook", e.ConnectorID.String())
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature", sig)
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("deliverWebhook: want 202, got %d: %s", rr.Code, rr.Body.String())
	}

	// Confirm the handler produced the inbound-sync outbox event for this issue (the
	// hand-off the subscriber drains).
	var n int
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM outbox WHERE topic=$1
		   AND payload->>'connector_id'=$2 AND payload->>'external_id'=$3 AND processed_at IS NULL`,
		TopicConnectorInboundSync, e.ConnectorID.String(), issueKey).Scan(&n); err != nil {
		t.Fatalf("deliverWebhook: count outbox: %v", err)
	}
	if n != 1 {
		t.Fatalf("deliverWebhook: want 1 pending inbound-sync outbox event, got %d", n)
	}
}

// runInboundOnce drains the oldest pending connector.inbound.sync outbox event for this
// connector through the real InboundSyncSubscriber (which calls FetchIssue + the
// sync_inbound_* DEFINERs), then marks it processed — exactly as the outbox worker would.
// This is the genuine webhook -> outbox -> subscriber -> native ticket chain.
func (e *connectorRoundTripEnv) runInboundOnce(t *testing.T, ctx context.Context) {
	t.Helper()
	var evID uuid.UUID
	var tenantRoot uuid.UUID
	var payload []byte
	if err := e.tdb.Super.QueryRow(ctx,
		`SELECT id, tenant_root_id, payload FROM outbox
		   WHERE topic=$1 AND payload->>'connector_id'=$2 AND processed_at IS NULL
		   ORDER BY id LIMIT 1`,
		TopicConnectorInboundSync, e.ConnectorID.String()).Scan(&evID, &tenantRoot, &payload); err != nil {
		t.Fatalf("runInboundOnce: load outbox event: %v", err)
	}

	ev := events.Event{ID: evID, TenantRootID: tenantRoot, Topic: TopicConnectorInboundSync, Payload: payload}
	if err := e.tdb.App.WithTx(ctx, func(tx pgx.Tx) error {
		return e.Subscriber.Handle(ctx, tx, ev)
	}); err != nil {
		t.Fatalf("runInboundOnce: subscriber Handle: %v", err)
	}

	if _, err := e.tdb.Super.Exec(ctx, `UPDATE outbox SET processed_at=now() WHERE id=$1`, evID); err != nil {
		t.Fatalf("runInboundOnce: mark processed: %v", err)
	}
}

// replyAsOperator sends an operator reply on the (now connector-linked) ticket through the
// genuine ticketing.Service.Reply producer — the Task 3 hook that enqueues the pending
// 'comment' connector_outbound_op inside the SAME source tx (NOT a raw queue insert).
func (e *connectorRoundTripEnv) replyAsOperator(t *testing.T, ctx context.Context, ticketID uuid.UUID, body string) {
	t.Helper()
	if _, err := e.Ticketing.Reply(ctx, e.PrincipalID, e.BusinessID, ticketID, ticketing.ReplyInput{BodyText: body}); err != nil {
		t.Fatalf("replyAsOperator: Reply: %v", err)
	}
}

// ticketByExternal returns the native ticket id linked to (connector_id, external_id), or
// fatals if it does not exist. Read via the RLS-exempt Super pool.
func ticketByExternal(t *testing.T, ctx context.Context, tdb *testdb.TestDB, connID uuid.UUID, externalID string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := tdb.Super.QueryRow(ctx,
		`SELECT id FROM ticket WHERE connector_id=$1 AND external_id=$2`,
		connID, externalID).Scan(&id); err != nil {
		t.Fatalf("ticketByExternal(%q): %v", externalID, err)
	}
	return id
}

// operatorMessageHasExternalID reports whether the ticket's outbound operator message was
// linked back to a Jira comment id (external_id non-NULL) — the write-back the dispatcher
// performs via complete_outbound_comment. Read via the RLS-exempt Super pool.
func operatorMessageHasExternalID(t *testing.T, ctx context.Context, tdb *testdb.TestDB, ticketID uuid.UUID) bool {
	t.Helper()
	var n int
	if err := tdb.Super.QueryRow(ctx,
		`SELECT count(*) FROM ticket_message
		   WHERE ticket_id=$1 AND direction='outbound' AND external_id IS NOT NULL`,
		ticketID).Scan(&n); err != nil {
		t.Fatalf("operatorMessageHasExternalID: %v", err)
	}
	return n > 0
}
