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

// ParseFindings validates opencode's structured output. Empty/malformed → error
// (no partial review is ever posted).
func ParseFindings(raw []byte) (FindingsDoc, error) {
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) == 0 {
		return FindingsDoc{}, fmt.Errorf("coding: empty findings output")
	}
	var doc FindingsDoc
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return FindingsDoc{}, fmt.Errorf("coding: malformed findings json: %w", err)
	}
	if strings.TrimSpace(doc.Summary) == "" {
		return FindingsDoc{}, fmt.Errorf("coding: findings missing summary")
	}
	for i, f := range doc.Findings {
		if f.File == "" || f.Title == "" {
			return FindingsDoc{}, fmt.Errorf("coding: finding %d missing file/title", i)
		}
		if !validSeverity[f.Severity] {
			return FindingsDoc{}, fmt.Errorf("coding: finding %d bad severity %q", i, f.Severity)
		}
	}
	return doc, nil
}

func RenderMarkdown(doc FindingsDoc) string {
	var b strings.Builder
	b.WriteString("## 🤖 Automated code review\n\n")
	b.WriteString(doc.Summary)
	b.WriteString("\n\n")
	if len(doc.Findings) == 0 {
		b.WriteString("_No specific findings._\n")
		return b.String()
	}
	b.WriteString(fmt.Sprintf("### Findings (%d)\n\n", len(doc.Findings)))
	for _, f := range doc.Findings {
		loc := f.File
		if f.Line != nil {
			loc = fmt.Sprintf("%s:%d", f.File, *f.Line)
		}
		b.WriteString(fmt.Sprintf("- **[%s]** `%s` — %s\n", strings.ToUpper(f.Severity), loc, f.Title))
		if strings.TrimSpace(f.Detail) != "" {
			b.WriteString("  " + f.Detail + "\n")
		}
	}
	return b.String()
}
