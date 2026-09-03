package automations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	DefaultMaxNodesPerTick = 25
	DefaultMaxNodeAttempts = 5
)

type Enrollment struct {
	ID, BusinessID, TenantRootID, AutomationID, VersionID, SubscriberID uuid.UUID
	CurrentNodeID                                                       string
	WakeAt, EnrolledAt                                                  time.Time
	NodeAttempts, ClaimGeneration                                       int
}

type AdvanceOutcome struct {
	NodesProcessed int
	Status         string
	Parked         bool
	LeaseLost      bool
	LastError      string
}

type Engine struct {
	Deps            Deps
	MaxNodesPerTick int
	MaxNodeAttempts int
}

func (e Engine) Advance(ctx context.Context, tx pgx.Tx, enrollment Enrollment, graph Graph, now time.Time) (AdvanceOutcome, error) {
	out := AdvanceOutcome{Status: "active"}
	if e.Deps.Steps == nil {
		return out, errors.New("automation step store is not configured")
	}
	limit := e.MaxNodesPerTick
	if limit <= 0 {
		limit = DefaultMaxNodesPerTick
	}
	nodes := make(map[string]Node, len(graph.Nodes))
	edges := make(map[string][]Edge, len(graph.Nodes))
	for _, node := range graph.Nodes {
		nodes[node.ID] = node
	}
	for _, edge := range graph.Edges {
		edges[edge.From] = append(edges[edge.From], edge)
	}

	current := enrollment.CurrentNodeID
	for out.NodesProcessed < limit {
		node, ok := nodes[current]
		if !ok {
			return e.fail(ctx, tx, enrollment, out, fmt.Errorf("graph_node_missing: %s: %w", current, ErrInvalidReference), now)
		}
		out.NodesProcessed++
		next := nextNode(edges[node.ID], "")
		status := "active"
		if next == "" {
			status = "completed"
		}
		record := StepRecord{
			EnrollmentID: enrollment.ID, ClaimGeneration: enrollment.ClaimGeneration,
			NodeID: node.ID, NodeKind: node.Kind, NextNodeID: stringPointer(next),
			Status: status, Detail: map[string]any{}, RecordedAt: now,
		}

		switch node.Kind {
		case "trigger":
			record.Outcome = "advanced"
		case "send_email":
			snapshot, err := e.snapshot(ctx, tx, enrollment.SubscriberID)
			if err != nil {
				return e.fail(ctx, tx, enrollment, out, err, now)
			}
			if snapshot.Status != "active" {
				record.Outcome, record.Status, record.NextNodeID = "exited", "exited", nil
				record.Detail["reason"] = snapshot.Status
				return e.recordAndStop(ctx, tx, record, out)
			}
			var cfg struct {
				TemplateID  uuid.UUID `json:"template_id"`
				TrackOpens  *bool     `json:"track_opens"`
				TrackClicks *bool     `json:"track_clicks"`
			}
			if err := decodeConfig(node.Config, &cfg); err != nil || cfg.TemplateID == uuid.Nil || cfg.TrackOpens == nil || cfg.TrackClicks == nil {
				return e.fail(ctx, tx, enrollment, out, fmt.Errorf("invalid send_email config: %w", ErrInvalidReference), now)
			}
			if e.Deps.Sender == nil {
				return e.fail(ctx, tx, enrollment, out, errors.New("message sender is not configured"), now)
			}
			deliveryID, err := e.Deps.Sender.Enqueue(ctx, tx, MessageSpec{
				BusinessID: enrollment.BusinessID, TenantRootID: enrollment.TenantRootID,
				SubscriberID: enrollment.SubscriberID, TemplateID: cfg.TemplateID,
				TrackOpens: *cfg.TrackOpens, TrackClicks: *cfg.TrackClicks,
				SourceKind: "automation", SourceID: stepSourceID(enrollment.ID, node.ID), NotBefore: now,
			})
			if err != nil {
				return e.fail(ctx, tx, enrollment, out, err, now)
			}
			record.Outcome, record.DeliveryID = "sent", &deliveryID
		case "wait":
			waiting, err := e.Deps.Steps.Waiting(ctx, tx, enrollment.ID, node.ID)
			if err != nil {
				return e.fail(ctx, tx, enrollment, out, err, now)
			}
			if !waiting {
				wakeAt, err := waitUntil(node.Config, now)
				if err != nil {
					return e.fail(ctx, tx, enrollment, out, fmt.Errorf("invalid wait config: %v: %w", err, ErrInvalidReference), now)
				}
				record.Outcome, record.Status, record.NextNodeID, record.WakeAt = "waiting", "active", nil, &wakeAt
				record.Detail["wake_at"] = wakeAt
				result, err := e.recordAndStop(ctx, tx, record, out)
				result.Parked = err == nil && !result.LeaseLost
				return result, err
			}
			record.Outcome = "advanced"
		case "condition":
			yes, err := e.evaluateCondition(ctx, tx, enrollment, node.Config)
			if err != nil {
				return e.fail(ctx, tx, enrollment, out, err, now)
			}
			branch := "no"
			record.Outcome = "branch_no"
			if yes {
				branch, record.Outcome = "yes", "branch_yes"
			}
			next = nextNode(edges[node.ID], branch)
			record.NextNodeID = stringPointer(next)
			if next == "" {
				record.Status = "completed"
			} else {
				record.Status = "active"
			}
		case "add_tag", "remove_tag":
			var cfg struct {
				Tag string `json:"tag"`
			}
			if err := decodeConfig(node.Config, &cfg); err != nil || strings.TrimSpace(cfg.Tag) == "" {
				return e.fail(ctx, tx, enrollment, out, fmt.Errorf("invalid %s config: %w", node.Kind, ErrInvalidReference), now)
			}
			if e.Deps.Tagger == nil {
				return e.fail(ctx, tx, enrollment, out, errors.New("tagger is not configured"), now)
			}
			if node.Kind == "add_tag" {
				snapshot, err := e.snapshot(ctx, tx, enrollment.SubscriberID)
				if err != nil {
					return e.fail(ctx, tx, enrollment, out, err, now)
				}
				if snapshot.Status != "active" {
					record.Outcome, record.Status, record.NextNodeID = "exited", "exited", nil
					record.Detail["reason"] = snapshot.Status
					return e.recordAndStop(ctx, tx, record, out)
				}
				if err := e.Deps.Tagger.AddTag(ctx, tx, enrollment.BusinessID, enrollment.TenantRootID, enrollment.SubscriberID, cfg.Tag); err != nil {
					return e.fail(ctx, tx, enrollment, out, err, now)
				}
			} else if err := e.Deps.Tagger.RemoveTag(ctx, tx, enrollment.BusinessID, enrollment.TenantRootID, enrollment.SubscriberID, cfg.Tag); err != nil {
				return e.fail(ctx, tx, enrollment, out, err, now)
			}
			record.Outcome = "advanced"
		case "exit":
			record.Outcome, record.Status, record.NextNodeID = "exited", "completed", nil
		default:
			return e.fail(ctx, tx, enrollment, out, fmt.Errorf("unknown node kind %q: %w", node.Kind, ErrInvalidReference), now)
		}

		changed, err := e.Deps.Steps.Record(ctx, tx, record)
		if err != nil {
			return out, err
		}
		if !changed {
			out.LeaseLost = true
			return out, nil
		}
		if record.Status != "active" {
			out.Status = record.Status
			return out, nil
		}
		// automation_record_step resets the persisted counter for the next
		// node. Mirror that transition locally so a later node in this same
		// tick does not inherit retries from the node that just recovered.
		enrollment.NodeAttempts = 0
		current = next
	}
	return out, nil
}

