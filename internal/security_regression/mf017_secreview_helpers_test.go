package security_regression

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// secreviewUpMigrations returns the *.up.sql migration paths, sorted by version.
func secreviewUpMigrations(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("../../migrations/*.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no up migrations found")
	}
	sort.Strings(files)
	return files
}

// secreviewLatestFunction returns the body (up to its closing $$;) of fn's
// definition in the highest-numbered *.up.sql migration that defines it, plus that
// file's name. Pinning the LATEST definition means a future migration that
// replaces the function without the fix fails here, not just an old file.
func secreviewLatestFunction(t *testing.T, fn string) (body, file string) {
	t.Helper()
	files := secreviewUpMigrations(t)
	for i := len(files) - 1; i >= 0; i-- {
		src := mustRead(t, files[i])
		for _, marker := range []string{"CREATE OR REPLACE FUNCTION " + fn + "(", "CREATE FUNCTION " + fn + "("} {
			if strings.Contains(src, marker) {
				return mf017Slice(t, src, marker, "$$;"), filepath.Base(files[i])
			}
		}
	}
	t.Fatalf("no migration defines %s", fn)
	return "", ""
}

// secreviewMigrationsMentioning returns the up-migration file names (sorted)
// whose SQL contains s.
func secreviewMigrationsMentioning(t *testing.T, s string) []string {
	t.Helper()
	var out []string
	for _, f := range secreviewUpMigrations(t) {
		if strings.Contains(mustRead(t, f), s) {
			out = append(out, filepath.Base(f))
		}
	}
	return out
}
