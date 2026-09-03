package automations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type TransactionDB interface {
	WithTx(context.Context, func(pgx.Tx) error) error
}

// SQLStepStore persists engine transitions exclusively through the fenced
// SECURITY DEFINER functions. Every method uses the transaction supplied by
// the stepper so a complete Advance call commits atomically.
type SQLStepStore struct{}

func (SQLStepStore) Record(ctx context.Context, tx pgx.Tx, record StepRecord) (bool, error) {
	detail, err := json.Marshal(record.Detail)
	if err != nil {
		return false, err
	}
	var changed bool
	err = tx.QueryRow(ctx, `SELECT automation_record_step(
		$1,$2,$3,$4,$5::automation_step_outcome,$6,$7,$8::automation_enrollment_status,$9,$10::jsonb,$11)`,
		record.EnrollmentID, record.ClaimGeneration, record.NodeID, record.NodeKind,
		record.Outcome, record.NextNodeID, record.WakeAt, record.Status,
		record.DeliveryID, detail, record.RecordedAt,
	).Scan(&changed)
	return changed, err
}

func (SQLStepStore) Fail(ctx context.Context, tx pgx.Tx, failure StepFailure) (bool, error) {
	var changed bool
	err := tx.QueryRow(ctx, "SELECT automation_fail_step($1,$2,$3,$4,$5)",
		failure.EnrollmentID, failure.ClaimGeneration, failure.Error,
		failure.Terminal, failure.RetryAt,
	).Scan(&changed)
	return changed, err
}

func (SQLStepStore) Waiting(ctx context.Context, tx pgx.Tx, enrollmentID uuid.UUID, nodeID string) (bool, error) {
	var waiting bool
	err := tx.QueryRow(ctx, "SELECT automation_step_waiting($1,$2)", enrollmentID, nodeID).Scan(&waiting)
	return waiting, err
}

func (SQLStepStore) Delivery(ctx context.Context, tx pgx.Tx, enrollmentID uuid.UUID, nodeID string) (*uuid.UUID, error) {
	var value pgtype.UUID
	if err := tx.QueryRow(ctx, "SELECT automation_step_delivery($1,$2)", enrollmentID, nodeID).Scan(&value); err != nil {
		return nil, err
	}
	if !value.Valid {
		return nil, nil
	}
	id := uuid.UUID(value.Bytes)
	return &id, nil
}

func (SQLStepStore) EventExists(ctx context.Context, tx pgx.Tx, businessID uuid.UUID, email, name string, since time.Time, within *time.Duration) (bool, error) {
	var interval pgtype.Interval
	if within != nil {
		interval = pgtype.Interval{Microseconds: within.Microseconds(), Valid: true}
	}
	var exists bool
	err := tx.QueryRow(ctx, "SELECT automation_event_exists($1,$2,$3,$4,$5)", businessID, email, name, since, interval).Scan(&exists)
	return exists, err
}

type Stepper struct {
	DB              TransactionDB
	Deps            Deps
	Every           time.Duration
	Batch           int
	Lease           time.Duration
	MaxNodesPerTick int
	MaxNodeAttempts int
	Now             func() time.Time
	Logger          *slog.Logger
}

func (s *Stepper) Run(ctx context.Context) {
	if s == nil || s.DB == nil {
		return
	}
	if err := s.Tick(ctx); err != nil {
		s.logger().ErrorContext(ctx, "automation stepper tick failed", "err", err)
	}
	every := s.Every
	if every <= 0 {
		every = 5 * time.Second
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Tick(ctx); err != nil {
				s.logger().ErrorContext(ctx, "automation stepper tick failed", "err", err)
			}
		}
	}
}

// Tick greedily drains full claim batches. Claiming and advancing use separate
// transactions so a crashed Advance leaves a lease that another replica can
// reclaim after expiry.
func (s *Stepper) Tick(ctx context.Context) error {
	if s == nil || s.DB == nil {
		return errors.New("automation stepper is not configured")
	}
	batch := s.Batch
	if batch <= 0 {
		batch = 50
	}
	lease := s.Lease
	if lease <= 0 {
		lease = 2 * time.Minute
	}
	for {
		claimed, err := s.claim(ctx, s.now(), batch, lease)
		if err != nil {
			return err
		}
		var firstErr error
		for _, enrollment := range claimed {
			err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
				engine := Engine{Deps: s.Deps, MaxNodesPerTick: s.MaxNodesPerTick, MaxNodeAttempts: s.MaxNodeAttempts}
				outcome, err := engine.Advance(ctx, tx, enrollment.Enrollment, enrollment.Graph, s.now())
				if err == nil && outcome.LastError != "" {
					s.logger().WarnContext(ctx, "automation node failed", "enrollment_id", enrollment.ID, "terminal", outcome.Status == "errored", "err", outcome.LastError)
				}
				return err
			})
			if err != nil && firstErr == nil {
				firstErr = fmt.Errorf("advance enrollment %s: %w", enrollment.ID, err)
			}
		}
		if firstErr != nil {
			return firstErr
		}
		if len(claimed) < batch {
			return nil
		}
	}
}

type claimedEnrollment struct {
	Enrollment
	Graph Graph
}

func (s *Stepper) claim(ctx context.Context, now time.Time, batch int, lease time.Duration) ([]claimedEnrollment, error) {
	var claimed []claimedEnrollment
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT enrollment_id,business_id,tenant_root_id,automation_id,
			version_id,subscriber_id,current_node_id,wake_at,enrolled_at,node_attempts,
			claim_generation,graph FROM automation_claim_due($1,$2,$3)`, now, batch, lease)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item claimedEnrollment
			var raw []byte
			if err := rows.Scan(&item.ID, &item.BusinessID, &item.TenantRootID,
				&item.AutomationID, &item.VersionID, &item.SubscriberID,
				&item.CurrentNodeID, &item.WakeAt, &item.EnrolledAt, &item.NodeAttempts,
				&item.ClaimGeneration, &raw); err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &item.Graph); err != nil {
				return fmt.Errorf("decode graph for enrollment %s: %w", item.ID, err)
			}
			claimed = append(claimed, item)
		}
		return rows.Err()
	})
	return claimed, err
}

func (s *Stepper) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Stepper) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}
