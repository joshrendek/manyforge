package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// SECREVIEW-L3: a non-positive cap must mean the hard default ceiling, never
// "unlimited". A body just over DefaultMaxReceivedBytes is refused, with and
// without a truthful Content-Length.
func TestResendDownloadReceivedRawNonPositiveCapUsesDefaultCeiling(t *testing.T) {
	var chunked atomic.Bool
	big := strings.Repeat("a", int(DefaultMaxReceivedBytes)+1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if chunked.Load() {
			_, _ = io.WriteString(w, big[:10])
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, big[10:])
			return
		}
		_, _ = io.WriteString(w, big)
	}))
	defer server.Close()
	r := &Resend{APIKey: "re_test", Client: server.Client(), AllowInsecureRawURL: true}
	for _, mode := range []bool{false, true} {
		chunked.Store(mode)
		for _, maxBytes := range []int64{0, -1} {
			if _, err := r.DownloadReceivedRaw(context.Background(), server.URL+"/raw", maxBytes); !errors.Is(err, ErrReceivedEmailTooLarge) {
				t.Fatalf("chunked=%v maxBytes=%d: error = %v, want ErrReceivedEmailTooLarge", mode, maxBytes, err)
			}
		}
	}
}

func TestReceivedBytesCap(t *testing.T) {
	for in, want := range map[int64]int64{0: DefaultMaxReceivedBytes, -5: DefaultMaxReceivedBytes, 1: 1, 1 << 20: 1 << 20} {
		if got := receivedBytesCap(in); got != want {
			t.Errorf("receivedBytesCap(%d) = %d, want %d", in, got, want)
		}
	}
}
