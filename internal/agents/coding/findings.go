package coding

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/manyforge/manyforge/internal/connectors"
)

type FindingsDoc struct {
	Summary  string               `json:"summary"`
	Findings []connectors.Finding `json:"findings"`
}

var validSeverity = map[string]bool{"info": true, "warning": true, "error": true}

// sandboxSrcPrefix is the in-sandbox working dir the entrypoint copies the
// checkout into (deploy/sandbox/entrypoint.sh: `cp -r /work /tmp/src; cd /tmp/src`).
// opencode reports absolute paths under it; we strip it so findings are
// repo-relative (needed for GitHub line links). Keep in sync with entrypoint.sh.
const sandboxSrcPrefix = "/tmp/src/"

// topLevelJSONObjects returns every balanced, top-level {…} region in s, scanning
// string-aware (braces inside JSON strings don't count). opencode prints the
// model's final message, which models wrap in markdown fences and/or prose that
// can itself contain braces (e.g. "the {createReview} fn ..."), so a naive
// first-{-to-last-} grab picks the wrong object. Returning all candidates lets the
// caller try each and keep the one that's actually a findings document.
func topLevelJSONObjects(s string) []string {
	var out []string
	depth, start := 0, -1
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					out = append(out, s[start:i+1])
					start = -1
				}
			}
		}
	}
	return out
}

// snippet returns a short, single-line preview of raw model output for error
// messages (debuggability of model-compat issues). Server-side only.
func snippet(raw []byte) string {
	s := strings.Join(strings.Fields(string(raw)), " ")
	const max = 280
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// ParseFindings validates opencode's structured output. Empty/malformed → error
// (no partial review is ever posted). It tolerates the ways different models wrap
// JSON — markdown fences, leading/trailing prose, prose containing braces, and
// extra/unknown fields — by scanning for balanced {…} candidates and keeping the
// first that decodes into a findings document with a summary. Finding paths are
// normalized to repo-relative form.
func ParseFindings(raw []byte) (FindingsDoc, error) {
	candidates := topLevelJSONObjects(string(raw))
	if len(candidates) == 0 {
		return FindingsDoc{}, fmt.Errorf("coding: empty findings output (got: %q)", snippet(raw))
	}
	var doc FindingsDoc
	var found, anyDecoded bool
	for _, cand := range candidates {
		var d FindingsDoc
		// Lenient decode (no DisallowUnknownFields): models often add extra keys;
		// an advisory review should not fail on them.
		if err := json.Unmarshal([]byte(cand), &d); err != nil {
			continue // a prose brace or non-JSON region — try the next candidate
		}
		anyDecoded = true
		// A review is the candidate that has a findings array (even empty = "no
		// issues found") OR a non-empty summary. A bare {} (no findings key, no
		// summary) is treated as garbage, not a clean review.
		if d.Findings != nil || strings.TrimSpace(d.Summary) != "" {
			doc, found = d, true
			break
		}
	}
	if !found {
		if anyDecoded {
			return FindingsDoc{}, fmt.Errorf("coding: no findings object in output (got: %q)", snippet(raw))
		}
		return FindingsDoc{}, fmt.Errorf("coding: malformed findings json (got: %q)", snippet(raw))
	}
	// Models sometimes leave the summary blank on a clean review; default it rather
	// than fail — a no-issues review is valid and worth posting (advisory).
	if strings.TrimSpace(doc.Summary) == "" {
		if len(doc.Findings) == 0 {
			doc.Summary = "No issues found."
		} else {
			doc.Summary = "Code review findings."
		}
	}
	for i := range doc.Findings {
		// Normalize the sandbox absolute path to repo-relative before validating.
		doc.Findings[i].File = strings.TrimPrefix(strings.TrimSpace(doc.Findings[i].File), sandboxSrcPrefix)
		// Normalize severity: models vary on whitespace/case (e.g. " Warning ").
		doc.Findings[i].Severity = strings.ToLower(strings.TrimSpace(doc.Findings[i].Severity))
		f := doc.Findings[i]
		if f.File == "" || f.Title == "" {
			return FindingsDoc{}, fmt.Errorf("coding: finding %d missing file/title", i)
		}
		if !validSeverity[f.Severity] {
			return FindingsDoc{}, fmt.Errorf("coding: finding %d bad severity %q", i, f.Severity)
		}
	}
	return doc, nil
}
