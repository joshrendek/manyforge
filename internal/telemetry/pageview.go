package telemetry

import (
	"strings"

	"github.com/google/uuid"
)

// validSignedPageview accepts a route template, not a browser URL. The signing service owns
// the fixed route vocabulary and must replace dynamic identifiers before signing. No query,
// fragment, percent-encoding, matrix parameter, full origin, or free-form Unicode is accepted.
func validSignedPageview(path, sessionID string) bool {
	if len(sessionID) != 36 {
		return false
	}
	id, err := uuid.Parse(sessionID)
	if err != nil || strings.ContainsAny(sessionID, "ABCDEF") || id.Version() != 4 || id.Variant() != uuid.RFC4122 {
		return false
	}
	if path == "/" {
		return true
	}
	if len(path) < 2 || len(path) > 512 || path[0] != '/' || path[len(path)-1] == '/' || strings.Contains(path, sessionID) {
		return false
	}
	segmentStart := true
	for i := 1; i < len(path); i++ {
		ch := path[i]
		if ch == '/' {
			if segmentStart {
				return false
			}
			segmentStart = true
			continue
		}
		if ch == ':' {
			if !segmentStart || i+1 == len(path) || !analyticsPathLetter(path[i+1]) {
				return false
			}
		} else if !analyticsPathLetter(ch) && (ch < '0' || ch > '9') && ch != '-' {
			return false
		}
		segmentStart = false
	}
	return true
}

func analyticsPathLetter(ch byte) bool {
	return ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch == '_'
}