func (e Engine) recordAndStop(ctx context.Context, tx pgx.Tx, record StepRecord, out AdvanceOutcome) (AdvanceOutcome, error) {
	changed, err := e.Deps.Steps.Record(ctx, tx, record)
	if err != nil {
		return out, err
	}
	if !changed {
		out.LeaseLost = true
		return out, nil
	}
	out.Status = record.Status
	return out, nil
}

func (e Engine) fail(ctx context.Context, tx pgx.Tx, enrollment Enrollment, out AdvanceOutcome, cause error, now time.Time) (AdvanceOutcome, error) {
	maxAttempts := e.MaxNodeAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxNodeAttempts
	}
	terminal := errors.Is(cause, ErrInvalidReference) || enrollment.NodeAttempts+1 >= maxAttempts
	retryAt := now.Add(retryDelay(enrollment.NodeAttempts))
	changed, err := e.Deps.Steps.Fail(ctx, tx, StepFailure{
		EnrollmentID: enrollment.ID, ClaimGeneration: enrollment.ClaimGeneration,
		Error: cause.Error(), Terminal: terminal, RetryAt: retryAt,
	})
	if err != nil {
		return out, err
	}
	if !changed {
		out.LeaseLost = true
		return out, nil
	}
	out.LastError = cause.Error()
	if terminal {
		out.Status = "errored"
	}
	return out, nil
}

