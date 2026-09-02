package automations

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/platform/audit"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

const (
	defaultPageSize = 50
	maxPageSize     = 100
	cursorKind      = "automation"
)

var emptyGraph = Graph{Nodes: []Node{}, Edges: []Edge{}}

type Service struct{ DB *db.DB }

func (s *Service) Create(ctx context.Context, principalID, businessID uuid.UUID, in CreateInput) (Automation, error) {
	in.Name = strings.TrimSpace(in.Name)
	if err := validateDefinition(in.Name, in.Description); err != nil {
		return Automation{}, err
	}
	var out Automation
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		automationID, versionID := uuid.New(), uuid.New()
		row, err := q.InsertAutomation(ctx, dbgen.InsertAutomationParams{
			ID: automationID, BusinessID: businessID, TenantRootID: root,
			Name: in.Name, Description: cleanOptional(in.Description), AllowReenroll: in.AllowReenroll,
			CreatedByPrincipalID: pgtype.UUID{Bytes: principalID, Valid: true},
		})
		if err != nil {
			return err
		}
		raw, _ := marshalGraph(emptyGraph)
		if _, err = q.InsertAutomationVersion(ctx, dbgen.InsertAutomationVersionParams{
			ID: versionID, BusinessID: businessID, TenantRootID: root,
			AutomationID: automationID, Number: 1, Graph: raw,
		}); err != nil {
			return err
		}
		if tag, err := tx.Exec(ctx, `UPDATE automation SET draft_version_id=$1,updated_at=now()
			WHERE id=$2 AND business_id=$3 AND tenant_root_id=$4`, versionID, automationID, businessID, root); err != nil || tag.RowsAffected() != 1 {
			if err != nil {
				return err
			}
			return pgx.ErrNoRows
		}
		if err = writeAudit(ctx, tx, principalID, businessID, root, "automation.created", automationID, map[string]any{"version_id": versionID}); err != nil {
			return err
		}
		row.DraftVersionID = pgtype.UUID{Bytes: versionID, Valid: true}
		out = toAutomation(row)
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) Get(ctx context.Context, principalID, businessID, automationID uuid.UUID) (Automation, error) {
	var out Automation
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		row, err := q.GetAutomation(ctx, dbgen.GetAutomationParams{ID: automationID, BusinessID: businessID, TenantRootID: root})
		if err != nil {
			return err
		}
		out = toAutomation(row)
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) List(ctx context.Context, principalID, businessID uuid.UUID, cursor string, limit int) (Page[Automation], error) {
	limit = clampLimit(limit)
	var out Page[Automation]
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		var rows []dbgen.Automation
		if cursor == "" {
			rows, err = q.ListAutomations(ctx, dbgen.ListAutomationsParams{BusinessID: businessID, TenantRootID: root, Limit: int32(limit + 1)})
		} else {
			at, id, decodeErr := decodeCursor(cursor)
			if decodeErr != nil {
				return decodeErr
			}
			rows, err = q.ListAutomationsAfter(ctx, dbgen.ListAutomationsAfterParams{BusinessID: businessID, TenantRootID: root, CurCreated: at, CurID: id, Lim: int32(limit + 1)})
		}
		if err != nil {
			return err
		}
		more := len(rows) > limit
		if more {
			rows = rows[:limit]
		}
		out.Items = make([]Automation, 0, len(rows))
		for _, row := range rows {
			out.Items = append(out.Items, toAutomation(row))
		}
		if more {
			cursor := encodeCursor(rows[len(rows)-1].CreatedAt, rows[len(rows)-1].ID)
			out.NextCursor = &cursor
		}
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) Update(ctx context.Context, principalID, businessID, automationID uuid.UUID, in UpdateInput) (Automation, error) {
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		in.Name = &name
	}
	var out Automation
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		current, err := q.GetAutomation(ctx, dbgen.GetAutomationParams{ID: automationID, BusinessID: businessID, TenantRootID: root})
		if err != nil {
			return err
		}
		if current.Status == dbgen.AutomationStatusArchived {
			return errs.ErrConflict
		}
		name, description, reenroll := current.Name, current.Description, current.AllowReenroll
		if in.Name != nil {
			name = *in.Name
		}
		if in.SetDescription {
			description = cleanOptional(in.Description)
		}
		if in.AllowReenroll != nil {
			reenroll = *in.AllowReenroll
		}
		if err = validateDefinition(name, description); err != nil {
			return err
		}
		row, err := q.UpdateAutomationDefinition(ctx, dbgen.UpdateAutomationDefinitionParams{
			Name: name, Description: description, AllowReenroll: reenroll,
			ID: automationID, BusinessID: businessID, TenantRootID: root,
		})
		if err != nil {
			return err
		}
		if err = writeAudit(ctx, tx, principalID, businessID, root, "automation.updated", automationID, map[string]any{"allow_reenroll": row.AllowReenroll}); err != nil {
			return err
		}
		out = toAutomation(row)
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) Versions(ctx context.Context, principalID, businessID, automationID uuid.UUID) ([]Version, error) {
	var out []Version
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		if _, err = q.GetAutomation(ctx, dbgen.GetAutomationParams{ID: automationID, BusinessID: businessID, TenantRootID: root}); err != nil {
			return err
		}
		rows, err := q.ListAutomationVersions(ctx, dbgen.ListAutomationVersionsParams{AutomationID: automationID, BusinessID: businessID, TenantRootID: root})
		if err != nil {
			return err
		}
		out = make([]Version, 0, len(rows))
		for _, row := range rows {
			version, err := toVersion(row)
			if err != nil {
				return err
			}
			out = append(out, version)
		}
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) Version(ctx context.Context, principalID, businessID, automationID, versionID uuid.UUID) (Version, error) {
	var out Version
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		row, err := q.GetAutomationVersion(ctx, dbgen.GetAutomationVersionParams{ID: versionID, AutomationID: automationID, BusinessID: businessID, TenantRootID: root})
		if err != nil {
			return err
		}
		out, err = toVersion(row)
		return err
	})
	return out, mapErr(err)
}

