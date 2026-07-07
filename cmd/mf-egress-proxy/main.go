package main

import (
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/manyforge/manyforge/internal/platform/netsafe"
)

func main() {
	// The allowlist matcher is shared with the code-review service's pre-flight
	// validation (internal/agents/coding) so the enforcer and validator can't drift.
	allow := netsafe.ParseHostAllowlist(os.Getenv("EGRESS_ALLOW"))
	addr := os.Getenv("LISTEN")
	if addr == "" {
		addr = ":8080"
	}
	srv := &http.Server{Addr: addr, Handler: proxyHandler(allow)}
	log.Printf("mf-egress-proxy listening on %s allow=%v", addr, allow)
	log.Fatal(srv.ListenAndServe())
}

// proxyHandler builds the egress handler: CONNECT tunnels (HTTPS) and plain-HTTP
// forwarding, both gated by the same host allowlist. Local providers use plain HTTP,
// so a non-CONNECT request to an allowlisted host is round-tripped upstream.
func proxyHandler(allow netsafe.HostAllowlist) http.Handler {
	fwd := &http.Transport{Proxy: nil} // no upstream proxy; dial the target directly
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			if !allow.Allows(r.Host) {
				http.Error(w, "egress not allowed", http.StatusForbidden)
				return
			}
			out := r.Clone(r.Context())
			out.RequestURI = "" // required for a client (outbound) request
			resp, err := fwd.RoundTrip(out)
			if err != nil {
				http.Error(w, "upstream error", http.StatusBadGateway)
				return
			}
			defer func() { _ = resp.Body.Close() }()
			for k, vs := range resp.Header {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(flushWriter{w}, resp.Body) // stream (SSE) as chunks arrive
			return
		}
		if !allow.Allows(r.Host) {
			http.Error(w, "egress not allowed", http.StatusForbidden)
			return
		}
		dst, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
		if err != nil {
			http.Error(w, "dial failed", http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
		hj, ok := w.(http.Hijacker)
		if !ok {
			_ = dst.Close()
			return
		}
		src, _, err := hj.Hijack()
		if err != nil {
			_ = dst.Close()
			return
		}
		go func() { _, _ = io.Copy(dst, src); _ = dst.Close() }()
		go func() { _, _ = io.Copy(src, dst); _ = src.Close() }()
	})
}

// flushWriter flushes after each write so SSE chunks reach the sandbox promptly.
type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}
