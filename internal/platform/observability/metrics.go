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
