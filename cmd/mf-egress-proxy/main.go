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
	h := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "only CONNECT supported", http.StatusMethodNotAllowed)
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
	}
	srv := &http.Server{Addr: addr, Handler: http.HandlerFunc(h)}
	log.Printf("mf-egress-proxy listening on %s allow=%v", addr, allow)
	log.Fatal(srv.ListenAndServe())
}
