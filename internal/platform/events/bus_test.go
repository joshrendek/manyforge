package events

import "testing"

func TestIsFinalAttempt(t *testing.T) {
	for _, tc := range []struct {
		attempts, max int32
		want          bool
	}{
		{0, 0, false}, // in-process bus: no retry budget, never short-circuit
		{0, 10, false},
		{8, 10, false},
		{9, 10, true},
		{12, 10, true},
	} {
		if got := (Event{Attempts: tc.attempts, MaxAttempts: tc.max}).IsFinalAttempt(); got != tc.want {
			t.Errorf("IsFinalAttempt(%d/%d) = %v, want %v", tc.attempts, tc.max, got, tc.want)
		}
	}
}