func (e Engine) snapshot(ctx context.Context, tx pgx.Tx, subscriberID uuid.UUID) (SubscriberSnapshot, error) {
	if e.Deps.Subscribers == nil {
		return SubscriberSnapshot{}, errors.New("subscriber reader is not configured")
	}
	return e.Deps.Subscribers.Snapshot(ctx, tx, subscriberID)
}

func (e Engine) evaluateCondition(ctx context.Context, tx pgx.Tx, enrollment Enrollment, raw json.RawMessage) (bool, error) {
	var cfg struct {
		Predicate json.RawMessage `json:"predicate"`
	}
	if err := decodeConfig(raw, &cfg); err != nil {
		return false, fmt.Errorf("invalid condition config: %w", ErrInvalidReference)
	}
	var kind struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(cfg.Predicate, &kind); err != nil {
		return false, fmt.Errorf("invalid condition predicate: %w", ErrInvalidReference)
	}
	switch kind.Type {
	case "opened_email", "clicked_link":
		var predicate struct {
			Type   string  `json:"type"`
			NodeID string  `json:"node_id"`
			URL    *string `json:"url,omitempty"`
		}
		if err := decodeConfig(cfg.Predicate, &predicate); err != nil {
			return false, fmt.Errorf("invalid engagement predicate: %w", ErrInvalidReference)
		}
		deliveryID, err := e.Deps.Steps.Delivery(ctx, tx, enrollment.ID, predicate.NodeID)
		if err != nil || deliveryID == nil {
			return false, err
		}
		if e.Deps.Engagement == nil {
			return false, errors.New("engagement reader is not configured")
		}
		engagement, err := e.Deps.Engagement.Engagement(ctx, tx, *deliveryID)
		if err != nil {
			return false, err
		}
		if kind.Type == "opened_email" {
			return engagement.Opened, nil
		}
		if predicate.URL == nil {
			return len(engagement.ClickedURLs) > 0, nil
		}
		for _, clicked := range engagement.ClickedURLs {
			if clicked == *predicate.URL {
				return true, nil
			}
		}
		return false, nil
	case "has_tag":
		var predicate struct{ Type, Tag string }
		if err := decodeConfig(cfg.Predicate, &predicate); err != nil {
			return false, fmt.Errorf("invalid tag predicate: %w", ErrInvalidReference)
		}
		snapshot, err := e.snapshot(ctx, tx, enrollment.SubscriberID)
		if err != nil {
			return false, err
		}
		for _, tag := range snapshot.Tags {
			if strings.EqualFold(tag, predicate.Tag) {
				return true, nil
			}
		}
		return false, nil
	case "on_list":
		var predicate struct {
			Type   string    `json:"type"`
			ListID uuid.UUID `json:"list_id"`
		}
		if err := decodeConfig(cfg.Predicate, &predicate); err != nil || predicate.ListID == uuid.Nil {
			return false, fmt.Errorf("invalid list predicate: %w", ErrInvalidReference)
		}
		snapshot, err := e.snapshot(ctx, tx, enrollment.SubscriberID)
		if err != nil {
			return false, err
		}
		return e.Deps.Subscribers.ActiveOnList(ctx, tx, enrollment.BusinessID, snapshot.Email, predicate.ListID)
	case "event_received":
		var predicate struct {
			Type          string `json:"type"`
			Name          string `json:"name"`
			WithinSeconds *int64 `json:"within_seconds"`
		}
		if err := decodeConfig(cfg.Predicate, &predicate); err != nil {
			return false, fmt.Errorf("invalid event predicate: %w", ErrInvalidReference)
		}
		snapshot, err := e.snapshot(ctx, tx, enrollment.SubscriberID)
		if err != nil {
			return false, err
		}
		var within *time.Duration
		if predicate.WithinSeconds != nil {
			d := time.Duration(*predicate.WithinSeconds) * time.Second
			within = &d
		}
		return e.Deps.Steps.EventExists(ctx, tx, enrollment.BusinessID, snapshot.Email, predicate.Name, enrollment.EnrolledAt, within)
	default:
		return false, fmt.Errorf("unknown condition predicate %q: %w", kind.Type, ErrInvalidReference)
	}
}

