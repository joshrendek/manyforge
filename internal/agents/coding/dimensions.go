package coding

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/manyforge/manyforge/internal/connectors"
	"github.com/manyforge/manyforge/internal/platform/errs"
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

// generalDimensionKey is the Key of the zero-config default lane. Findings from this lane are
// left UNTAGGED (Dimension="") during aggregation so a default review's stored/posted shape is
// byte-for-byte the legacy single-agent review — a dimension tag is only meaningful once
// specialist lanes run alongside each other (Slice 2).
const generalDimensionKey = "general"

// defaultPanel is the zero-config review panel: a SINGLE "general" lane using the default
// reviewInstructions, no scope (all files), and no severity floor. It is what a review runs
// when the business has configured no dimensions of its own — making a default review
// byte-for-byte the pre-panel single-agent review (same prompt, same files, every finding
// posted), so shipping the panel machinery adds NO cost or latency for existing users. The
// specialist lanes in dimensionCatalog() are opt-in on top of this.
func defaultPanel() []Dimension {
	return []Dimension{{
		Key:         generalDimensionKey,
		Label:       "General",
		Prompt:      reviewInstructions,
		MinSeverity: "info", // no floor — every finding posts, as today
		Enabled:     true,
		Order:       1,
	}}
}

// dimensionCatalog is the built-in library of specialized reviewer lanes (spec 008). It is
// NOT the zero-config default — defaultPanel() is (a single general lane). These specialists
// are OPT-IN: a business enables/customizes them via presets + the Review Setup UI (Slice 2).
// Enabling every lane multiplies a review's cost and latency (each is a separate model pass,
// and on a single-GPU local provider the passes run sequentially), so the catalog must never
// be auto-enabled as the default. Provider/Model are left empty so each enabled lane runs on
// the review's resolved default model; the value it adds is per-concern PROMPTS + scope
// routing. The UI lane is scoped to frontend paths so it auto-skips backend-only PRs; Docs
// ships disabled (noisiest).
func dimensionCatalog() []Dimension {
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

// dimensionLabel returns a display label for a dimension key, reusing the built-in catalog's
// label for a known specialist (or "General" for the default lane) and falling back to the raw
// key otherwise. Configured rows carry no label column, so the label is derived here.
func dimensionLabel(key string) string {
	if key == generalDimensionKey {
		return "General"
	}
	for _, d := range dimensionCatalog() {
		if d.Key == key {
			return d.Label
		}
	}
	return key
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

// filterFilesByScope returns the changed files whose path matches the dimension's scope globs
// — the per-lane diff subset a scoped dimension reviews. Empty globs ⇒ all files (returned as
// the same slice, so an unscoped lane assembles the identical payload to a whole-PR review).
func filterFilesByScope(files []connectors.ChangedFile, globs []string) []connectors.ChangedFile {
	if len(globs) == 0 {
		return files
	}
	out := make([]connectors.ChangedFile, 0, len(files))
	for _, f := range files {
		if matchesScope(globs, []string{f.Path}) {
			out = append(out, f)
		}
	}
	return out
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

// laneResult is one dimension's outcome in a review fan-out (spec 008): the dimension it ran,
// its findings doc (zero-valued on failure), the tokens/cost it spent, and any error. The
// accounting fields are populated even on Err so a failed-but-billed lane is still summed —
// a lane can burn tokens and then fail to parse (the single-agent manyforge-7n5 rule, applied
// per lane).
type laneResult struct {
	Dim       Dimension
	Doc       FindingsDoc
	Model     string // the model actually used (dim.Model, or the resolved default)
	Provider  string // the provider actually used
	TokensIn  int32
	TokensOut int32
	CostCents int64
	Err       error
	// FailReason is a short, client-safe category for a failed lane ("timed out",
	// "no output produced", "unparseable model output", "sandbox error") — persisted so the
	// UI can show WHY a lane failed. The full Err is logged server-side, never surfaced
	// (it can carry model-output snippets / sandbox internals). Empty on success.
	FailReason string
}

// dimensionRun is the persisted per-lane accounting record (spec 008), serialized into the
// code_review.dimension_runs JSONB array. status is "succeeded"/"failed" for a lane that ran,
// or "skipped" (with SkippedReason) for a configured dimension that did not run this review.
type dimensionRun struct {
	Dimension     string `json:"dimension"`
	Model         string `json:"model,omitempty"`
	Provider      string `json:"provider,omitempty"`
	TokensIn      int32  `json:"tokens_in"`
	TokensOut     int32  `json:"tokens_out"`
	CostCents     int64  `json:"cost_cents"`
	Status        string `json:"status"`
	SkippedReason string `json:"skipped_reason,omitempty"`
	LastError     string `json:"last_error,omitempty"` // client-safe reason a "failed" lane failed (e.g. "timed out")
	FindingCount  int    `json:"finding_count"`
}

// partitionByProvider splits active dimensions into those runnable under the review's resolved
// provider and those skipped because they request a DIFFERENT per-dimension provider. That field
// is persisted + shown in the setup UI but its credential/egress plumbing is not wired yet
// (manyforge-ubk): running such a lane would silently use the review's default provider (wrong
// key/base_url), so skip it with a clear reason instead of misrouting. An empty dim.Provider
// inherits the review default and is always kept.
func partitionByProvider(active []Dimension, reviewProvider string) (kept []Dimension, skipped []SkippedDimension) {
	for _, d := range active {
		if d.Provider != "" && !strings.EqualFold(d.Provider, reviewProvider) {
			skipped = append(skipped, SkippedDimension{Key: d.Key,
				Reason: fmt.Sprintf("per-dimension provider %q not supported yet (review runs on %q)", d.Provider, reviewProvider)})
			continue
		}
		kept = append(kept, d)
	}
	return kept, skipped
}

// buildDimensionRuns turns the fan-out's lane results + skipped dimensions into the persisted
// dimension_runs records. Ran lanes are "succeeded"/"failed" with their usage + raw finding
// count; skipped dimensions are "skipped" with the reason — so every configured lane's outcome
// is recorded, never silently dropped (spec 008 FR-003).
func buildDimensionRuns(results []laneResult, skipped []SkippedDimension) []dimensionRun {
	runs := make([]dimensionRun, 0, len(results)+len(skipped))
	for _, lr := range results {
		status := "succeeded"
		lastErr := ""
		if lr.Err != nil {
			status = "failed"
			lastErr = lr.FailReason
			if lastErr == "" {
				lastErr = "sandbox error" // default category if a failure site didn't set one
			}
		}
		runs = append(runs, dimensionRun{
			Dimension:    lr.Dim.Key,
			Model:        lr.Model,
			Provider:     lr.Provider,
			TokensIn:     lr.TokensIn,
			TokensOut:    lr.TokensOut,
			CostCents:    lr.CostCents,
			Status:       status,
			LastError:    lastErr,
			FindingCount: len(lr.Doc.Findings),
		})
	}
	for _, sd := range skipped {
		runs = append(runs, dimensionRun{Dimension: sd.Key, Status: "skipped", SkippedReason: sd.Reason})
	}
	return runs
}

// aggregateReview combines the per-lane results of a fan-out into ONE review doc plus the
// summed usage (spec 008 FR-005/FR-013). For each surviving lane it floors the findings to the
// lane's MinSeverity and tags them with the dimension key (the zero-config general lane is left
// untagged, matching the legacy single-agent shape), then de-duplicates the union across lanes
// (same file+line+title → highest severity, unioned tags). Tokens and cost are summed across
// ALL lanes, including failed ones. Partial success (FR-013): if ANY lane produced a doc the
// review proceeds with the survivors; only if EVERY lane failed does it return an error — the
// first lane's error, so the failure surfaced to the user is a real one.
func aggregateReview(results []laneResult) (doc FindingsDoc, tokensIn, tokensOut int32, costCents int64, err error) {
	var findings []connectors.Finding
	var summaries []string
	anyOK := false
	var firstErr error
	for _, lr := range results {
		tokensIn += lr.TokensIn
		tokensOut += lr.TokensOut
		costCents += lr.CostCents
		if lr.Err != nil {
			if firstErr == nil {
				firstErr = lr.Err
			}
			continue
		}
		anyOK = true
		fs := applySeverityFloor(lr.Doc.Findings, lr.Dim.MinSeverity)
		if lr.Dim.Key != generalDimensionKey {
			for i := range fs {
				fs[i].Dimension = lr.Dim.Key
			}
		}
		findings = append(findings, fs...)
		if s := strings.TrimSpace(lr.Doc.Summary); s != "" {
			summaries = append(summaries, s)
		}
	}
	if !anyOK {
		if firstErr == nil {
			// No lane ran at all (every dimension skipped). Never return a nil error with an
			// empty doc — the caller would post it as a bogus "No issues found" review that
			// falsely implies the PR was checked (manyforge-t2s / PR #8 review).
			firstErr = fmt.Errorf("coding: no dimensions produced a review: %w", errs.ErrValidation)
		}
		return FindingsDoc{}, tokensIn, tokensOut, costCents, firstErr
	}
	return FindingsDoc{
		Summary:  strings.Join(summaries, "\n\n"),
		Findings: dedupeFindings(findings),
	}, tokensIn, tokensOut, costCents, nil
}
