package tenancy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/manyforge/manyforge/internal/platform/auth"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
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

// TenantMergeReconciliationTable is the exact operation authorized for one
// manifest table. Cutover's common write trigger consumes this action for every
// source-to-destination root rewrite.
type TenantMergeReconciliationTable struct {
	Action         string `json:"action"`
	Rows           int64  `json:"rows"`
	StableIDDigest string `json:"stable_id_digest"`
}

// TenantMergeReconciliationPolicy describes one explicit v1 identity/access
// rule. The optional digests pin indirectly-scoped state such as custom-role
// permissions without exposing tenant identity values.
type TenantMergeReconciliationPolicy struct {
	Key              string `json:"key"`
	Action           string `json:"action"`
	Count            int64  `json:"count"`
	IdentityDigest   string `json:"identity_digest,omitempty"`
	PermissionCount  int64  `json:"permission_count,omitempty"`
	PermissionDigest string `json:"permission_digest,omitempty"`
}

// TenantMergeReconciliationAccess records the access invariants that must
// survive cutover. Existing grants stay anchored at their original business.
type TenantMergeReconciliationAccess struct {
	SourceDirectOwners      int64  `json:"source_direct_owners"`
	DestinationDirectOwners int64  `json:"destination_direct_owners"`
	SourceMemberships       int64  `json:"source_memberships"`
	ScopeRule               string `json:"scope_rule"`
}

