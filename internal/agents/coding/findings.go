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

// extractJSONObject returns the outermost {…} object from raw model output. The
// opencode CLI prints the model's final message, which models routinely wrap in a
// ```json markdown fence and/or a line of prose, so we locate the object rather
// than require the whole output to be pure JSON. Returns "" when no object is
// present (caller treats that as empty output).
func extractJSONObject(raw string) string {
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end < start {
		return ""
	}
	return raw[start : end+1]
}

// ParseFindings validates opencode's structured output. Empty/malformed → error
// (no partial review is ever posted). The model's JSON object is extracted from
// any surrounding markdown fence/prose first, and finding paths are normalized to
// repo-relative form.
func ParseFindings(raw []byte) (FindingsDoc, error) {
	body := extractJSONObject(string(raw))
	if body == "" {
		return FindingsDoc{}, fmt.Errorf("coding: empty findings output")
	}
	var doc FindingsDoc
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return FindingsDoc{}, fmt.Errorf("coding: malformed findings json: %w", err)
	}
	if strings.TrimSpace(doc.Summary) == "" {
		return FindingsDoc{}, fmt.Errorf("coding: findings missing summary")
	}
	for i := range doc.Findings {
		// Normalize the sandbox absolute path to repo-relative before validating.
		doc.Findings[i].File = strings.TrimPrefix(strings.TrimSpace(doc.Findings[i].File), sandboxSrcPrefix)
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
