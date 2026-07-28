package tenancy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/errs"
)

// TenantMergeFinding is a machine-readable preflight blocker or warning. Values
// that could contain tenant PII (email addresses, domains, blob keys) are never
// included; Object and Count are sufficient to locate and resolve the conflict.
type TenantMergeFinding struct {
	Code    string         `json:"code"`
	Module  string         `json:"module"`
	Object  string         `json:"object"`
	Count   int64          `json:"count"`
	Limit   int64          `json:"limit,omitempty"`
	Bytes   int64          `json:"bytes,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

// TenantMergeCount is the deterministic source-root count and logical size for
// one module.
type TenantMergeCount struct {
	Rows  int64 `json:"rows"`
	Bytes int64 `json:"bytes"`
}

// TenantMergeTableMetric is the immutable preflight fingerprint for one
// manifest table.
type TenantMergeTableMetric struct {
	Module         string `json:"module"`
	Strategy       string `json:"strategy"`
	Rows           int64  `json:"rows"`
	Bytes          int64  `json:"bytes"`
	ContentDigest  string `json:"content_digest"`
	StableIDDigest string `json:"stable_id_digest"`
}

// TenantMergeEvent is the append-only transition audit attached to an
// operation. It is control-plane metadata, not tenant data.
type TenantMergeEvent struct {
	ID               int64          `json:"id"`
	OperationID      uuid.UUID      `json:"operation_id"`
	ActorPrincipalID uuid.UUID      `json:"actor_principal_id"`
	FromStatus       *string        `json:"from_status"`
	ToStatus         string         `json:"to_status"`
	Event            string         `json:"event"`
	Metadata         map[string]any `json:"metadata"`
	CreatedAt        time.Time      `json:"created_at"`
}

// TenantMergeOperation is the durable preflight result consumed by the later
// cutover primitive.
type TenantMergeOperation struct {
	ID                    uuid.UUID                         `json:"id"`
	SourceRootID          uuid.UUID                         `json:"source_root_id"`
	DestinationParentID   uuid.UUID                         `json:"destination_parent_id"`
	DestinationRootID     uuid.UUID                         `json:"destination_root_id"`
	ActorPrincipalID      uuid.UUID                         `json:"actor_principal_id"`
	IdempotencyKey        string                            `json:"idempotency_key"`
	Status                string                            `json:"status"`
	InventoryVersion      *int32                            `json:"inventory_version"`
	SchemaVersion         *int64                            `json:"schema_version"`
	SchemaHash            *string                           `json:"schema_hash"`
	SourceGeneration      *string                           `json:"source_generation"`
	DestinationGeneration *string                           `json:"destination_generation"`
	PreflightGeneration   *string                           `json:"preflight_generation"`
	TableMetrics          map[string]TenantMergeTableMetric `json:"table_metrics"`
	ModuleCounts          map[string]TenantMergeCount       `json:"module_counts"`
	Conflicts             []TenantMergeFinding              `json:"conflicts"`
	Warnings              []TenantMergeFinding              `json:"warnings"`
	AffectedRows          int64                             `json:"affected_rows"`
	EstimatedBytes        int64                             `json:"estimated_bytes"`
	SourceBusinesses      *int32                            `json:"source_businesses"`
	ResultingDepth        *int32                            `json:"resulting_depth"`
	AttachmentCount       int64                             `json:"attachment_count"`
	AttachmentBytes       int64                             `json:"attachment_bytes"`
	PreflightCompletedAt  *time.Time                        `json:"preflight_completed_at"`
	ReadyAt               *time.Time                        `json:"ready_at"`
	CreatedAt             time.Time                         `json:"created_at"`
	UpdatedAt             time.Time                         `json:"updated_at"`
	Events                []TenantMergeEvent                `json:"events"`
}

// ValidateTenantMergeResult reports whether the stored ready generation still
// matches the complete current schema/source/destination snapshot.
type ValidateTenantMergeResult struct {
	Current   bool                 `json:"current"`
	Operation TenantMergeOperation `json:"operation"`
}

func decodeTenantMergeOperation(raw []byte) (TenantMergeOperation, error) {
	var operation TenantMergeOperation
	if err := json.Unmarshal(raw, &operation); err != nil {
		return TenantMergeOperation{}, fmt.Errorf("decode tenant merge operation: %w", err)
	}
	return operation, nil
}

func tenantMergeDBError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errs.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "TM409" {
		return fmt.Errorf("tenant root already belongs to another active merge: %w", errs.ErrConflict)
	}
	return err
}

// CreateTenantMergeOperation binds an actor, source root, destination parent
// and idempotency key. Unknown and unauthorized IDs both return ErrNotFound.
// Replaying the same request returns the same operation; reusing its key for a
// different authorized request returns ErrConflict.
func (s *Service) CreateTenantMergeOperation(
	ctx context.Context,
	actorID, sourceRootID, destinationParentID uuid.UUID,
	idempotencyKey string,
) (TenantMergeOperation, error) {
	if len(idempotencyKey) == 0 || len(idempotencyKey) > 255 {
		return TenantMergeOperation{}, fmt.Errorf("idempotency_key must contain 1 to 255 characters: %w", errs.ErrValidation)
	}

	var operation TenantMergeOperation
	err := s.DB.WithPrincipal(ctx, actorID, func(tx pgx.Tx) error {
		var raw []byte
		err := tx.QueryRow(ctx,
			"SELECT tenant_merge_create($1, $2, $3, $4)",
			actorID, sourceRootID, destinationParentID, idempotencyKey,
		).Scan(&raw)
		if err != nil {
			return tenantMergeDBError(err)
		}
		operation, err = decodeTenantMergeOperation(raw)
		return err
	})
	return operation, err
}

// GetTenantMergeOperation returns durable status and transition history to the
// actor who created the operation. Other actors and unknown operation IDs are
// indistinguishable.
func (s *Service) GetTenantMergeOperation(
	ctx context.Context,
	actorID, operationID uuid.UUID,
) (TenantMergeOperation, error) {
	var operation TenantMergeOperation
	err := s.DB.WithPrincipal(ctx, actorID, func(tx pgx.Tx) error {
		var raw []byte
		err := tx.QueryRow(ctx,
			"SELECT tenant_merge_get($1, $2)",
			actorID, operationID,
		).Scan(&raw)
		if err != nil {
			return tenantMergeDBError(err)
		}
		operation, err = decodeTenantMergeOperation(raw)
		return err
	})
	return operation, err
}

// PreflightTenantMerge takes one statement-level PostgreSQL snapshot, computes
// every manifest table's count/size/content digest, reports all known blockers,
// and persists the resulting generation. It never mutates tenant-owned rows.
func (s *Service) PreflightTenantMerge(
	ctx context.Context,
	actorID, operationID uuid.UUID,
) (TenantMergeOperation, error) {
	var operation TenantMergeOperation
	err := s.DB.WithPrincipal(ctx, actorID, func(tx pgx.Tx) error {
		var raw []byte
		err := tx.QueryRow(ctx,
			"SELECT tenant_merge_preflight($1, $2)",
			actorID, operationID,
		).Scan(&raw)
		if err != nil {
			return tenantMergeDBError(err)
		}
		operation, err = decodeTenantMergeOperation(raw)
		return err
	})
	return operation, err
}

// ValidateTenantMergePreflight recomputes the complete generation. A mismatch
// atomically transitions ready back to preflight_required and appends a stale
// audit event. The later cutover primitive must call the same contract while
// holding both exclusive root locks.
func (s *Service) ValidateTenantMergePreflight(
	ctx context.Context,
	actorID, operationID uuid.UUID,
) (ValidateTenantMergeResult, error) {
	var result ValidateTenantMergeResult
	err := s.DB.WithPrincipal(ctx, actorID, func(tx pgx.Tx) error {
		var raw []byte
		err := tx.QueryRow(ctx,
			"SELECT tenant_merge_validate_preflight($1, $2)",
			actorID, operationID,
		).Scan(&raw)
		if err != nil {
			return tenantMergeDBError(err)
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return fmt.Errorf("decode tenant merge validation: %w", err)
		}
		return nil
	})
	return result, err
}

// BeginTenantMergeFence publishes a durable two-root maintenance fence after
// draining earlier writers and workers. Replays are idempotent, which lets a
// process resume after crashing between the fence and cutover transactions.
func (s *Service) BeginTenantMergeFence(
	ctx context.Context,
	actorID, operationID uuid.UUID,
) (TenantMergeOperation, error) {
	return s.runTenantMergeControl(ctx, actorID, operationID, "tenant_merge_begin_fence")
}

// ReleaseTenantMergeFence unpauses roots only after the operation is terminal
// or its preflight was invalidated. It drains worker lock namespaces again so
// no transaction with a pre-cutover snapshot can resume after release.
func (s *Service) ReleaseTenantMergeFence(
	ctx context.Context,
	actorID, operationID uuid.UUID,
) (TenantMergeOperation, error) {
	return s.runTenantMergeControl(ctx, actorID, operationID, "tenant_merge_release_fence")
}

// CancelTenantMergeFence recovers a ready operation that was fenced but never
// entered cutover. It invalidates the preflight before releasing the roots.
func (s *Service) CancelTenantMergeFence(
	ctx context.Context,
	actorID, operationID uuid.UUID,
) (TenantMergeOperation, error) {
	return s.runTenantMergeControl(ctx, actorID, operationID, "tenant_merge_cancel_fence")
}

func (s *Service) runTenantMergeControl(
	ctx context.Context,
	actorID, operationID uuid.UUID,
	function string,
) (TenantMergeOperation, error) {
	var operation TenantMergeOperation
	err := s.DB.WithPrincipal(ctx, actorID, func(tx pgx.Tx) error {
		var raw []byte
		query := "SELECT " + function + "($1, $2)"
		err := tx.QueryRow(ctx, query, actorID, operationID).Scan(&raw)
		if err != nil {
			return tenantMergeDBError(err)
		}
		operation, err = decodeTenantMergeOperation(raw)
		return err
	})
	return operation, err
}

// CutoverTenantMerge first commits the durable fence, then atomically moves the
// validated source tenant beneath its destination parent. The database cutover
// accepts only the operation ID; it derives and rechecks the actor, roots,
// authorization, and complete preflight generation while holding both roots.
//
// A non-ready or newly-stale operation is returned without mutating tenant
// rows. A failed cutover returns durable status "failed" after its internal
// subtransaction has rolled back every hierarchy and tenant-row change. A
// successful call releases the fence only after that cutover transaction has
// committed; an interrupted call deliberately leaves the durable fence for a
// retry or explicit CancelTenantMergeFence recovery.
func (s *Service) CutoverTenantMerge(
	ctx context.Context,
	actorID, operationID uuid.UUID,
) (TenantMergeOperation, error) {
	operation, err := s.BeginTenantMergeFence(ctx, actorID, operationID)
	if err != nil {
		return TenantMergeOperation{}, err
	}
	if operation.Status != "ready" {
		if operation.Status == "preflight_required" ||
			operation.Status == "succeeded" ||
			operation.Status == "failed" {
			return s.ReleaseTenantMergeFence(ctx, actorID, operationID)
		}
		return operation, nil
	}

	err = s.DB.WithPrincipal(ctx, actorID, func(tx pgx.Tx) error {
		var raw []byte
		err := tx.QueryRow(ctx,
			"SELECT tenant_merge_cutover($1)",
			operationID,
		).Scan(&raw)
		if err != nil {
			return tenantMergeDBError(err)
		}
		operation, err = decodeTenantMergeOperation(raw)
		return err
	})
	if err != nil {
		return TenantMergeOperation{}, err
	}
	if operation.Status == "preflight_required" ||
		operation.Status == "succeeded" ||
		operation.Status == "failed" {
		return s.ReleaseTenantMergeFence(ctx, actorID, operationID)
	}
	return operation, nil
}
