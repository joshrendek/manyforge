package github

import (
	"strconv"
	"strings"
)

// commentableLines parses a GitHub unified-diff patch (the files[].patch field)
// and returns the set of NEW-side (RIGHT) line numbers that fall inside a diff
// hunk — the only lines GitHub accepts as inline-comment targets. Added and
// context lines advance and mark the new side; removed lines advance only the old
// side. Lines outside any hunk are not commentable. Returns an empty map for an
// empty/absent patch (binary or too-large files have no patch).
func commentableLines(patch string) map[int]bool {
	out := map[int]bool{}
	newLine := 0
	inHunk := false
	for _, ln := range strings.Split(patch, "\n") {
		if strings.HasPrefix(ln, "@@") {
			newLine = parseHunkNewStart(ln)
			inHunk = newLine > 0
			continue
		}
		if !inHunk {
			continue
		}
		switch {
		case strings.HasPrefix(ln, "+"):
			out[newLine] = true
			newLine++
		case strings.HasPrefix(ln, "-"):
			// removed: old side only, do not advance the new side
		case strings.HasPrefix(ln, "\\"):
			// "\ No newline at end of file" marker — ignore
		case ln == "":
			// trailing artifact from splitting on the final newline — not a real
			// hunk body line (genuine blank context lines start with a space)
		default:
			// context line (leading space) → present on the new side, commentable
			out[newLine] = true
			newLine++
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
