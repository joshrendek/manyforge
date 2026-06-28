package coding

import (
	"fmt"
	"sort"
	"strings"

	"github.com/manyforge/manyforge/internal/connectors"
)

// changedFilePaths returns the changed file paths (keys) sorted, for the scoped
// review file list handed to the sandbox.
func changedFilePaths(changed map[string]map[int]bool) []string {
	paths := make([]string, 0, len(changed))
	for p := range changed {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// buildReview maps a parsed findings doc onto a connectors.Review. A finding whose
// (file, line) is a commentable line of the PR diff becomes an inline comment; the
// rest go into the summary body so nothing is lost and the review always posts
// (even when no diff info is available — then everything lands in the body, which
// is the pre-inline behavior). commitID anchors the inline comments to the
// reviewed head SHA.
func buildReview(doc FindingsDoc, changed map[string]map[int]bool, commitID string, skipped, omitted []string) connectors.Review {
	var comments []connectors.ReviewComment
	var leftover []connectors.Finding
	for _, f := range doc.Findings {
		if f.Line != nil {
			if lines, ok := changed[f.File]; ok && lines[*f.Line] {
				comments = append(comments, connectors.ReviewComment{
					Path: f.File,
					Line: *f.Line,
					Body: renderInlineComment(f),
				})
				continue
			}
		}
		leftover = append(leftover, f)
	}
	return connectors.Review{
		Summary:  doc.Summary,
		Findings: doc.Findings,
		Body:     renderReviewBody(doc.Summary, leftover, len(comments), skipped, omitted),
		CommitID: commitID,
		Comments: comments,
	}
}

// renderInlineComment formats one finding as an inline review-comment body.
func renderInlineComment(f connectors.Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**[%s] %s**", strings.ToUpper(f.Severity), f.Title)
	if strings.TrimSpace(f.Detail) != "" {
		b.WriteString("\n\n")
		b.WriteString(f.Detail)
	}
	return b.String()
}

// renderReviewBody renders the top-level review body: header, the model's summary,
// a note about how many inline comments were posted, and any findings that could
// not be placed inline (file/line absent from the diff).
func renderReviewBody(summary string, leftover []connectors.Finding, inlineCount int, skipped, omitted []string) string {
	var b strings.Builder
	b.WriteString("## 🤖 Automated code review\n\n")
	b.WriteString(summary)
	b.WriteString("\n\n")
	if inlineCount > 0 {
		fmt.Fprintf(&b, "_%d inline comment(s) posted on the changed lines._\n", inlineCount)
	}
	if len(leftover) > 0 {
		fmt.Fprintf(&b, "\n### Other findings (not on changed lines) — %d\n\n", len(leftover))
		for _, f := range leftover {
			loc := f.File
			if f.Line != nil {
				loc = fmt.Sprintf("%s:%d", f.File, *f.Line)
			}
			fmt.Fprintf(&b, "- **[%s]** `%s` — %s\n", strings.ToUpper(f.Severity), loc, f.Title)
			if strings.TrimSpace(f.Detail) != "" {
				b.WriteString("  " + f.Detail + "\n")
			}
		}
	}
	if len(skipped) > 0 {
		fmt.Fprintf(&b, "\nⓘ %d file(s) not reviewed (binary or too large): %s\n", len(skipped), strings.Join(skipped, ", "))
	}
	if len(omitted) > 0 {
		fmt.Fprintf(&b, "\n⚠ %d file(s) omitted (diff too large for the review budget): %s\n", len(omitted), strings.Join(omitted, ", "))
	}
	return b.String()
}
