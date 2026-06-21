package sandbox

import "testing"

func TestValidateEnvKey(t *testing.T) {
	tests := []struct {
		key   string
		valid bool
	}{
		{"LLM_API_KEY", true},
		{"FOO=BAR", false},
		{"bad key", false},
		{"lower", false}, // lowercase key — regex requires all-caps identifier
		{"", false},
	}
	for _, tc := range tests {
		got := validEnvKey(tc.key)
		if got != tc.valid {
			t.Errorf("validEnvKey(%q) = %v, want %v", tc.key, got, tc.valid)
		}
	}
}
