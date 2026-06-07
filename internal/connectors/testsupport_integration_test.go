//go:build integration

package connectors

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/netsafe"
	"github.com/manyforge/manyforge/internal/platform/secrets"
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
	// rc.AllowPrivateBaseURL — exercising the same dial path the production jira factory
	// uses. The connectors/jira package can't be imported here (it imports connectors →
	// import cycle), so this minimal connector reproduces the comment/create POST against
	// the stub through the same SSRF client.
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