func nextNode(edges []Edge, branch string) string {
	for _, edge := range edges {
		if branch == "" && edge.Branch == nil {
			return edge.To
		}
		if edge.Branch != nil && *edge.Branch == branch {
			return edge.To
		}
	}
	return ""
}

func stringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func stepSourceID(enrollmentID uuid.UUID, nodeID string) uuid.UUID {
	return uuid.NewSHA1(enrollmentID, []byte(nodeID))
}

func retryDelay(priorAttempts int) time.Duration {
	if priorAttempts < 0 {
		priorAttempts = 0
	}
	if priorAttempts > 7 {
		priorAttempts = 7
	}
	delay := 30 * time.Second * time.Duration(1<<priorAttempts)
	if delay > time.Hour {
		return time.Hour
	}
	return delay
}

func waitUntil(raw json.RawMessage, now time.Time) (time.Time, error) {
	var cfg struct {
		Mode     string `json:"mode"`
		Seconds  *int64 `json:"seconds,omitempty"`
		Weekday  *int   `json:"weekday,omitempty"`
		Time     string `json:"time,omitempty"`
		Timezone string `json:"timezone,omitempty"`
	}
	if err := decodeConfig(raw, &cfg); err != nil {
		return time.Time{}, err
	}
	if cfg.Mode == "duration" {
		if cfg.Seconds == nil || *cfg.Seconds < 60 || *cfg.Seconds > 31536000 || cfg.Weekday != nil || cfg.Time != "" || cfg.Timezone != "" {
			return time.Time{}, errors.New("invalid duration wait")
		}
		return now.Add(time.Duration(*cfg.Seconds) * time.Second), nil
	}
	if cfg.Mode != "until" || cfg.Seconds != nil || (cfg.Weekday != nil && (*cfg.Weekday < 1 || *cfg.Weekday > 7)) {
		return time.Time{}, errors.New("unknown wait mode")
	}
	parts := strings.Split(cfg.Time, ":")
	if len(parts) != 2 {
		return time.Time{}, errors.New("invalid clock time")
	}
	hour, err := strconv.Atoi(parts[0])
	if err != nil {
		return time.Time{}, err
	}
	minute, err := strconv.Atoi(parts[1])
	if err != nil {
		return time.Time{}, err
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return time.Time{}, errors.New("invalid clock time")
	}
	location, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		return time.Time{}, err
	}
	local := now.In(location)
	target := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, location)
	if cfg.Weekday == nil {
		if !target.After(local) {
			target = target.AddDate(0, 0, 1)
		}
		return target, nil
	}
	desired := time.Weekday(*cfg.Weekday % 7) // ISO 1=Monday ... 7=Sunday.
	days := (int(desired) - int(local.Weekday()) + 7) % 7
	target = target.AddDate(0, 0, days)
	if !target.After(local) {
		target = target.AddDate(0, 0, 7)
	}
	return target, nil
}
