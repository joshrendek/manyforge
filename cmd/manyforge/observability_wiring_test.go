//go:build contract

package main

import (
	"os"
	"strings"
	"testing"
)

// T072 — pins that each pipeline is instrumented: its source references the
// observability metric constants. A future edit that strips instrumentation
// (re-blinding a pipeline) fails CI.
func TestPipelinesInstrumented(t *testing.T) {
	cases := map[string][]string{
		"../../internal/inbox/handler.go":                     {"MetricIngestReceived", "MetricIngestAccepted", "MetricIngestRejected"},
		"../../internal/platform/notify/sender_subscriber.go": {"MetricOutboundSent", "MetricOutboundFailed", "MetricOutboundSuppressed"},
		"../../internal/platform/events/outbox.go":            {"MetricOutboxDrained", "MetricOutboxRetried", "MetricOutboxDropped"},
	}
	for file, consts := range cases {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		src := string(b)
		for _, c := range consts {
			if !strings.Contains(src, c) {
				t.Errorf("%s does not reference observability.%s — pipeline not instrumented", file, c)
			}
		}
	}
}