// TenantMergeReconciliationPlan is the versioned, immutable decision consumed
// by cutover. V1 supports lossless root rewrites only; every collision blocks.
type TenantMergeReconciliationPlan struct {
	Version           int32                                     `json:"version"`
	Mode              string                                    `json:"mode"`
	SourceRootID      uuid.UUID                                 `json:"source_root_id"`
	DestinationRootID uuid.UUID                                 `json:"destination_root_id"`
	Tables            map[string]TenantMergeReconciliationTable `json:"tables"`
	Access            TenantMergeReconciliationAccess           `json:"access"`
	Policies          []TenantMergeReconciliationPolicy         `json:"policies"`
	Conflicts         []TenantMergeFinding                      `json:"conflicts"`
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

// TenantMergeFailure is the safe status detail returned to a tenant owner.
// Raw PostgreSQL messages stay in the control-plane event table for operators;
// the API exposes only the failed stage and correlation ID.
type TenantMergeFailure struct {
	Code                  string    `json:"code"`
	Stage                 string    `json:"stage"`
	OperatorCorrelationID uuid.UUID `json:"operator_correlation_id"`
}

// TenantMergeAuditManifest is the immutable success receipt written in the
// same transaction as cutover. It contains identifiers, aggregate counts, and
// digests, but no tenant PII or credentials.
type TenantMergeAuditManifest struct {
	OperationID           uuid.UUID                         `json:"operation_id"`
	CorrelationID         uuid.UUID                         `json:"correlation_id"`
	ActorPrincipalID      uuid.UUID                         `json:"actor_principal_id"`
	SourceRootID          uuid.UUID                         `json:"source_root_id"`
	DestinationRootID     uuid.UUID                         `json:"destination_root_id"`
	DestinationParentID   uuid.UUID                         `json:"destination_parent_id"`
	InventoryVersion      int32                             `json:"inventory_version"`
	SchemaVersion         int64                             `json:"schema_version"`
	SchemaHash            string                            `json:"schema_hash"`
	PreflightGeneration   string                            `json:"preflight_generation"`
	ReconciliationVersion int32                             `json:"reconciliation_version"`
	ReconciliationHash    string                            `json:"reconciliation_hash"`
	TableMetrics          map[string]TenantMergeTableMetric `json:"table_metrics"`
	TableCounts           map[string]int64                  `json:"table_counts"`
	ModuleCounts          map[string]TenantMergeCount       `json:"module_counts"`
	AffectedRows          int64                             `json:"affected_rows"`
	EstimatedBytes        int64                             `json:"estimated_bytes"`
	Warnings              []TenantMergeFinding              `json:"warnings"`
	Resolutions           []TenantMergeFinding              `json:"resolutions"`
	StartedAt             time.Time                         `json:"started_at"`
	CompletedAt           time.Time                         `json:"completed_at"`
	CreatedAt             time.Time                         `json:"created_at"`
}

// TenantMergeOperation is the durable preflight result consumed by the later
// cutover primitive.
type TenantMergeOperation struct {
	ID                    uuid.UUID                         `json:"id"`
	CorrelationID         uuid.UUID                         `json:"correlation_id"`
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
	ReconciliationVersion *int32                            `json:"reconciliation_version"`
	ReconciliationHash    *string                           `json:"reconciliation_hash"`
	ReconciliationPlan    *TenantMergeReconciliationPlan    `json:"reconciliation_plan"`
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
	AttachmentsStagedAt   *time.Time                        `json:"attachments_staged_at"`
	PreflightCompletedAt  *time.Time                        `json:"preflight_completed_at"`
	ReadyAt               *time.Time                        `json:"ready_at"`
	ConfirmedAt           *time.Time                        `json:"confirmed_at"`
	CreatedAt             time.Time                         `json:"created_at"`
	UpdatedAt             time.Time                         `json:"updated_at"`
	Events                []TenantMergeEvent                `json:"events"`
	Failure               *TenantMergeFailure               `json:"failure"`
	Manifest              *TenantMergeAuditManifest         `json:"manifest"`
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
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "TM400":
			return fmt.Errorf("confirmation names must exactly match the current source and destination names: %w", errs.ErrValidation)
		case "TM409":
			return fmt.Errorf("tenant merge operation conflicts with current state: %w", errs.ErrConflict)
		case "TM412":
			return fmt.Errorf("tenant merge preflight is stale: %w", errs.ErrStalePrecondition)
		}
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
	if operation.Status != "ready" && operation.Status != "running" {
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

// ConfirmTenantMerge verifies the actor's current password, records exact typed
// source/destination confirmation against the current preflight generation, and
// immediately starts or safely resumes cutover. Replays never create a second
// operation: terminal/running status is returned from the durable operation.
func (s *Service) ConfirmTenantMerge(
	ctx context.Context,
	actorID, operationID uuid.UUID,
	sourceName, destinationName, password string,
) (TenantMergeOperation, error) {
	if sourceName == "" || destinationName == "" || password == "" {
		return TenantMergeOperation{}, fmt.Errorf(
			"source_name, destination_name, and password are required: %w",
			errs.ErrValidation,
		)
	}
	if _, err := s.GetTenantMergeOperation(ctx, actorID, operationID); err != nil {
		return TenantMergeOperation{}, err
	}
	if err := s.verifyTenantMergePassword(ctx, actorID, password); err != nil {
		return TenantMergeOperation{}, err
	}

	proof := sha256.Sum256([]byte(
		actorID.String() + "|" + operationID.String() + "|" +
			sourceName + "|" + destinationName,
	))
	var operation TenantMergeOperation
	err := s.DB.WithPrincipal(ctx, actorID, func(tx pgx.Tx) error {
		var raw []byte
		err := tx.QueryRow(ctx,
			"SELECT tenant_merge_confirm($1, $2, $3, $4, $5)",
			actorID, operationID, sourceName, destinationName,
			fmt.Sprintf("%x", proof[:]),
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
	if operation.Status == "preflight_required" {
		return TenantMergeOperation{}, errs.ErrStalePrecondition
	}
	if operation.Status != "ready" && operation.Status != "running" {
		return operation, nil
	}
	if err := s.stageTenantMergeAttachments(ctx, actorID, operation); err != nil {
		return TenantMergeOperation{}, err
	}
	operation, err = s.CutoverTenantMerge(ctx, actorID, operationID)
	if err != nil {
		return TenantMergeOperation{}, err
	}
	if operation.Status == "preflight_required" {
		return TenantMergeOperation{}, errs.ErrStalePrecondition
	}
	return operation, nil
}

type tenantMergeAttachment struct {
	Key         string
	ContentType string
	Size        int64
}

// stageTenantMergeAttachments copies every source object to its post-merge key
// before the database fence/cutover. The copy is idempotent; preflight
// generation is recorded only after every object has been verified and written.
func (s *Service) stageTenantMergeAttachments(
	ctx context.Context,
	actorID uuid.UUID,
	operation TenantMergeOperation,
) error {
	if operation.AttachmentCount == 0 {
		return nil
	}
	if s.Blob == nil {
		return fmt.Errorf(
			"attachment storage is unavailable for this merge: %w",
			errs.ErrConflict,
		)
	}

	attachments := make([]tenantMergeAttachment, 0, operation.AttachmentCount)
	err := s.DB.WithPrincipal(ctx, actorID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT blob_key, content_type, size
			FROM attachment
			WHERE tenant_root_id = $1
			ORDER BY id`,
			operation.SourceRootID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var attachment tenantMergeAttachment
			if err := rows.Scan(
				&attachment.Key, &attachment.ContentType, &attachment.Size,
			); err != nil {
				return err
			}
			attachments = append(attachments, attachment)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	if int64(len(attachments)) != operation.AttachmentCount {
		return errs.ErrStalePrecondition
	}

	sourcePrefix := operation.SourceRootID.String() + "/"
	destinationPrefix := operation.DestinationRootID.String() + "/"
	var stagedBytes int64
	for _, attachment := range attachments {
		if !strings.HasPrefix(attachment.Key, sourcePrefix) {
			return fmt.Errorf(
				"source attachment key is outside its tenant namespace: %w",
				errs.ErrConflict,
			)
		}
		content, err := s.Blob.Get(ctx, attachment.Key)
		if err != nil {
			return fmt.Errorf("read source attachment for tenant merge: %w", err)
		}
		if int64(len(content)) != attachment.Size {
			return fmt.Errorf(
				"source attachment size changed: %w",
				errs.ErrStalePrecondition,
			)
		}
		destinationKey := destinationPrefix +
			strings.TrimPrefix(attachment.Key, sourcePrefix)
		if err := s.Blob.Put(
			ctx, destinationKey, content, attachment.ContentType,
		); err != nil {
			return fmt.Errorf("stage destination attachment for tenant merge: %w", err)
		}
		stagedBytes += attachment.Size
	}
	if stagedBytes != operation.AttachmentBytes {
		return errs.ErrStalePrecondition
	}

	return s.DB.WithPrincipal(ctx, actorID, func(tx pgx.Tx) error {
		var raw []byte
		err := tx.QueryRow(ctx,
			"SELECT tenant_merge_mark_attachments_staged($1, $2, $3, $4)",
			actorID, operation.ID, int64(len(attachments)), stagedBytes,
		).Scan(&raw)
		if err != nil {
			return tenantMergeDBError(err)
		}
		staged, err := decodeTenantMergeOperation(raw)
		if err != nil {
			return err
		}
		if staged.Status == "preflight_required" {
			return errs.ErrStalePrecondition
		}
		if staged.Status != "ready" || staged.AttachmentsStagedAt == nil {
			return fmt.Errorf(
				"attachment staging was not accepted: %w",
				errs.ErrConflict,
			)
		}
		return nil
	})
}

func (s *Service) verifyTenantMergePassword(
	ctx context.Context,
	actorID uuid.UUID,
	password string,
) error {
	verified := false
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		account, err := dbgen.New(tx).GetAccountByPrincipal(ctx, actorID)
		if err != nil {
			auth.DummyVerify(password)
			return nil
		}
		if account.PasswordHash == nil {
			auth.DummyVerify(password)
			return nil
		}
		if account.Status != "active" {
			auth.DummyVerify(password)
			return nil
		}
		verified = auth.VerifyPassword(password, *account.PasswordHash) == nil
		return nil
	})
	if err != nil {
		return err
	}
	if !verified {
		return errs.ErrReauthenticationRequired
	}
	return nil
}
