package main

import (
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func allowed(set map[string]bool, hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return set[host] || set[hostport]
}

func main() {
	allow := map[string]bool{}
	for h := range strings.SplitSeq(os.Getenv("EGRESS_ALLOW"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			allow[h] = true
		}
	}
	addr := os.Getenv("LISTEN")
	if addr == "" {
		addr = ":8080"
	}
	h := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "only CONNECT supported", http.StatusMethodNotAllowed)
			return
		}
		if !allowed(allow, r.Host) {
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