func (s *Service) CloneVersion(ctx context.Context, principalID, businessID, automationID uuid.UUID) (Version, error) {
	var out Version
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		automation, err := lockAutomation(ctx, tx, q, businessID, root, automationID)
		if err != nil {
			return err
		}
		if automation.Status == dbgen.AutomationStatusArchived || automation.DraftVersionID.Valid || !automation.ActiveVersionID.Valid {
			return errs.ErrConflict
		}
		active, err := q.GetAutomationVersion(ctx, dbgen.GetAutomationVersionParams{ID: uuid.UUID(automation.ActiveVersionID.Bytes), AutomationID: automationID, BusinessID: businessID, TenantRootID: root})
		if err != nil {
			return err
		}
		versionID := uuid.New()
		row, err := q.InsertAutomationVersion(ctx, dbgen.InsertAutomationVersionParams{ID: versionID, BusinessID: businessID, TenantRootID: root, AutomationID: automationID, Number: active.Number + 1, Graph: active.Graph})
		if err != nil {
			return err
		}
		if tag, execErr := tx.Exec(ctx, `UPDATE automation SET draft_version_id=$1,updated_at=now()
			WHERE id=$2 AND business_id=$3 AND tenant_root_id=$4 AND draft_version_id IS NULL`, versionID, automationID, businessID, root); execErr != nil || tag.RowsAffected() != 1 {
			if execErr != nil {
				return execErr
			}
			return errs.ErrConflict
		}
		if err = writeAudit(ctx, tx, principalID, businessID, root, "automation.version.created", automationID, map[string]any{"version_id": versionID, "number": row.Number}); err != nil {
			return err
		}
		out, err = toVersion(row)
		return err
	})
	return out, mapErr(err)
}

