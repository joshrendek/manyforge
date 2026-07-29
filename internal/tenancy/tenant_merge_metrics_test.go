package tenancy

import (
	"testing"
	"time"

	"github.com/manyforge/manyforge/internal/platform/observability"
)

func TestTenantMergeTerminalMetricsAreEmittedOnceForNewTerminalEvent(t *testing.T) {
	metrics := observability.NewMetrics()
	service := &Service{Metrics: metrics}
	createdAt := time.Now().Add(-5 * time.Second)
	fenceStartedAt := createdAt.Add(2 * time.Second)
	completedAt := createdAt.Add(4 * time.Second)
	fenceReleasedAt := completedAt.Add(250 * time.Millisecond)
	operation := TenantMergeOperation{
		CreatedAt: createdAt,
		ModuleCounts: map[string]TenantMergeCount{
			"tenancy": {Rows: 3, Bytes: 200},
			"support": {Rows: 7, Bytes: 500},
		},
		Events: []TenantMergeEvent{
			{Event: "operation.created", CreatedAt: createdAt},
			{Event: "fence.started", CreatedAt: fenceStartedAt},
			{Event: "cutover.started", CreatedAt: fenceStartedAt},
			{Event: "cutover.succeeded", CreatedAt: completedAt},
			{Event: "fence.released", CreatedAt: fenceReleasedAt},
		},
	}

	beforeSucceeded := metrics.Get(observability.MetricTenantMergeSucceeded)
	beforeRowsTenancy := metrics.Get(
		observability.MetricTenantMergeRowsPrefix + "tenancy",
	)
	beforeRowsSupport := metrics.Get(
		observability.MetricTenantMergeRowsPrefix + "support",
	)
	beforeOperationDuration := metrics.Get(
		observability.MetricTenantMergeOperationDurationMS,
	)
	beforeFenceDuration := metrics.Get(
		observability.MetricTenantMergeFenceDurationMS,
	)

	service.recordTenantMergeTerminalMetrics(3, operation)

	if got := metrics.Get(observability.MetricTenantMergeSucceeded) -
		beforeSucceeded; got != 1 {
		t.Errorf("succeeded delta = %d, want 1", got)
	}
	if got := metrics.Get(
		observability.MetricTenantMergeRowsPrefix+"tenancy",
	) - beforeRowsTenancy; got != 3 {
		t.Errorf("tenancy row delta = %d, want 3", got)
	}
	if got := metrics.Get(
		observability.MetricTenantMergeRowsPrefix+"support",
	) - beforeRowsSupport; got != 7 {
		t.Errorf("support row delta = %d, want 7", got)
	}
	if got := metrics.Get(observability.MetricTenantMergeOperationDurationMS) -
		beforeOperationDuration; got != 4000 {
		t.Errorf("operation duration delta = %dms, want 4000ms", got)
	}
	if got := metrics.Get(observability.MetricTenantMergeFenceDurationMS) -
		beforeFenceDuration; got != 2250 {
		t.Errorf("fence duration delta = %dms, want 2250ms", got)
	}

	// A replay has no new terminal event after the caller's event cursor.
	service.recordTenantMergeTerminalMetrics(len(operation.Events), operation)
	if got := metrics.Get(observability.MetricTenantMergeSucceeded) -
		beforeSucceeded; got != 1 {
		t.Errorf("replay changed succeeded delta to %d, want 1", got)
	}
}

func TestTenantMergeFailureMetricsRecordRollback(t *testing.T) {
	metrics := observability.NewMetrics()
	service := &Service{Metrics: metrics}
	now := time.Now()
	operation := TenantMergeOperation{
		CreatedAt: now.Add(-time.Second),
		Events: []TenantMergeEvent{
			{Event: "operation.created", CreatedAt: now.Add(-time.Second)},
			{Event: "cutover.failed", CreatedAt: now},
		},
	}
	beforeFailures := metrics.Get(observability.MetricTenantMergeFailures)
	beforeRollbacks := metrics.Get(observability.MetricTenantMergeRollbacks)

	service.recordTenantMergeTerminalMetrics(1, operation)

	if got := metrics.Get(observability.MetricTenantMergeFailures) -
		beforeFailures; got != 1 {
		t.Errorf("failure delta = %d, want 1", got)
	}
	if got := metrics.Get(observability.MetricTenantMergeRollbacks) -
		beforeRollbacks; got != 1 {
		t.Errorf("rollback delta = %d, want 1", got)
	}
}
