package analytics

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenMMDBEmptyPathDisablesCountryLookup(t *testing.T) {
	got, err := OpenMMDB("", nil)
	if err != nil {
		t.Fatalf("OpenMMDB(empty): %v", err)
	}
	if got != nil {
		t.Fatal("OpenMMDB(empty) returned a resolver, want nil")
	}
}

func TestOpenMMDBOfficialCountryFixtureOpens(t *testing.T) {
	path := filepath.Join("..", "..", "scripts", "ci", "testdata", "GeoLite2-Country-Test.mmdb")

	got, err := OpenMMDB(path, nil)
	if err != nil {
		t.Fatalf("OpenMMDB(valid fixture): %v", err)
	}
	if got == nil {
		t.Fatal("OpenMMDB(valid fixture) returned a nil resolver")
	}
	if err := got.Close(); err != nil {
		t.Fatalf("close valid fixture: %v", err)
	}
}

func TestOpenMMDBMissingConfiguredFileWarnsAndDisablesCountryLookup(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	path := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")

	got, err := OpenMMDB(path, logger)
	if err != nil {
		t.Fatalf("OpenMMDB(missing): %v", err)
	}
	if got != nil {
		t.Fatal("OpenMMDB(missing) returned a resolver, want nil")
	}
	if text := logs.String(); !strings.Contains(text, "database not found") ||
		!strings.Contains(text, path) || !strings.Contains(text, "level=WARN") {
		t.Fatalf("warning = %q, want WARN-level missing-database message and configured path", text)
	}
}

func TestOpenMMDBCorruptConfiguredFileRemainsFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "GeoLite2-Country.mmdb")
	if err := os.WriteFile(path, []byte("not a MaxMind database"), 0o600); err != nil {
		t.Fatalf("write corrupt fixture: %v", err)
	}

	got, err := OpenMMDB(path, nil)
	if err == nil {
		t.Fatal("OpenMMDB(corrupt) error = nil, want invalid-database error")
	}
	if got != nil {
		t.Fatal("OpenMMDB(corrupt) returned a resolver, want nil")
	}
	if !strings.Contains(err.Error(), "open geoip db") {
		t.Fatalf("OpenMMDB(corrupt) error = %q, want wrapped geoip context", err)
	}
}

func TestOpenMMDBConfiguredDirectoryRemainsFatal(t *testing.T) {
	path := t.TempDir()

	got, err := OpenMMDB(path, nil)
	if err == nil {
		t.Fatal("OpenMMDB(directory) error = nil, want open error")
	}
	if got != nil {
		t.Fatal("OpenMMDB(directory) returned a resolver, want nil")
	}
	if !strings.Contains(err.Error(), "open geoip db") {
		t.Fatalf("OpenMMDB(directory) error = %q, want wrapped geoip context", err)
	}
}
