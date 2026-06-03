package ai

import (
	"os"
	"path/filepath"
	"testing"
)

// loadGolden reads a recorded provider response body from testdata/. These are
// real provider wire shapes recorded once and replayed in CI (no live calls).
// Refresh them with AI_RECORD=1 (see TestRecord* below) if a provider changes
// its format.
func loadGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("loadGolden %s: %v", name, err)
	}
	return b
}

// recording reports whether AI_RECORD mode is on (maintainer refresh of golden
// fixtures against the live API). Off in CI.
func recording() bool { return os.Getenv("AI_RECORD") != "" }
