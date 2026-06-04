// Package agents — MCP server registry service (Spec 003 US6). This file
// implements CRUD over mcp_server with bearer auth sealed at rest, plus
// ListEnabledForAgent (unseals auth for run-start discovery) and
// ValidateServerIDs (used by the agent service to enforce allowed_mcp_servers).
package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/crypto"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

// mcpServerDB is the minimal DB surface MCPServerService needs — satisfied by
// the real *db.DB. Declared as an interface so unit tests can omit it.
type mcpServerDB interface {
	WithPrincipal(ctx context.Context, principalID uuid.UUID, fn func(pgx.Tx) error) error
}

// MCPServerService manages per-business MCP server connection records. DB is
// the RLS-scoped handle (nil in pure unit tests that only exercise seal/resolve).
type MCPServerService struct {
	DB     mcpServerDB
	Sealer *crypto.Sealer
}

// CreateMCPServerInput is the caller-supplied data for a new MCP server.
type CreateMCPServerInput struct {
	Name      string
	URL       string
	AuthToken string // plaintext bearer token; sealed before persistence, never stored/logged raw
	Enabled   bool
}

// UpdateMCPServerInput is the caller-supplied PATCH data. Nil pointer fields
// mean "leave unchanged" (COALESCE in SQL).
type UpdateMCPServerInput struct {
	Name      *string
	URL       *string
	AuthToken *string // nil = leave sealed_auth_ref unchanged; "" = clear auth
	Enabled   *bool
}

// ResolvedMCPServer is what the agent runtime needs to connect to an MCP server.
type ResolvedMCPServer struct {
	ID         uuid.UUID
	Name       string
	URL        string
	AuthHeader string // "Bearer <token>", or "" if no auth
}

// authBlob is the JSON structure sealed into sealed_auth_ref.
type authBlob struct {
	Scheme string `json:"scheme"`
	Token  string `json:"token"`
}

// validate checks a CreateMCPServerInput at the service boundary.
func (s *MCPServerService) validate(in CreateMCPServerInput) error {
	if in.Name == "" {
		return fmt.Errorf("agents: mcp_server name required: %w", errs.ErrValidation)
	}
	// Colon guard: the tool namespace "mcp:<server>:<tool>" uses SplitN(...,3) to
	// parse the server name. A colon in the server name would create an ambiguous
	// split — e.g. "mcp:srv:x:tool" cannot be reliably split into (srv:x, tool).
	// Rejecting colons in names at creation time keeps the namespace unambiguous.
	if strings.Contains(in.Name, ":") {
		return fmt.Errorf("agents: mcp_server name must not contain ':'  (reserved namespace separator): %w", errs.ErrValidation)
	}
	if in.URL == "" {
		return fmt.Errorf("agents: mcp_server url required: %w", errs.ErrValidation)
	}
	u, err := url.Parse(in.URL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("agents: mcp_server url must be a valid absolute URL: %w", errs.ErrValidation)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("agents: mcp_server url scheme must be http or https, got %q: %w", u.Scheme, errs.ErrValidation)
	}
	return nil
}

// sealAuth seals a plaintext bearer token into an opaque ref.
// Empty token → empty ref (no auth). Non-empty token with nil Sealer → error.
func (s *MCPServerService) sealAuth(token string) (string, error) {
	if token == "" {
		return "", nil
	}
	if s.Sealer == nil {
		return "", fmt.Errorf("agents: sealer not configured (cannot seal bearer token): %w", errs.ErrValidation)
	}
	blob, err := json.Marshal(authBlob{Scheme: "bearer", Token: token})
	if err != nil {
		return "", fmt.Errorf("agents: marshal auth blob: %w", err)
	}
	ref, err := s.Sealer.Seal(blob)
	if err != nil {
		return "", fmt.Errorf("agents: seal auth token: %w", err)
	}
	return ref, nil
}

// resolveAuthHeader opens a sealed ref and returns the Authorization header
// value ("Bearer <token>"). A nil or empty ref means no auth → "".
func (s *MCPServerService) resolveAuthHeader(ref *string) (string, error) {
	if ref == nil || *ref == "" {
		return "", nil
	}
	plain, err := s.Sealer.Open(*ref)
	if err != nil {
		return "", fmt.Errorf("agents: open auth ref: %w", err)
	}
	var blob authBlob
	if err := json.Unmarshal(plain, &blob); err != nil {
		return "", fmt.Errorf("agents: unmarshal auth blob: %w", err)
	}
	return "Bearer " + blob.Token, nil
}

