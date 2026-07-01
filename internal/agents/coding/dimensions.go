package coding

import (
	"path"
	"sort"
	"strings"

	"github.com/manyforge/manyforge/internal/connectors"
)

// Dimension is one specialized reviewer lane in a code-review panel (spec 008): its own
// model, prompt, file scope, and severity floor. A review fans out across the active
// dimensions and aggregates their findings. An empty Provider/Model means "use the
// review's default resolved credential" — so a zero-config business runs every default
// dimension on the triggering agent's model, differing only by prompt + scope.
type Dimension struct {
	Key         string   // stable id: security|correctness|performance|ui|docs|tests
	Label       string   // display label, e.g. "Security"
	Provider    string   // resolves the per-provider BYO credential; "" ⇒ default
	Model       string   // "" ⇒ default (the review's resolved model)
	Prompt      string   // review instructions for this lane (reviewSchemaLine is appended by the caller)
	ScopeGlobs  []string // file globs (doublestar); empty ⇒ all files
	MinSeverity string   // "info" | "warning" | "error" — floor below which findings are dropped
	Enabled     bool
	Order       int
}

// SkippedDimension records a configured dimension that did not run this review, with why
// (so a skip is surfaced, never silent).
type SkippedDimension struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// defaultDimensions is the built-in review panel used when a business has configured no
// dimensions of its own. Provider/Model are left empty so each lane runs on the review's
// resolved default model; the value it adds out of the box is per-concern PROMPTS +
// scope routing (vs today's single blended prompt). The UI lane is scoped to frontend
// paths so it auto-skips backend-only PRs. Docs is shipped disabled (noisiest by default).
func defaultDimensions() []Dimension {
	return []Dimension{
		{
			Key: "security", Label: "Security", Order: 1, Enabled: true, MinSeverity: "warning",
			Prompt: "You are a senior application-security engineer reviewing a pull request. Report ONLY security concerns: injection (SQL/command/template), authentication or authorization gaps, secret/credential exposure, unsafe or unbounded input handling, SSRF, path traversal, insecure deserialization, and missing validation on trust boundaries. Severity: error = an exploitable vulnerability; warning = a risky pattern or missing control; info = a hardening suggestion. Do not report style, performance, or non-security issues, and do not fabricate issues with no basis in the code.",
		},
		{
			Key: "correctness", Label: "Correctness", Order: 2, Enabled: true, MinSeverity: "info",
			Prompt: "You are a senior software engineer reviewing a pull request for CORRECTNESS. Report bugs and logic errors: crashes, nil/undefined access, off-by-one, incorrect conditions, race conditions and concurrency hazards, resource leaks, unhandled errors, and wrong results. Severity: error = a real bug causing a crash, data loss, or incorrect behavior; warning = a likely bug or risky pattern; info = a plausible concern worth surfacing. Skip pure style and performance; do not fabricate issues.",
		},
		{
			Key: "performance", Label: "Performance", Order: 3, Enabled: true, MinSeverity: "warning",
			ScopeGlobs: []string{"**/*.go", "**/*.ts", "**/*.tsx", "**/*.js", "**/*.py", "**/*.rs", "**/*.java", "**/*.sql"},
			Prompt:     "You are a performance engineer reviewing a pull request. Report efficiency concerns: N+1 queries, unbounded loops or allocations, blocking I/O on hot paths, missing indexes or pagination, redundant work, and quadratic behavior. Severity: error = a change that will clearly degrade production performance; warning = a likely inefficiency; info = an optimization worth considering. Skip correctness and style; do not fabricate issues.",
		},
		{
			Key: "ui", Label: "UI / A11y", Order: 4, Enabled: true, MinSeverity: "info",
			ScopeGlobs: []string{"frontend/**", "web/**", "**/*.tsx", "**/*.jsx", "**/*.vue", "**/*.svelte", "**/*.css", "**/*.scss", "**/*.html"},
			Prompt:     "You are a frontend engineer reviewing a pull request for UI quality and ACCESSIBILITY. Report: missing ARIA/roles/labels, keyboard-navigation and focus-management gaps, insufficient color contrast, non-semantic markup, layout/responsive issues, and inconsistent component usage. Severity: error = a broken or inaccessible experience; warning = a likely UX/a11y problem; info = a polish suggestion. Skip backend logic; do not fabricate issues.",
		},
		{
			Key: "tests", Label: "Tests", Order: 5, Enabled: true, MinSeverity: "warning",
			Prompt: "You are a senior engineer reviewing a pull request's TEST coverage and quality. Report: new or changed logic that lacks tests, missing edge/error-case coverage, weak assertions, flaky patterns (time/order/network dependence), and tests that don't actually exercise the change. Severity: error = untested critical/security logic; warning = a meaningful coverage or quality gap; info = a testing suggestion. Do not fabricate issues.",
		},
		{
			Key: "docs", Label: "Docs & Comments", Order: 6, Enabled: false, MinSeverity: "info",
			Prompt: "You are reviewing a pull request for DOCUMENTATION and COMMENT quality. Report: comments that no longer match the code (comment rot), missing docs on exported/public API, misleading names, and stale references. Severity: warning = a misleading or wrong comment/doc; info = a documentation suggestion. Skip code logic; do not fabricate issues.",
		},
	}
}

