package automations

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

const enrollmentCursorKind = "automation-enrollment"

var enrollmentNodeIDPattern = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

func (s *Service) ListEnrollments(ctx context.Context, principalID, businessID, automationID uuid.UUID, filter EnrollmentFilter) (Page[EnrollmentView], error) {
	filter.Limit = clampLimit(filter.Limit)
	if filter.Status != "" && !validEnrollmentStatus(filter.Status) {
		return Page[EnrollmentView]{}, validation("invalid enrollment status")
	}
	if filter.NodeID != "" && !enrollmentNodeIDPattern.MatchString(filter.NodeID) {
		return Page[EnrollmentView]{}, validation("invalid node_id")
	}
	cursorAt := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
	cursorID := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
	if filter.Cursor != "" {
		var err error
		cursorAt, cursorID, err = decodeTypedCursor(enrollmentCursorKind, filter.Cursor)
		if err != nil {
			return Page[EnrollmentView]{}, err
		}
	}
	var out Page[EnrollmentView]
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		if _, err = q.GetAutomation(ctx, dbgen.GetAutomationParams{ID: automationID, BusinessID: businessID, TenantRootID: root}); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id,business_id,tenant_root_id,automation_id,version_id,subscriber_id,
			status::text,current_node_id,wake_at,node_attempts,last_error,exit_reason,source_event_id,
			enrolled_at,finished_at,updated_at
			FROM automation_enrollment
			WHERE automation_id=$1 AND business_id=$2 AND tenant_root_id=$3
			  AND ($4='' OR status::text=$4) AND ($5='' OR current_node_id=$5)
			  AND (enrolled_at,id) < ($6,$7)
			ORDER BY enrolled_at DESC,id DESC LIMIT $8`, automationID, businessID, root,
			filter.Status, filter.NodeID, cursorAt, cursorID, filter.Limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			value, scanErr := scanEnrollment(rows)
			if scanErr != nil {
				return scanErr
			}
			out.Items = append(out.Items, value)
		}
		if err = rows.Err(); err != nil {
			return err
		}
		if len(out.Items) > filter.Limit {
			out.Items = out.Items[:filter.Limit]
			last := out.Items[len(out.Items)-1]
			cursor := encodeTypedCursor(enrollmentCursorKind, last.EnrolledAt, last.ID)
			out.NextCursor = &cursor
		}
		if out.Items == nil {
			out.Items = []EnrollmentView{}
		}
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) GetEnrollment(ctx context.Context, principalID, businessID, automationID, enrollmentID uuid.UUID) (EnrollmentDetail, error) {
	var out EnrollmentDetail
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `SELECT id,business_id,tenant_root_id,automation_id,version_id,subscriber_id,
			status::text,current_node_id,wake_at,node_attempts,last_error,exit_reason,source_event_id,
			enrolled_at,finished_at,updated_at FROM automation_enrollment
			WHERE id=$1 AND automation_id=$2 AND business_id=$3 AND tenant_root_id=$4`, enrollmentID, automationID, businessID, root)
		out.Enrollment, err = scanEnrollment(row)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id,node_id,node_kind,attempt,entered_at,completed_at,outcome::text,delivery_id,detail
			FROM automation_enrollment_step WHERE enrollment_id=$1 AND business_id=$2 AND tenant_root_id=$3
			ORDER BY entered_at,id`, enrollmentID, businessID, root)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var step EnrollmentStepView
			var completed pgtype.Timestamptz
			var delivery pgtype.UUID
			if err = rows.Scan(&step.ID, &step.NodeID, &step.NodeKind, &step.Attempt, &step.EnteredAt, &completed, &step.Outcome, &delivery, &step.Detail); err != nil {
				return err
			}
			step.CompletedAt = timePtr(completed)
			step.DeliveryID = uuidPtr(delivery)
			out.Steps = append(out.Steps, step)
		}
		if out.Steps == nil {
			out.Steps = []EnrollmentStepView{}
		}
		return rows.Err()
	})
	return out, mapErr(err)
}

