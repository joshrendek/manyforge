package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

const (
	NetworkName  = "mf-sandbox-net"
	ProxyName    = "mf-egress-proxy"
	ProxyDNSAddr = "http://mf-egress-proxy:8080"
)

// EnsureEgressInfra creates an --internal docker network (no external route) and a
// long-lived egress-proxy container attached to BOTH that network and the default
// bridge, allowlisting `allow`. Idempotent.
func EnsureEgressInfra(ctx context.Context, proxyImage string, allow []string) error {
	// internal network: containers on it have no external connectivity except via the proxy.
	_ = exec.CommandContext(ctx, "docker", "network", "create", "--internal", NetworkName).Run()

	// already running?
	out, _ := exec.CommandContext(ctx, "docker", "ps", "-q", "-f", "name=^/"+ProxyName+"$").Output()
	if strings.TrimSpace(string(out)) != "" {
		return nil
	}
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", ProxyName).Run()
	// start proxy on the default bridge (external route), then attach the internal network.
	run := exec.CommandContext(ctx, "docker", "run", "-d", "--name", ProxyName,
		"-e", "EGRESS_ALLOW="+strings.Join(allow, ","), proxyImage)
	if b, err := run.CombinedOutput(); err != nil {
		return fmt.Errorf("sandbox: start egress proxy: %w (%s)", err, string(b))
	}
	if b, err := exec.CommandContext(ctx, "docker", "network", "connect", NetworkName, ProxyName).CombinedOutput(); err != nil {
		return fmt.Errorf("sandbox: attach proxy to internal net: %w (%s)", err, string(b))
	}
	return nil
}
