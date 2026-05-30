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
		{"127.0.0.1", true},     // loopback
		{"::1", true},           // loopback v6
		{"10.0.0.1", true},      // private
		{"192.168.1.1", true},   // private
		{"172.16.0.1", true},    // private
		{"169.254.169.254", true}, // cloud metadata
		{"169.254.1.1", true},   // link-local
		{"0.0.0.0", true},       // unspecified
		{"fd00:ec2::254", true}, // metadata v6
		{"8.8.8.8", false},      // public
		{"1.1.1.1", false},      // public
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
