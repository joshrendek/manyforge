package observability

import "expvar"

// Counter keys for the spec-002 pipelines, published under the "support" expvar
// map (so /metrics shows {"support": {...}}). Producers and tests share these.
const (
	MetricIngestReceived  = "ingest.received"
	MetricIngestAccepted  = "ingest.accepted"
	MetricIngestRejected  = "ingest.rejected"
	MetricIngestDuplicate = "ingest.duplicate"

	MetricOutboundSent       = "outbound.sent"
	MetricOutboundFailed     = "outbound.failed"
	MetricOutboundSuppressed = "outbound.suppressed"

	MetricOutboxDrained = "outbox.drained"
	MetricOutboxRetried = "outbox.retried"
	MetricOutboxDropped = "outbox.dropped"

	// manyforge-p20 time-series foundation. A sustained partition.sweep_failed is the alertable
	// one: pre-created partitions give several periods of slack, so ingest keeps working for a
	// while after maintenance breaks — the failure is silent until that slack runs out.
	MetricPartitionCreated     = "partition.created"
	MetricPartitionDropped     = "partition.dropped"
	MetricPartitionSweepFailed = "partition.sweep_failed"

	MetricRollupBucketsWritten = "rollup.buckets_written"
	MetricRollupSweepFailed    = "rollup.sweep_failed"

	MetricTelemetryIngestAccepted = "telemetry.ingest_accepted"
	MetricTelemetryIngestRejected = "telemetry.ingest_rejected"

	// manyforge-as0 analytics collect. The endpoint always answers 204, so these counters are the
	// ONLY way to see that a site's key stopped resolving — a spike in collect_rejected means
	// someone's embed is silently sending into the void.
	MetricAnalyticsCollected       = "analytics.collected"
	MetricAnalyticsCollectRejected = "analytics.collect_rejected"
	MetricAnalyticsCollectFailed   = "analytics.collect_failed"

	// Whole-master tenant merge observations. Counters are request-local
	// telemetry; the append-only tenant_merge_operation_event and immutable
	// audit manifest remain the durable authority across process restarts.
	MetricTenantMergePreflightTotal      = "tenant_merge.preflight_total"
	MetricTenantMergePreflightDurationMS = "tenant_merge.preflight_duration_ms"
	MetricTenantMergeConflicts           = "tenant_merge.conflicts"
	MetricTenantMergeSucceeded           = "tenant_merge.succeeded"
	MetricTenantMergeFailures            = "tenant_merge.failures"
	MetricTenantMergeRollbacks           = "tenant_merge.rollbacks"
	MetricTenantMergeOperationDurationMS = "tenant_merge.operation_duration_ms"
	MetricTenantMergeFenceDurationMS     = "tenant_merge.fence_duration_ms"
	MetricTenantMergeRowsPrefix          = "tenant_merge.rows."
)

// Metrics is a thin, nil-safe wrapper over a published expvar.Map. A nil *Metrics
// makes every method a no-op, so a pipeline with no metrics wired behaves exactly
// as before. expvar serves the underlying map at /metrics with zero new deps.
type Metrics struct{ m *expvar.Map }

const metricsMapName = "support"

// NewMetrics returns a handle to the published "support" map, creating it on first
// call and reusing it thereafter (so repeated calls — e.g. in tests — never trip
// expvar.NewMap's duplicate-registration panic).
func NewMetrics() *Metrics {
	if v := expvar.Get(metricsMapName); v != nil {
		if mp, ok := v.(*expvar.Map); ok {
			return &Metrics{m: mp}
		}
	}
	return &Metrics{m: expvar.NewMap(metricsMapName)}
}

// Inc adds 1 to the named counter. No-op on a nil receiver.
func (m *Metrics) Inc(key string) { m.Add(key, 1) }

// Add adds n to the named counter. No-op on a nil receiver.
func (m *Metrics) Add(key string, n int64) {
	if m == nil || m.m == nil {
		return
	}
	m.m.Add(key, n)
}

// Set publishes a gauge value. No-op on a nil receiver. Gauges are process-local snapshots; use
// Add for counters whose deltas must remain meaningful for the lifetime of the process.
func (m *Metrics) Set(key string, n int64) {
	if m == nil || m.m == nil {
		return
	}
	if existing := m.m.Get(key); existing != nil {
		// Match expvar.Map.Add: a key already owned by a different metric type is left intact.
		// Metric-name collisions must not silently replace a published map, string, or float.
		if v, ok := existing.(*expvar.Int); ok {
			v.Set(n)
		}
		return
	}
	v := new(expvar.Int)
	v.Set(n)
	m.m.Set(key, v)
}

// Get reads the named counter (0 if unset). For tests/inspection.
func (m *Metrics) Get(key string) int64 {
	if m == nil || m.m == nil {
		return 0
	}
	if v, ok := m.m.Get(key).(*expvar.Int); ok && v != nil {
		return v.Value()
	}
	return 0
}
