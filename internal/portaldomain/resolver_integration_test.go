//go:build integration

package portaldomain

import (
	"testing"
	"time"
)

func TestHostResolverAgainstDatabase(t *testing.T) {
	f := newFixture(t)
	hosts := NewHostResolver(f.tdb.App, time.Hour) // long TTL: only Forget may refresh
	f.svc.Forget = hosts.Forget

	d, err := f.svc.Create(f.ctx, f.a.principal, f.a.business, "portal.example.com")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, ok, err := hosts.Lookup(f.ctx, d.Hostname); err != nil || ok {
		t.Fatalf("pending hostname: ok=%v err=%v, want miss", ok, err)
	}

	f.dns.publish(d.Verification.Name, d.Verification.Value)
	if _, err := f.svc.Verify(f.ctx, f.a.principal, f.a.business, d.ID); err != nil {
		t.Fatalf("verify: %v", err)
	}
	got, ok, err := hosts.Lookup(f.ctx, d.Hostname)
	if err != nil || !ok || got != f.a.business {
		t.Fatalf("verified hostname: got=%v ok=%v err=%v", got, ok, err)
	}

	if err := f.svc.Delete(f.ctx, f.a.principal, f.a.business, d.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, err := hosts.Lookup(f.ctx, d.Hostname); err != nil || ok {
		t.Fatalf("deleted hostname: ok=%v err=%v, want miss", ok, err)
	}
}
