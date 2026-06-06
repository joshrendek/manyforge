package netsafe

import (
	"net"
	"testing"
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