// Create seals the auth token and inserts the MCP server, returning its id.
func (s *MCPServerService) Create(ctx context.Context, principalID, businessID uuid.UUID, in CreateMCPServerInput) (uuid.UUID, error) {
	if err := s.validate(in); err != nil {
		return uuid.Nil, err
	}
	ref, err := s.sealAuth(in.AuthToken)
	if err != nil {
		return uuid.Nil, err
	}
	id := uuid.New()
	var refArg *string
	if ref != "" {
		refArg = &ref
	}
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		_, qerr := dbgen.New(tx).InsertMCPServer(ctx, dbgen.InsertMCPServerParams{
			ID:            id,
			BusinessID:    businessID,
			Name:          in.Name,
			Url:           in.URL,
			SealedAuthRef: refArg,
			Enabled:       in.Enabled,
		})
		return qerr
	})
	if err != nil {
		return uuid.Nil, mapMCPErr(err)
	}
	return id, nil
}

// Get fetches an MCP server by (id, business_id). Unknown or foreign id →
// ErrNotFound (no existence oracle).
func (s *MCPServerService) Get(ctx context.Context, principalID, businessID, id uuid.UUID) (dbgen.McpServer, error) {
	var row dbgen.McpServer
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, qerr := dbgen.New(tx).GetMCPServerByID(ctx, dbgen.GetMCPServerByIDParams{
			ID:         id,
			BusinessID: businessID,
		})
		row = r
		return qerr
	})
	if err != nil {
		return dbgen.McpServer{}, mapMCPErr(err)
	}
	return row, nil
}

// List returns all MCP servers for a business ordered by name.
func (s *MCPServerService) List(ctx context.Context, principalID, businessID uuid.UUID) ([]dbgen.McpServer, error) {
	var rows []dbgen.McpServer
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, qerr := dbgen.New(tx).ListMCPServers(ctx, businessID)
		rows = r
		return qerr
	})
	if err != nil {
		return nil, mapMCPErr(err)
	}
	return rows, nil
}

// Update applies a partial update. Fields whose pointer is nil are unchanged.
func (s *MCPServerService) Update(ctx context.Context, principalID, businessID, id uuid.UUID, in UpdateMCPServerInput) (dbgen.McpServer, error) {
	// Seal updated auth token if provided.
	var sealedAuthArg *string
	if in.AuthToken != nil {
		ref, err := s.sealAuth(*in.AuthToken)
		if err != nil {
			return dbgen.McpServer{}, err
		}
		sealedAuthArg = &ref
	}

	var row dbgen.McpServer
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, qerr := dbgen.New(tx).UpdateMCPServer(ctx, dbgen.UpdateMCPServerParams{
			ID:            id,
			BusinessID:    businessID,
			Name:          in.Name,
			Url:           in.URL,
			SealedAuthRef: sealedAuthArg,
			Enabled:       in.Enabled,
		})
		row = r
		return qerr
	})
	if err != nil {
		return dbgen.McpServer{}, mapMCPErr(err)
	}
	return row, nil
}

// Delete removes an MCP server. Returns ErrNotFound if the row doesn't exist
// or isn't visible to the caller.
func (s *MCPServerService) Delete(ctx context.Context, principalID, businessID, id uuid.UUID) error {
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		n, qerr := dbgen.New(tx).DeleteMCPServer(ctx, dbgen.DeleteMCPServerParams{
			ID:         id,
			BusinessID: businessID,
		})
		if qerr != nil {
			return qerr
		}
		if n == 0 {
			return pgx.ErrNoRows
		}
		return nil
	})
	if err != nil {
		return mapMCPErr(err)
	}
	return nil
}

