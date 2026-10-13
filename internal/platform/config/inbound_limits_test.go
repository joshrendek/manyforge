package config

import (
	"strings"
	"testing"
)

// SECREVIEW-L3: a non-positive MANYFORGE_INBOUND_MAX_BYTES would disable the raw
// download cap, so Load refuses it with a clear error.
func TestLoadRejectsNonPositiveInboundMaxBytes(t *testing.T) {
	for _, v := range []string{"0", "-1", "-31457280"} {
		t.Setenv("MANYFORGE_INBOUND_MAX_BYTES", v)
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "MANYFORGE_INBOUND_MAX_BYTES") || !strings.Contains(err.Error(), "positive") {
			t.Fatalf("MANYFORGE_INBOUND_MAX_BYTES=%s: Load error = %v, want a positive-value error", v, err)
		}
	}
	t.Setenv("MANYFORGE_INBOUND_MAX_BYTES", "1048576")
	cfg, err := Load()
	if err != nil || cfg.InboundMaxBytes != 1<<20 {
		t.Fatalf("Load = %d, %v; want 1048576", cfg.InboundMaxBytes, err)
	}
}

func TestLoadInboundMaxBytesDefault(t *testing.T) {
	t.Setenv("MANYFORGE_INBOUND_MAX_BYTES", "")
	cfg, err := Load()
	if err != nil || cfg.InboundMaxBytes != 30<<20 {
		t.Fatalf("Load = %d, %v; want the 30 MiB default", cfg.InboundMaxBytes, err)
	}
}