func (s *Service) Enroll(ctx context.Context, principalID, businessID, automationID, subscriberID uuid.UUID) (EnrollmentView, error) {
	var out EnrollmentView
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
		if automation.Status != dbgen.AutomationStatusActive || !automation.ActiveVersionID.Valid {
			return errs.ErrConflict
		}
		versionID := uuid.UUID(automation.ActiveVersionID.Bytes)
		version, err := q.GetAutomationVersion(ctx, dbgen.GetAutomationVersionParams{ID: versionID, AutomationID: automationID, BusinessID: businessID, TenantRootID: root})
		if err != nil {
			return err
		}
		graph, err := decodeGraph(version.Graph)
		if err != nil {
			return err
		}
		triggerNode, listID, err := triggerNodeAndList(graph)
		if err != nil {
			return err
		}
		var subscriberList uuid.UUID
		var subscriberStatus string
		if err = tx.QueryRow(ctx, `SELECT list_id,status::text FROM list_subscriber
			WHERE id=$1 AND business_id=$2 AND tenant_root_id=$3`, subscriberID, businessID, root).Scan(&subscriberList, &subscriberStatus); err != nil {
			return err
		}
		if subscriberList != listID || subscriberStatus != "active" {
			return pgx.ErrNoRows
		}
		if !automation.AllowReenroll {
			var exists bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM automation_enrollment
				WHERE automation_id=$1 AND subscriber_id=$2)`, automationID, subscriberID).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return errs.ErrConflict
			}
		}
		enrollmentID := uuid.New()
		row := tx.QueryRow(ctx, `INSERT INTO automation_enrollment
			(id,business_id,tenant_root_id,automation_id,version_id,subscriber_id,status,current_node_id,wake_at,enrolled_at,updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,'active',$7,now(),now(),now())
			RETURNING id,business_id,tenant_root_id,automation_id,version_id,subscriber_id,status::text,
			current_node_id,wake_at,node_attempts,last_error,exit_reason,source_event_id,enrolled_at,finished_at,updated_at`,
			enrollmentID, businessID, root, automationID, versionID, subscriberID, triggerNode)
		out, err = scanEnrollment(row)
		if err != nil {
			return err
		}
		return writeAudit(ctx, tx, principalID, businessID, root, "automation.enrollment.created", automationID, map[string]any{"enrollment_id": enrollmentID, "subscriber_id": subscriberID})
	})
	return out, mapErr(err)
}

func (s *Service) ExitEnrollment(ctx context.Context, principalID, businessID, automationID, enrollmentID uuid.UUID) (EnrollmentView, error) {
	var out EnrollmentView
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `UPDATE automation_enrollment SET status='exited',current_node_id=NULL,
			wake_at=NULL,lease_expires_at=NULL,exit_reason='manual',finished_at=now(),updated_at=now()
			WHERE id=$1 AND automation_id=$2 AND business_id=$3 AND tenant_root_id=$4 AND status='active'
			RETURNING id,business_id,tenant_root_id,automation_id,version_id,subscriber_id,status::text,
			current_node_id,wake_at,node_attempts,last_error,exit_reason,source_event_id,enrolled_at,finished_at,updated_at`,
			enrollmentID, automationID, businessID, root)
		out, err = scanEnrollment(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				var found bool
				if findErr := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM automation_enrollment
					WHERE id=$1 AND automation_id=$2 AND business_id=$3 AND tenant_root_id=$4)`, enrollmentID, automationID, businessID, root).Scan(&found); findErr != nil {
					return findErr
				}
				if found {
					return errs.ErrConflict
				}
			}
			return err
		}
		return writeAudit(ctx, tx, principalID, businessID, root, "automation.enrollment.exited", automationID, map[string]any{"enrollment_id": enrollmentID})
	})
	return out, mapErr(err)
}

