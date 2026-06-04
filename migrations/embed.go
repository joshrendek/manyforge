// Package migrations embeds the SQL migration files so the binary carries the schema
// version it expects, independent of the working directory. The server uses LatestVersion
// at startup to refuse to serve a database that is behind the code (which would otherwise
// 500 on missing columns/tables — e.g. a query selecting a column a newer migration adds).
package migrations

import (
	"embed"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

//go:embed *.sql
var FS embed.FS

// LatestVersion returns the highest migration version (the NNNN filename prefix) among the
// embedded *.up.sql files — the schema version this build of the code expects.
func LatestVersion() (int, error) {
	return latestVersion(FS)
}

// latestVersion is the pure core (testable over any fs.FS): the max numeric prefix of the
// *.up.sql files. Files without a numeric prefix are ignored; zero up-migrations is an error.
func latestVersion(fsys fs.FS) (int, error) {
	names, err := fs.Glob(fsys, "*.up.sql")
	if err != nil {
		return 0, fmt.Errorf("migrations: glob: %w", err)
	}
	max := 0
	for _, name := range names {
		prefix, _, ok := strings.Cut(name, "_")
		if !ok {
			continue
		}
		v, err := strconv.Atoi(prefix)
		if err != nil {
			continue
		}
		if v > max {
			max = v
		}
	}
	if max == 0 {
		return 0, fmt.Errorf("migrations: no versioned *.up.sql files found")
	}
	return max, nil
}
