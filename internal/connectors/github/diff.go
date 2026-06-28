package github

import (
	"fmt"
	"strconv"
	"strings"
)

// DiffLine is one new-side line of a diff hunk (added or context). Removed lines
// have no new-side number and are not represented.
type DiffLine struct {
	NewLineNo int    // new-side (RIGHT) line number
	Added     bool   // true if a '+' line, false if a context line
	Text      string // line content without the leading +/space marker
}

// Hunk is a contiguous run of new-side lines from one "@@" block.
type Hunk struct {
	NewStart int
	Lines    []DiffLine
}

// ParseHunks parses a GitHub unified-diff patch (the files[].patch field) into its
// hunks, tracking the new-side (RIGHT) line number of every added and context line.
// Removed lines advance only the old side and are dropped. Returns nil for an
// empty/absent patch (binary or too-large files have no patch).
func ParseHunks(patch string) []Hunk {
	var hunks []Hunk
	var cur *Hunk
	newLine := 0
	inHunk := false
	for _, ln := range strings.Split(patch, "\n") {
		if strings.HasPrefix(ln, "@@") {
			newLine = parseHunkNewStart(ln)
			inHunk = newLine > 0
			if inHunk {
				hunks = append(hunks, Hunk{NewStart: newLine})
				cur = &hunks[len(hunks)-1]
			} else {
				cur = nil
			}
			continue
		}
		if !inHunk {
			continue
		}
		switch {
		case strings.HasPrefix(ln, "+"):
			cur.Lines = append(cur.Lines, DiffLine{NewLineNo: newLine, Added: true, Text: ln[1:]})
			newLine++
		case strings.HasPrefix(ln, "-"):
			// removed: old side only, do not advance the new side
		case strings.HasPrefix(ln, "\\"):
			// "\ No newline at end of file" marker — ignore
		case ln == "":
			// trailing artifact from splitting on the final newline — not a real
			// hunk body line (genuine blank context lines start with a space)
		default:
			// context line (leading space) → present on the new side
			cur.Lines = append(cur.Lines, DiffLine{NewLineNo: newLine, Added: false, Text: ln[1:]})
			newLine++
		}
	}
	return hunks
}

// RenderAnnotatedHunks renders a patch as gutter-numbered hunks: each changed line
// shows its current-file (new-side) line number and a +/space marker, so a model
// can cite exact, in-diff line numbers. Returns "" when the patch has no hunks.
func RenderAnnotatedHunks(patch string) string {
	hunks := ParseHunks(patch)
	if len(hunks) == 0 {
		return ""
	}
	var b strings.Builder
	for _, h := range hunks {
		end := h.NewStart
		if n := len(h.Lines); n > 0 {
			end = h.Lines[n-1].NewLineNo
		}
		fmt.Fprintf(&b, "@@ %d-%d @@\n", h.NewStart, end)
		for _, l := range h.Lines {
			marker := " "
			if l.Added {
				marker = "+"
			}
			fmt.Fprintf(&b, "%5d %s %s\n", l.NewLineNo, marker, l.Text)
		}
	}
	return b.String()
}

// commentableLines returns the set of new-side line numbers that fall inside a diff
// hunk — the only lines GitHub accepts as inline-comment targets. Built on
// ParseHunks so there is a single hunk parser. Empty for an empty/absent patch.
func commentableLines(patch string) map[int]bool {
	out := map[int]bool{}
	for _, h := range ParseHunks(patch) {
		for _, l := range h.Lines {
			out[l.NewLineNo] = true
		}
	}
	return out
}

// parseHunkNewStart extracts the new-side start line from a hunk header
// "@@ -a,b +c,d @@ optional" → c. Returns 0 if it can't parse.
func parseHunkNewStart(header string) int {
	plus := strings.IndexByte(header, '+')
	if plus < 0 {
		return 0
	}
	rest := header[plus+1:]
	end := strings.IndexAny(rest, ", ")
	if end < 0 {
		end = len(rest)
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0
	}
	return n
}