func (s *Service) PutGraph(ctx context.Context, principalID, businessID, automationID, versionID uuid.UUID, graph Graph) (Version, error) {
	raw, err := marshalGraph(graph)
	if err != nil {
		return Version{}, validation("graph is not valid JSON")
	}
	var out Version
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		automation, err := q.GetAutomation(ctx, dbgen.GetAutomationParams{ID: automationID, BusinessID: businessID, TenantRootID: root})
		if err != nil {
			return err
		}
		version, err := q.GetAutomationVersion(ctx, dbgen.GetAutomationVersionParams{ID: versionID, AutomationID: automationID, BusinessID: businessID, TenantRootID: root})
		if err != nil {
			return err
		}
		if !automation.DraftVersionID.Valid || uuid.UUID(automation.DraftVersionID.Bytes) != versionID {
			return errs.ErrConflict
		}
		if version.Status != dbgen.AutomationVersionStatusDraft {
			return errs.ErrConflict
		}
		row, err := q.UpdateAutomationVersionGraph(ctx, dbgen.UpdateAutomationVersionGraphParams{Graph: raw, ID: versionID, AutomationID: automationID, BusinessID: businessID, TenantRootID: root})
		if err != nil {
			return err
		}
		if err = writeAudit(ctx, tx, principalID, businessID, root, "automation.graph.updated", automationID, map[string]any{"version_id": versionID}); err != nil {
			return err
		}
		out, err = toVersion(row)
		return err
	})
	return out, mapErr(err)
}

func (s *Service) ValidateVersion(ctx context.Context, principalID, businessID, automationID, versionID uuid.UUID) (ValidationResult, error) {
	var result ValidationResult
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		row, err := q.GetAutomationVersion(ctx, dbgen.GetAutomationVersionParams{ID: versionID, AutomationID: automationID, BusinessID: businessID, TenantRootID: root})
		if err != nil {
			return err
		}
		graph, err := decodeGraph(row.Graph)
		if err != nil {
			return err
		}
		result, err = validateInTenant(ctx, tx, businessID, root, graph)
		return err
	})
	return result, mapErr(err)
}

func (s *Service) Activate(ctx context.Context, principalID, businessID, automationID, versionID uuid.UUID) (Automation, error) {
	var out Automation
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		automation, err := lockAutomation(ctx, tx, q, businessID, root, automationID)
		if err != nil {
			return err
		}
		version, err := q.GetAutomationVersion(ctx, dbgen.GetAutomationVersionParams{ID: versionID, AutomationID: automationID, BusinessID: businessID, TenantRootID: root})
		if err != nil {
			return err
		}
		if automation.Status == dbgen.AutomationStatusArchived || !automation.DraftVersionID.Valid || uuid.UUID(automation.DraftVersionID.Bytes) != versionID || version.Status != dbgen.AutomationVersionStatusDraft {
			return errs.ErrConflict
		}
		graph, err := decodeGraph(version.Graph)
		if err != nil {
			return err
		}
		validationResult, err := validateInTenant(ctx, tx, businessID, root, graph)
		if err != nil {
			return err
		}
		if !validationResult.Valid {
			return &InvalidGraphError{Issues: validationResult.Issues}
		}
		triggerKind, triggerRef, err := triggerIdentity(graph)
		if err != nil {
			return err
		}
		if automation.ActiveVersionID.Valid {
			if tag, execErr := tx.Exec(ctx, `UPDATE automation_version SET status='superseded',updated_at=now()
				WHERE id=$1 AND automation_id=$2 AND business_id=$3 AND tenant_root_id=$4 AND status='active'`, uuid.UUID(automation.ActiveVersionID.Bytes), automationID, businessID, root); execErr != nil || tag.RowsAffected() != 1 {
				if execErr != nil {
					return execErr
				}
				return errs.ErrConflict
			}
		}
		if tag, execErr := tx.Exec(ctx, `UPDATE automation_version SET status='active',trigger_kind=$1,trigger_ref=$2,activated_at=now(),updated_at=now()
			WHERE id=$3 AND automation_id=$4 AND business_id=$5 AND tenant_root_id=$6 AND status='draft'`, triggerKind, triggerRef, versionID, automationID, businessID, root); execErr != nil || tag.RowsAffected() != 1 {
			if execErr != nil {
				return execErr
			}
			return errs.ErrConflict
		}
		if tag, execErr := tx.Exec(ctx, `UPDATE automation SET status='active',active_version_id=$1,draft_version_id=NULL,updated_at=now()
			WHERE id=$2 AND business_id=$3 AND tenant_root_id=$4`, versionID, automationID, businessID, root); execErr != nil || tag.RowsAffected() != 1 {
			if execErr != nil {
				return execErr
			}
			return pgx.ErrNoRows
		}
		if err = writeAudit(ctx, tx, principalID, businessID, root, "automation.activated", automationID, map[string]any{"version_id": versionID}); err != nil {
			return err
		}
		row, err := q.GetAutomation(ctx, dbgen.GetAutomationParams{ID: automationID, BusinessID: businessID, TenantRootID: root})
		if err != nil {
			return err
		}
		out = toAutomation(row)
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) Pause(ctx context.Context, principalID, businessID, automationID uuid.UUID) (Automation, error) {
	return s.transition(ctx, principalID, businessID, automationID, "active", "paused", "automation.paused")
}

