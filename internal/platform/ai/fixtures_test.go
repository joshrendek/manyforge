package ai

import (
	"os"
	"path/filepath"
	"testing"
)

// loadGolden reads a recorded provider response body from testdata/. These are
// real provider wire shapes recorded once and replayed in CI (no live calls).
func loadGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("loadGolden %s: %v", name, err)
	}
	return b
}
