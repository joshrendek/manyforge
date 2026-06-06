package observability

import "testing"

func TestMetricsCounters(t *testing.T) {
	m := NewMetrics()

	m.Inc(MetricIngestReceived)
	m.Inc(MetricIngestReceived)
	m.Add(MetricOutboxDrained, 5)

	if got := m.Get(MetricIngestReceived); got != 2 {
		t.Errorf("%s = %d, want 2", MetricIngestReceived, got)
	}
	if got := m.Get(MetricOutboxDrained); got != 5 {
		t.Errorf("%s = %d, want 5", MetricOutboxDrained, got)
	}
	if got := m.Get("never.touched"); got != 0 {
		t.Errorf("unset counter = %d, want 0", got)
	}
}

func TestMetricsNilSafe(t *testing.T) {
	var m *Metrics               // nil
	m.Inc(MetricOutboundSent)    // must not panic
	m.Add(MetricOutboundSent, 3) // must not panic
	if got := m.Get(MetricOutboundSent); got != 0 {
		t.Errorf("nil Get = %d, want 0", got)
	}
}

func TestNewMetricsTwiceShares(t *testing.T) {
	a := NewMetrics()
	b := NewMetrics() // must not panic (expvar.NewMap would); shares the map
	a.Add("shared.key", 7)
	if got := b.Get("shared.key"); got != 7 {
		t.Errorf("second handle sees %d, want 7 (shared map)", got)
	}
}