func (s *Service) Resume(ctx context.Context, principalID, businessID, automationID uuid.UUID) (Automation, error) {
	return s.transition(ctx, principalID, businessID, automationID, "paused", "active", "automation.resumed")
}

func (s *Service) Archive(ctx context.Context, principalID, businessID, automationID uuid.UUID) (Automation, error) {
	var out Automation
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		if _, err = lockAutomation(ctx, tx, q, businessID, root, automationID); err != nil {
			return err
		}
		if tag, execErr := tx.Exec(ctx, `UPDATE automation SET status='archived',draft_version_id=NULL,updated_at=now()
			WHERE id=$1 AND business_id=$2 AND tenant_root_id=$3 AND status<>'archived'`, automationID, businessID, root); execErr != nil || tag.RowsAffected() != 1 {
			if execErr != nil {
				return execErr
			}
			return errs.ErrConflict
		}
		if _, err = tx.Exec(ctx, `UPDATE automation_enrollment SET status='exited',exit_reason='archived',finished_at=now(),lease_expires_at=NULL,updated_at=now()
			WHERE automation_id=$1 AND business_id=$2 AND tenant_root_id=$3 AND status='active'`, automationID, businessID, root); err != nil {
			return err
		}
		if err = writeAudit(ctx, tx, principalID, businessID, root, "automation.archived", automationID, map[string]any{}); err != nil {
			return err
		}
		row, err := q.GetAutomation(ctx, dbgen.GetAutomationParams{ID: automationID, BusinessID: businessID, TenantRootID: root})
		if err != nil {
			return err
		}
		out = toAutomation(row)
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) transition(ctx context.Context, principalID, businessID, automationID uuid.UUID, from, to, action string) (Automation, error) {
	var out Automation
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		if _, err = lockAutomation(ctx, tx, q, businessID, root, automationID); err != nil {
			return err
		}
		if tag, execErr := tx.Exec(ctx, `UPDATE automation SET status=$1,updated_at=now()
			WHERE id=$2 AND business_id=$3 AND tenant_root_id=$4 AND status=$5`, to, automationID, businessID, root, from); execErr != nil || tag.RowsAffected() != 1 {
			if execErr != nil {
				return execErr
			}
			return errs.ErrConflict
		}
		if err = writeAudit(ctx, tx, principalID, businessID, root, action, automationID, map[string]any{}); err != nil {
			return err
		}
		row, err := q.GetAutomation(ctx, dbgen.GetAutomationParams{ID: automationID, BusinessID: businessID, TenantRootID: root})
		if err != nil {
			return err
		}
		out = toAutomation(row)
		return nil
	})
	return out, mapErr(err)
}

