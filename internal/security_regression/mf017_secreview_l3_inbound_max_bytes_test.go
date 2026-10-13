// Finding: SECREVIEW-L3 — a non-positive MANYFORGE_INBOUND_MAX_BYTES disabled the
// Resend raw-message download cap (maxBytes <= 0 meant "no limit").
//
// Fix: config.Load rejects non-positive values, and the receiving client treats a
// non-positive cap as the hard DefaultMaxReceivedBytes ceiling.
package security_regression

import (
	"strings"
	"testing"

	"github.com/manyforge/manyforge/internal/platform/config"
)

func TestSecreviewL3_ConfigRejectsZeroInboundMaxBytes(t *testing.T) {
	t.Setenv("MANYFORGE_INBOUND_MAX_BYTES", "0")
	if _, err := config.Load(); err == nil {
		t.Fatal("config.Load accepted MANYFORGE_INBOUND_MAX_BYTES=0")
	}
}

func TestPin_SecreviewL3RawDownloadCapNeverOptional(t *testing.T) {
	src := mustRead(t, "../mailing/provider/resend_receiving.go")
	body := mf017Slice(t, src, "func (r *Resend) DownloadReceivedRaw(", "func (r *Resend) EnsureReceivingEvents(")
	if !strings.Contains(body, "maxBytes = receivedBytesCap(maxBytes)") {
		t.Error("DownloadReceivedRaw must normalize a non-positive cap via receivedBytesCap")
	}
	for _, bad := range []string{"maxBytes > 0 &&", "if maxBytes > 0 {"} {
		if strings.Contains(body, bad) {
			t.Errorf("DownloadReceivedRaw makes the size cap optional again (%q)", bad)
		}
	}
	get := mf017Slice(t, src, "func (r *Resend) GetReceivedEmail(", "func (r *Resend) DownloadReceivedRaw(")
	if !strings.Contains(get, "receivedBytesCap(maxRawBytes)") {
		t.Error("GetReceivedEmail must bound its response with receivedBytesCap")
	}
	helper := mf017Slice(t, src, "func receivedBytesCap(", "\n}\n")
	if !strings.Contains(helper, "return DefaultMaxReceivedBytes") {
		t.Error("receivedBytesCap must fall back to DefaultMaxReceivedBytes")
	}
}
