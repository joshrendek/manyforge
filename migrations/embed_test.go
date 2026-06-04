package migrations

import (
	"testing"
	"testing/fstest"
)

func TestLatestVersion_Embedded(t *testing.T) {
	// The real embedded FS must yield the highest on-disk migration (>= 34 at time of
	// writing). We assert it parses to a positive version and matches our manual max so a
	// future migration that the embed misses (e.g. wrong glob) fails here.
	got, err := LatestVersion()
	if err != nil {
		t.Fatalf("LatestVersion: %v", err)
	}
	if got < 34 {
		t.Fatalf("LatestVersion() = %d, want >= 34 (the embedded migrations should include 0034+)", got)
	}
}

func TestLatestVersion_ParsesMaxPrefix(t *testing.T) {
	fsys := fstest.MapFS{
		"0001_init.up.sql":        {Data: []byte("")},
		"0001_init.down.sql":      {Data: []byte("")},
		"0002_x.up.sql":           {Data: []byte("")},
		"0010_later.up.sql":       {Data: []byte("")},
		"0010_later.down.sql":     {Data: []byte("")},
		"notanumber_skip.up.sql":  {Data: []byte("")}, // ignored (no numeric prefix)
		"0007_only_down.down.sql": {Data: []byte("")}, // down-only ignored by the up glob
		"readme.txt":              {Data: []byte("")}, // ignored (not *.up.sql)
	}
	got, err := latestVersion(fsys)
	if err != nil {
		t.Fatalf("latestVersion: %v", err)
	}
	if got != 10 {
		t.Fatalf("latestVersion = %d, want 10 (max up-migration prefix)", got)
	}
}

func TestLatestVersion_NoMigrations(t *testing.T) {
	if _, err := latestVersion(fstest.MapFS{"readme.txt": {Data: []byte("")}}); err == nil {
		t.Fatal("latestVersion with no *.up.sql should error, got nil")
	}
}