// ListEnabledForAgent fetches the enabled MCP servers for the given IDs and
// unseals their auth into ready-to-use Authorization header values.
func (s *MCPServerService) ListEnabledForAgent(ctx context.Context, principalID, businessID uuid.UUID, ids []uuid.UUID) ([]ResolvedMCPServer, error) {
	var rows []dbgen.McpServer
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, qerr := dbgen.New(tx).ListEnabledMCPServersByIDs(ctx, dbgen.ListEnabledMCPServersByIDsParams{
			BusinessID: businessID,
			Column2:    ids,
		})
		rows = r
		return qerr
	})
	if err != nil {
		return nil, mapMCPErr(err)
	}

	out := make([]ResolvedMCPServer, 0, len(rows))
	for _, row := range rows {
		header, err := s.resolveAuthHeader(row.SealedAuthRef)
		if err != nil {
			return nil, fmt.Errorf("agents: resolve auth for %s: %w", row.ID, err)
		}
		out = append(out, ResolvedMCPServer{
			ID:         row.ID,
			Name:       row.Name,
			URL:        row.Url,
			AuthHeader: header,
		})
	}
	return out, nil
}

// ResolveEnabledByName fetches a single enabled MCP server by (businessID, name)
// under RLS scoped to principalID, then unseals its auth header. An unknown or
// foreign server name (invisible under RLS) → ErrNotFound. Used by ApprovalExecutor
// to resolve the server for an approved mcp: tool call.
func (s *MCPServerService) ResolveEnabledByName(ctx context.Context, principalID, businessID uuid.UUID, name string) (ResolvedMCPServer, error) {
	var row dbgen.McpServer
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, qerr := dbgen.New(tx).GetEnabledMCPServerByName(ctx, dbgen.GetEnabledMCPServerByNameParams{
			BusinessID: businessID,
			Name:       name,
		})
		row = r
		return qerr
	})
	if err != nil {
		return ResolvedMCPServer{}, mapMCPErr(err)
	}
	header, err := s.resolveAuthHeader(row.SealedAuthRef)
	if err != nil {
		return ResolvedMCPServer{}, fmt.Errorf("agents: resolve auth for %s: %w", row.ID, err)
	}
	return ResolvedMCPServer{
		ID:         row.ID,
		Name:       row.Name,
		URL:        row.Url,
		AuthHeader: header,
	}, nil
}

// ValidateServerIDs checks that every id in ids belongs to businessID.
// Returns ErrValidation if any id is missing/foreign/cross-tenant.
func (s *MCPServerService) ValidateServerIDs(ctx context.Context, principalID, businessID uuid.UUID, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	var found []uuid.UUID
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		r, qerr := dbgen.New(tx).ValidateMCPServerIDs(ctx, dbgen.ValidateMCPServerIDsParams{
			BusinessID: businessID,
			Column2:    ids,
		})
		found = r
		return qerr
	})
	if err != nil {
		return mapMCPErr(err)
	}
	if !allPresent(ids, found) {
		return fmt.Errorf("agents: one or more mcp_server ids are unknown or not owned by this business: %w", errs.ErrValidation)
	}
	return nil
}

// allPresent reports whether every id in requested appears in found. It is
// set-membership (not count-based), so duplicate requested ids are tolerated
// (e.g. [a, a] vs [a] → true) while any foreign/unknown id (present in
// requested but absent from found) yields false.
func allPresent(requested, found []uuid.UUID) bool {
	set := make(map[uuid.UUID]struct{}, len(found))
	for _, id := range found {
		set[id] = struct{}{}
	}
	for _, id := range requested {
		if _, ok := set[id]; !ok {
			return false
		}
	}
	return true
}

// mapMCPErr converts a query/closure error into a stable service-layer sentinel.
// pgx.ErrNoRows → ErrNotFound (no oracle). Unique-constraint violation
// (SQLSTATE 23505 — duplicate (business_id, name)) → ErrConflict.
// Typed sentinels are preserved. Everything else is wrapped for server-side logging.
func mapMCPErr(err error) error {
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("agents: not found: %w", errs.ErrNotFound)
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return fmt.Errorf("agents: duplicate mcp_server: %w", errs.ErrConflict)
	case errors.Is(err, errs.ErrValidation), errors.Is(err, errs.ErrNotFound),
		errors.Is(err, errs.ErrConflict), errors.Is(err, errs.ErrRateLimited):
		return err
	default:
		return fmt.Errorf("agents: query: %w", err)
	}
}
