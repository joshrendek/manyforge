package netsafe

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestBlocked(t *testing.T) {
	cases := []struct {
		ip      string
		blocked bool
	}{
		{"127.0.0.1", true},       // loopback
		{"::1", true},             // loopback v6
		{"10.0.0.1", true},        // private
		{"192.168.1.1", true},     // private
		{"172.16.0.1", true},      // private
		{"169.254.169.254", true}, // cloud metadata
		{"169.254.1.1", true},     // link-local
		{"0.0.0.0", true},         // unspecified
		{"fd00:ec2::254", true},   // metadata v6
		{"8.8.8.8", false},        // public
		{"1.1.1.1", false},        // public
	}
	for _, c := range cases {
		if got := Blocked(net.ParseIP(c.ip)); got != c.blocked {
			t.Errorf("Blocked(%s) = %v, want %v", c.ip, got, c.blocked)
		}
	}
	if !Blocked(nil) {
		t.Error("Blocked(nil) should be true (fail closed)")
	}
}

func TestVettedDialAddrs(t *testing.T) {
	// Empty resolution (the soft LookupIPAddr guarantee) must error, not dial nowhere.
	if _, err := vettedDialAddrs(nil, "example.com", "443", Options{}); err == nil {
		t.Error("empty ips slice must return an error, not be dialed")
	}
	// A blocked IP among the resolved set is refused.
	if _, err := vettedDialAddrs([]net.IPAddr{{IP: net.ParseIP("10.0.0.1")}}, "h", "80", Options{}); err == nil {
		t.Error("private IP must be refused under the locked-down posture")
	}
	// A public IP yields the literal host:port to dial.
	got, err := vettedDialAddrs([]net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, "h", "443", Options{})
	if err != nil {
		t.Fatalf("public IP: unexpected err %v", err)
	}
	if len(got) != 1 || got[0] != "8.8.8.8:443" {
		t.Errorf("got %v, want [8.8.8.8:443]", got)
	}
	// Trust flags flow through: a private IP is permitted when AllowPrivate is set.
	if _, err := vettedDialAddrs([]net.IPAddr{{IP: net.ParseIP("10.0.0.1")}}, "h", "80", Options{AllowPrivate: true}); err != nil {
		t.Errorf("private IP under AllowPrivate must be allowed, got %v", err)
	}
}

// A multi-homed host (a CDN like router.huggingface.co has four A records) must yield EVERY
// vetted address so the dialer can fall back past a blipping edge. Returning only the first
// makes one transient failure fail the whole request (manyforge-bhx).
func TestVettedDialAddrs_ReturnsAllVettedAddressesInOrder(t *testing.T) {
	ips := []net.IPAddr{
		{IP: net.ParseIP("13.249.96.32")},
		{IP: net.ParseIP("13.249.96.50")},
		{IP: net.ParseIP("13.249.96.11")},
	}
	got, err := vettedDialAddrs(ips, "cdn.example", "443", Options{})
	if err != nil {
		t.Fatalf("unexpected err %v", err)
	}
	want := []string{"13.249.96.32:443", "13.249.96.50:443", "13.249.96.11:443"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("addr[%d] = %q, want %q (resolution order must be preserved)", i, got[i], want[i])
		}
	}
}

// The fallback must NEVER widen what we are willing to dial: one blocked address among the
// resolved set still fails the whole host closed, even when other addresses are public. That
// is the DNS-rebinding defense, and it must survive the multi-address change.
func TestVettedDialAddrs_OneBlockedAddressFailsTheWholeHost(t *testing.T) {
	mixed := []net.IPAddr{
		{IP: net.ParseIP("8.8.8.8")},         // public
		{IP: net.ParseIP("169.254.169.254")}, // cloud metadata
		{IP: net.ParseIP("1.1.1.1")},         // public
	}
	if _, err := vettedDialAddrs(mixed, "rebind.example", "443", Options{}); err == nil {
		t.Fatal("a host resolving to any blocked address must fail closed, even with public addresses present")
	}
	// Even under full trust, metadata/link-local never unblock.
	if _, err := vettedDialAddrs(mixed, "rebind.example", "443", Options{AllowLoopback: true, AllowPrivate: true}); err == nil {
		t.Fatal("cloud-metadata address must stay blocked under full trust")
	}
}

// withStubResolver swaps the package resolver for one test, so the real ScreenedDialContext
// (not a reimplementation of it) can be driven against a chosen address list.
func withStubResolver(t *testing.T, ips []net.IPAddr) {
	t.Helper()
	orig := lookupIPAddr
	lookupIPAddr = func(context.Context, string) ([]net.IPAddr, error) { return ips, nil }
	t.Cleanup(func() { lookupIPAddr = orig })
}

// ScreenedDialContext must actually try the later addresses. Bind a real listener, then put an
// unreachable address FIRST: a dialer that only ever dials ips[0] fails this test.
func TestScreenedDialContext_FallsBackPastAnUnreachableAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	// The listener is bound to IPv4 loopback only, so [::1]:port REFUSES instantly rather than
	// hanging — the fallback is exercised without a 10s dial timeout in `make test`. (127.0.0.2
	// is not usable here: macOS silently drops rather than refusing, costing the full timeout.)
	withStubResolver(t, []net.IPAddr{
		{IP: net.ParseIP("::1")},
		{IP: net.ParseIP("127.0.0.1")},
	})

	dial := ScreenedDialContext(Options{AllowLoopback: true, AllowPrivate: true})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := dial(ctx, "tcp", net.JoinHostPort("h", port))
	if err != nil {
		t.Fatalf("fallback failed: %v — 127.0.0.1 was listening, but only the first address was dialed", err)
	}
	_ = conn.Close()
}

