package coding

import (
	"regexp"
	"strings"
)

const redactedMarker = "[REDACTED]"

// secretPatterns scrub common API-key shapes for secrets we don't hold verbatim.
// Patterns are specific (known prefixes + length floors) to limit over-redaction.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_\-]{20,}`),            // OpenAI/Anthropic/OpenRouter
	regexp.MustCompile(`ghp_[A-Za-z0-9]{36}`),               // GitHub PAT (classic)
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{50,}`),      // GitHub fine-grained PAT
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{16,}`), // bearer auth header
}

// redactSecrets removes secrets from text bound for storage or a posted review:
// first the exact known values (e.g. the LLM key / GitHub token we hold), then a
// regex scrub of common key shapes for secrets we don't hold. Known values shorter
// than 8 chars are ignored so a trivial value can't mangle unrelated text.
func redactSecrets(s string, known ...string) string {
	for _, k := range known {
		if len(k) >= 8 {
			s = strings.ReplaceAll(s, k, redactedMarker)
		}
	}
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, redactedMarker)
	}
	return s
}

// redactDoc scrubs secrets from a findings doc before it is posted to the PR (and
// stored on the review row). Mutates the doc in place.
func redactDoc(doc *FindingsDoc, known ...string) {
	doc.Summary = redactSecrets(doc.Summary, known...)
	for i := range doc.Findings {
		doc.Findings[i].Title = redactSecrets(doc.Findings[i].Title, known...)
		doc.Findings[i].Detail = redactSecrets(doc.Findings[i].Detail, known...)
	}
}