func lockAutomation(ctx context.Context, tx pgx.Tx, q *dbgen.Queries, businessID, root, automationID uuid.UUID) (dbgen.Automation, error) {
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM automation WHERE id=$1 AND business_id=$2 AND tenant_root_id=$3 FOR UPDATE`, automationID, businessID, root).Scan(&id); err != nil {
		return dbgen.Automation{}, err
	}
	return q.GetAutomation(ctx, dbgen.GetAutomationParams{ID: id, BusinessID: businessID, TenantRootID: root})
}

func validateInTenant(ctx context.Context, tx pgx.Tx, businessID, root uuid.UUID, graph Graph) (ValidationResult, error) {
	listIDs, templateIDs := graphReferenceIDs(graph)
	refs := References{Lists: map[uuid.UUID]bool{}, Templates: map[uuid.UUID]bool{}}
	if err := loadReferences(ctx, tx, `SELECT id FROM mailing_list WHERE business_id=$1 AND tenant_root_id=$2 AND status='active' AND id=ANY($3::uuid[]) FOR SHARE`, businessID, root, listIDs, refs.Lists); err != nil {
		return ValidationResult{}, err
	}
	if err := loadReferences(ctx, tx, `SELECT id FROM mailing_template WHERE business_id=$1 AND tenant_root_id=$2 AND id=ANY($3::uuid[]) FOR SHARE`, businessID, root, templateIDs, refs.Templates); err != nil {
		return ValidationResult{}, err
	}
	issues := Validate(graph, refs)
	return ValidationResult{Valid: len(issues) == 0, Issues: issues}, nil
}

func graphReferenceIDs(graph Graph) ([]uuid.UUID, []uuid.UUID) {
	lists, templates := map[uuid.UUID]bool{}, map[uuid.UUID]bool{}
	for _, node := range graph.Nodes {
		var cfg struct {
			ListID     uuid.UUID       `json:"list_id"`
			TemplateID uuid.UUID       `json:"template_id"`
			Predicate  json.RawMessage `json:"predicate"`
		}
		if json.Unmarshal(node.Config, &cfg) != nil {
			continue
		}
		if cfg.ListID != uuid.Nil {
			lists[cfg.ListID] = true
		}
		if cfg.TemplateID != uuid.Nil {
			templates[cfg.TemplateID] = true
		}
		if len(cfg.Predicate) > 0 {
			var predicate struct {
				ListID uuid.UUID `json:"list_id"`
			}
			if json.Unmarshal(cfg.Predicate, &predicate) == nil && predicate.ListID != uuid.Nil {
				lists[predicate.ListID] = true
			}
		}
	}
	return mapKeys(lists), mapKeys(templates)
}

func loadReferences(ctx context.Context, tx pgx.Tx, query string, businessID, root uuid.UUID, ids []uuid.UUID, found map[uuid.UUID]bool) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, query, businessID, root, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err = rows.Scan(&id); err != nil {
			return err
		}
		found[id] = true
	}
	return rows.Err()
}

func mapKeys(values map[uuid.UUID]bool) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	return out
}

func triggerIdentity(graph Graph) (string, string, error) {
	for _, node := range graph.Nodes {
		if node.Kind != "trigger" {
			continue
		}
		var cfg struct {
			Type   string    `json:"type"`
			ListID uuid.UUID `json:"list_id"`
			Tag    string    `json:"tag"`
			Name   string    `json:"name"`
		}
		if err := json.Unmarshal(node.Config, &cfg); err != nil {
			return "", "", err
		}
		switch cfg.Type {
		case "list_joined":
			return cfg.Type, cfg.ListID.String(), nil
		case "tag_added":
			return cfg.Type, cfg.Tag, nil
		case "event":
			return cfg.Type, cfg.Name, nil
		}
	}
	return "", "", validation("graph has no trigger")
}

func decodeGraph(raw []byte) (Graph, error) {
	var graph Graph
	if err := json.Unmarshal(raw, &graph); err != nil {
		return Graph{}, fmt.Errorf("automation: stored graph: %w", err)
	}
	if graph.Nodes == nil {
		graph.Nodes = []Node{}
	}
	if graph.Edges == nil {
		graph.Edges = []Edge{}
	}
	return graph, nil
}

func toAutomation(row dbgen.Automation) Automation {
	return Automation{
		ID: row.ID, BusinessID: row.BusinessID, TenantRootID: row.TenantRootID,
		Name: row.Name, Description: row.Description, Status: string(row.Status), AllowReenroll: row.AllowReenroll,
		ActiveVersionID: uuidPtr(row.ActiveVersionID), DraftVersionID: uuidPtr(row.DraftVersionID), CreatedByPrincipalID: uuidPtr(row.CreatedByPrincipalID),
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func toVersion(row dbgen.AutomationVersion) (Version, error) {
	graph, err := decodeGraph(row.Graph)
	if err != nil {
		return Version{}, err
	}
	return Version{
		ID: row.ID, BusinessID: row.BusinessID, TenantRootID: row.TenantRootID, AutomationID: row.AutomationID,
		Number: row.Number, Status: string(row.Status), Graph: graph, TriggerKind: row.TriggerKind, TriggerRef: row.TriggerRef,
		ActivatedAt: timePtr(row.ActivatedAt), CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}, nil
}

func uuidPtr(value pgtype.UUID) *uuid.UUID {
	if !value.Valid {
		return nil
	}
	id := uuid.UUID(value.Bytes)
	return &id
}

func timePtr(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	return &value.Time
}

func resolveTenantRoot(ctx context.Context, q *dbgen.Queries, businessID uuid.UUID) (uuid.UUID, error) {
	business, err := q.GetBusiness(ctx, businessID)
	if err != nil {
		return uuid.Nil, err
	}
	return business.TenantRootID, nil
}

func validateDefinition(name string, description *string) error {
	if name == "" || len(name) > 200 {
		return validation("name is required and must not exceed 200 characters")
	}
	if description != nil && len(*description) > 5000 {
		return validation("description must not exceed 5000 characters")
	}
	return nil
}

func cleanOptional(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func validation(message string) error {
	return fmt.Errorf("automation: %s: %w", message, errs.ErrValidation)
}

func mapErr(err error) error {
	var invalid *InvalidGraphError
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &invalid):
		return invalid
	case errors.Is(err, pgx.ErrNoRows), errors.Is(err, errs.ErrNotFound):
		return fmt.Errorf("automation: not found: %w", errs.ErrNotFound)
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return fmt.Errorf("automation: duplicate: %w", errs.ErrConflict)
	case errors.As(err, &pgErr) && pgErr.Code == "23503":
		return fmt.Errorf("automation: foreign key: %w", errs.ErrNotFound)
	case errors.As(err, &pgErr) && pgErr.Code == "23514":
		return validation("invalid value")
	case errors.Is(err, errs.ErrValidation), errors.Is(err, errs.ErrConflict):
		return err
	default:
		return fmt.Errorf("automation: query: %w", err)
	}
}

func writeAudit(ctx context.Context, tx pgx.Tx, principalID, businessID, root uuid.UUID, action string, targetID uuid.UUID, value any) error {
	targetType := "automation"
	return audit.Write(ctx, tx, audit.Entry{BusinessID: &businessID, TenantRootID: &root, ActorPrincipalID: &principalID, Action: action, TargetType: &targetType, TargetID: &targetID, NewValue: value})
}

func clampLimit(limit int) int {
	if limit <= 0 {
		return defaultPageSize
	}
	if limit > maxPageSize {
		return maxPageSize
	}
	return limit
}

func encodeCursor(at time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursorKind + "|" + at.UTC().Format(time.RFC3339Nano) + "|" + id.String()))
}

func decodeCursor(value string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return time.Time{}, uuid.Nil, validation("invalid cursor")
	}
	parts := strings.SplitN(string(raw), "|", 3)
	if len(parts) != 3 || parts[0] != cursorKind {
		return time.Time{}, uuid.Nil, validation("invalid cursor")
	}
	at, err := time.Parse(time.RFC3339Nano, parts[1])
	if err != nil {
		return time.Time{}, uuid.Nil, validation("invalid cursor")
	}
	id, err := uuid.Parse(parts[2])
	if err != nil {
		return time.Time{}, uuid.Nil, validation("invalid cursor")
	}
	return at, id, nil
}