// Fallback must not paper over a policy refusal: a blocked address anywhere in the set means
// nothing is dialed, even though a reachable public address follows it.
func TestScreenedDialContext_BlockedAddressIsNotDialedAround(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	withStubResolver(t, []net.IPAddr{
		{IP: net.ParseIP("169.254.169.254")}, // cloud metadata
		{IP: net.ParseIP("127.0.0.1")},       // reachable, and allowed by the flags below
	})

	dial := ScreenedDialContext(Options{AllowLoopback: true, AllowPrivate: true})
	if _, err := dial(context.Background(), "tcp", net.JoinHostPort("h", port)); err == nil {
		t.Fatal("a resolved metadata address must fail the host closed, not be skipped in favor of the next IP")
	}
}

func TestBlockedWithLoopbackAllowed(t *testing.T) {
	// loopback permitted; everything else still blocked.
	if blockedWith(net.ParseIP("127.0.0.1"), true, false) {
		t.Error("127.0.0.1 should be allowed when allowLoopback=true")
	}
	if blockedWith(net.ParseIP("::1"), true, false) {
		t.Error("::1 should be allowed when allowLoopback=true")
	}
	for _, bad := range []string{"10.0.0.1", "169.254.169.254", "192.168.1.1", "172.16.0.1"} {
		if !blockedWith(net.ParseIP(bad), true, false) {
			t.Errorf("%s must stay blocked even with allowLoopback=true", bad)
		}
	}
	// default (allowLoopback=false) still blocks loopback.
	if !blockedWith(net.ParseIP("127.0.0.1"), false, false) {
		t.Error("127.0.0.1 must be blocked when allowLoopback=false")
	}
}

func TestBlockedWithPrivateAllowed(t *testing.T) {
	// RFC1918 + loopback + IPv6 ULA permitted when both flags on.
	for _, ip := range []string{"10.0.0.1", "192.168.1.1", "172.16.0.1", "127.0.0.1", "::1", "fd00::1"} {
		if blockedWith(net.ParseIP(ip), true, true) {
			t.Errorf("%s should be allowed when allowLoopback+allowPrivate", ip)
		}
	}
	// Metadata + link-local must NEVER unblock, even under full trust — fd00:ec2::254
	// is itself a ULA, so this proves the metadata check precedes the IsPrivate gate.
	for _, ip := range []string{"169.254.169.254", "fd00:ec2::254", "169.254.1.1", "0.0.0.0"} {
		if !blockedWith(net.ParseIP(ip), true, true) {
			t.Errorf("%s must stay blocked even with allowLoopback+allowPrivate", ip)
		}
	}
	// Flags are independent: allowPrivate alone does not permit loopback, and vice-versa.
	if !blockedWith(net.ParseIP("127.0.0.1"), false, true) {
		t.Error("loopback must stay blocked when only allowPrivate is set")
	}
	if !blockedWith(net.ParseIP("10.0.0.1"), true, false) {
		t.Error("RFC1918 must stay blocked when only allowLoopback is set")
	}
	// Exported IsBlocked mirrors blockedWith for the credential-create guard.
	if IsBlocked(net.ParseIP("169.254.169.254"), Options{AllowLoopback: true, AllowPrivate: true}) != true {
		t.Error("IsBlocked must block metadata under full trust")
	}
	if IsBlocked(net.ParseIP("10.0.0.1"), Options{AllowPrivate: true}) != false {
		t.Error("IsBlocked must allow RFC1918 when AllowPrivate is set")
	}
}

// TestScreenedDialContextRefusesMetadataAndLinkLocal pins the resolved-IP screen the
// egress proxy relies on (manyforge-9er): dialing a literal metadata/link-local address
// must be refused even under full trust (AllowLoopback+AllowPrivate), because these are
// blocked unconditionally by blockedWith. This is the defense-in-depth the deleted
// host-side localReview dialer used to provide — an allowlisted hostname alone is not
// enough, since DNS can rebind to one of these addresses between the allowlist check and
// the dial. A full proxy-level test can't force real DNS to resolve to IMDS, so this
// netsafe-level test (dialing the literal IP/IP-literal-host directly, no DNS involved)
// is what actually exercises the screen.
func TestScreenedDialContextRefusesMetadataAndLinkLocal(t *testing.T) {
	dial := ScreenedDialContext(Options{AllowLoopback: true, AllowPrivate: true})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, addr := range []string{"169.254.169.254:80", "[fe80::1]:80"} {
		conn, err := dial(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			t.Fatalf("dial(%s) succeeded, want blocked-address error", addr)
		}
		if !strings.Contains(err.Error(), "blocked address") {
			t.Fatalf("dial(%s) err = %v, want a blocked-address error", addr, err)
		}
	}
}