// matchGlob reports whether name matches a glob pattern that may contain "**" (any number
// of path segments, including zero) and "*" (any run of non-separator chars within a
// single segment). It is a compact doublestar matcher; standard path.Match handles each
// non-"**" segment.
func matchGlob(pattern, name string) bool {
	name = strings.TrimPrefix(strings.TrimSpace(name), "./")
	return matchSegments(strings.Split(strings.TrimSpace(pattern), "/"), strings.Split(name, "/"))
}

func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			rest := pat[1:]
			if len(rest) == 0 {
				return true // trailing ** matches any remaining segments (incl. none)
			}
			for i := 0; i <= len(name); i++ { // ** consumes 0..len(name) segments
				if matchSegments(rest, name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], name[0]); err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// matchesScope reports whether a dimension with the given globs applies to any of the
// changed paths. Empty globs ⇒ the dimension reviews everything.
func matchesScope(globs, paths []string) bool {
	if len(globs) == 0 {
		return true
	}
	for _, g := range globs {
		if strings.TrimSpace(g) == "" {
			continue
		}
		for _, p := range paths {
			if matchGlob(g, p) {
				return true
			}
		}
	}
	return false
}

// activeDimensions splits the panel into the enabled dimensions that apply to this PR's
// changed paths (in Order) and the ones that were skipped, with a reason. Disabled
// dimensions and dimensions whose scope matches no changed file are skipped — never
// silently dropped (spec 008 FR-003).
func activeDimensions(dims []Dimension, changedPaths []string) (active []Dimension, skipped []SkippedDimension) {
	ordered := append([]Dimension(nil), dims...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Order < ordered[j].Order })
	for _, d := range ordered {
		switch {
		case !d.Enabled:
			skipped = append(skipped, SkippedDimension{Key: d.Key, Reason: "disabled"})
		case !matchesScope(d.ScopeGlobs, changedPaths):
			skipped = append(skipped, SkippedDimension{Key: d.Key, Reason: "scope: no matching files"})
		default:
			active = append(active, d)
		}
	}
	return active, skipped
}

// severityRank orders the three severities; an unknown value ranks lowest so a floor
// never accidentally drops a real finding on a typo.
func severityRank(s string) int {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "error":
		return 2
	case "warning":
		return 1
	default:
		return 0 // "info" and anything unexpected
	}
}

// applySeverityFloor drops findings below the dimension's minimum severity.
func applySeverityFloor(findings []connectors.Finding, minSeverity string) []connectors.Finding {
	floor := severityRank(minSeverity)
	out := findings[:0:0]
	for _, f := range findings {
		if severityRank(f.Severity) >= floor {
			out = append(out, f)
		}
	}
	return out
}

// dedupeFindings collapses findings that refer to the same (file, line, title) into one,
// keeping the highest severity and the joined set of contributing dimensions (spec 008
// FR-005). Order of first appearance is preserved.
func dedupeFindings(findings []connectors.Finding) []connectors.Finding {
	type key struct {
		file, title string
		line        int // -1 for nil
	}
	idx := make(map[key]int, len(findings))
	var out []connectors.Finding
	for _, f := range findings {
		k := key{
			file:  strings.TrimSpace(f.File),
			title: strings.ToLower(strings.TrimSpace(f.Title)),
			line:  -1,
		}
		if f.Line != nil {
			k.line = *f.Line
		}
		if i, seen := idx[k]; seen {
			// Keep the higher severity; union the dimension tags.
			if severityRank(f.Severity) > severityRank(out[i].Severity) {
				out[i].Severity = f.Severity
				out[i].Detail = f.Detail
			}
			out[i].Dimension = mergeDimensionTags(out[i].Dimension, f.Dimension)
			continue
		}
		idx[k] = len(out)
		out = append(out, f)
	}
	return out
}

// mergeDimensionTags unions two comma-separated dimension tag sets into a sorted, unique
// comma-separated string.
func mergeDimensionTags(a, b string) string {
	set := map[string]bool{}
	for _, s := range append(strings.Split(a, ","), strings.Split(b, ",")...) {
		if t := strings.TrimSpace(s); t != "" {
			set[t] = true
		}
	}
	tags := make([]string, 0, len(set))
	for t := range set {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	return strings.Join(tags, ", ")
}