func (s *Service) Stats(ctx context.Context, principalID, businessID, automationID uuid.UUID, requestedVersion *uuid.UUID) (Stats, error) {
	var out Stats
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		automation, err := q.GetAutomation(ctx, dbgen.GetAutomationParams{ID: automationID, BusinessID: businessID, TenantRootID: root})
		if err != nil {
			return err
		}
		var versionID uuid.UUID
		if requestedVersion != nil {
			versionID = *requestedVersion
		} else if automation.ActiveVersionID.Valid {
			versionID = uuid.UUID(automation.ActiveVersionID.Bytes)
		} else if automation.DraftVersionID.Valid {
			versionID = uuid.UUID(automation.DraftVersionID.Bytes)
		} else {
			return pgx.ErrNoRows
		}
		version, err := q.GetAutomationVersion(ctx, dbgen.GetAutomationVersionParams{ID: versionID, AutomationID: automationID, BusinessID: businessID, TenantRootID: root})
		if err != nil {
			return err
		}
		graph, err := decodeGraph(version.Graph)
		if err != nil {
			return err
		}
		out.AutomationID, out.VersionID = automationID, versionID
		if err = tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status='active'),count(*) FILTER (WHERE status='completed'),
			count(*) FILTER (WHERE status='exited'),count(*) FILTER (WHERE status='errored')
			FROM automation_enrollment WHERE automation_id=$1 AND version_id=$2 AND business_id=$3 AND tenant_root_id=$4`,
			automationID, versionID, businessID, root).Scan(&out.Enrollments.Active, &out.Enrollments.Completed, &out.Enrollments.Exited, &out.Enrollments.Errored); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT s.node_id,s.node_kind,count(*),
			count(*) FILTER (WHERE s.outcome='waiting'),count(*) FILTER (WHERE s.outcome='advanced'),
			count(*) FILTER (WHERE s.outcome='sent'),count(*) FILTER (WHERE d.opened_at IS NOT NULL),
			count(*) FILTER (WHERE EXISTS(SELECT 1 FROM mailing_tracking_event e
				WHERE e.delivery_id=s.delivery_id AND e.business_id=s.business_id
				AND e.tenant_root_id=s.tenant_root_id AND e.kind='click' AND e.url IS NOT NULL)),
			count(*) FILTER (WHERE s.outcome='branch_yes'),count(*) FILTER (WHERE s.outcome='branch_no'),
			count(*) FILTER (WHERE s.outcome='exited'),count(*) FILTER (WHERE s.outcome='error')
			FROM automation_enrollment_step s
			LEFT JOIN mailing_delivery d ON d.id=s.delivery_id AND d.business_id=s.business_id
				AND d.tenant_root_id=s.tenant_root_id AND d.source_kind='automation'
			WHERE s.version_id=$1 AND s.business_id=$2 AND s.tenant_root_id=$3
			GROUP BY s.node_id,s.node_kind`, versionID, businessID, root)
		if err != nil {
			return err
		}
		defer rows.Close()
		byNode := make(map[string]NodeStats)
		for rows.Next() {
			var value NodeStats
			if err = rows.Scan(&value.NodeID, &value.NodeKind, &value.Entered, &value.Waiting, &value.Advanced, &value.Sent, &value.Opened, &value.Clicked, &value.BranchYes, &value.BranchNo, &value.Exited, &value.Errors); err != nil {
				return err
			}
			byNode[value.NodeID] = value
		}
		if err = rows.Err(); err != nil {
			return err
		}
		out.Nodes = make([]NodeStats, 0, len(graph.Nodes))
		for _, node := range graph.Nodes {
			value, ok := byNode[node.ID]
			if !ok {
				value = NodeStats{NodeID: node.ID, NodeKind: node.Kind}
			}
			out.Nodes = append(out.Nodes, value)
		}
		return nil
	})
	return out, mapErr(err)
}

type enrollmentScanner interface{ Scan(...any) error }

func scanEnrollment(row enrollmentScanner) (EnrollmentView, error) {
	var out EnrollmentView
	var current, lastError, exitReason pgtype.Text
	var wake, finished pgtype.Timestamptz
	var source pgtype.UUID
	err := row.Scan(&out.ID, &out.BusinessID, &out.TenantRootID, &out.AutomationID, &out.VersionID, &out.SubscriberID,
		&out.Status, &current, &wake, &out.NodeAttempts, &lastError, &exitReason, &source, &out.EnrolledAt, &finished, &out.UpdatedAt)
	if err != nil {
		return EnrollmentView{}, err
	}
	out.CurrentNodeID, out.WakeAt = textPtr(current), timePtr(wake)
	out.LastError, out.ExitReason = textPtr(lastError), textPtr(exitReason)
	out.SourceEventID, out.FinishedAt = uuidPtr(source), timePtr(finished)
	return out, nil
}

func textPtr(value pgtype.Text) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func validEnrollmentStatus(value string) bool {
	switch value {
	case "active", "completed", "exited", "errored":
		return true
	default:
		return false
	}
}

func triggerNodeAndList(graph Graph) (string, uuid.UUID, error) {
	for _, node := range graph.Nodes {
		if node.Kind != "trigger" {
			continue
		}
		var config struct {
			ListID uuid.UUID `json:"list_id"`
		}
		if err := json.Unmarshal(node.Config, &config); err != nil || config.ListID == uuid.Nil {
			return "", uuid.Nil, validation("active version has an invalid trigger")
		}
		return node.ID, config.ListID, nil
	}
	return "", uuid.Nil, validation("active version has no trigger")
}
