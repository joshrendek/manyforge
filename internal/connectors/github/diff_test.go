package github

import "testing"

func TestCommentableLines(t *testing.T) {
	// A patch with one hunk: context, an added line, context. New side starts at 10.
	patch := "@@ -10,3 +10,4 @@ func foo() {\n" +
		" ctxA\n" + // new line 10 (context) → commentable
		"+added\n" + // new line 11 (added) → commentable
		" ctxB\n" + // new line 12 (context) → commentable
		"-removed\n" + // old side only → does NOT advance new side, not commentable
		" ctxC\n" // new line 13 (context) → commentable
	got := commentableLines(patch)
	for _, want := range []int{10, 11, 12, 13} {
		if !got[want] {
			t.Fatalf("line %d should be commentable; got=%v", want, got)
		}
	}
	if got[14] {
		t.Fatalf("line 14 is outside the hunk and must not be commentable")
	}
}

func TestCommentableLines_MultiHunk(t *testing.T) {
	patch := "@@ -1,2 +1,2 @@\n-old1\n+new1\n ctx\n" +
		"@@ -50,1 +51,2 @@\n+addA\n+addB\n" +
		"\\ No newline at end of file\n"
	got := commentableLines(patch)
	for _, want := range []int{1 /*new1*/, 2 /*ctx*/, 51 /*addA*/, 52 /*addB*/} {
		if !got[want] {
			t.Fatalf("expected line %d commentable; got=%v", want, got)
		}
	}
	if len(got) != 4 {
		t.Fatalf("expected exactly 4 commentable lines, got %d: %v", len(got), got)
	}
}

func TestCommentableLines_EmptyOrBinary(t *testing.T) {
	if l := commentableLines(""); len(l) != 0 {
		t.Fatalf("empty patch should yield no lines, got %v", l)
	}
}

func TestParseHunks(t *testing.T) {
	patch := "@@ -1,1 +1,2 @@\n ctx\n+added\n"
	hs := ParseHunks(patch)
	if len(hs) != 1 {
		t.Fatalf("want 1 hunk, got %d", len(hs))
	}
	h := hs[0]
	if h.NewStart != 1 {
		t.Fatalf("NewStart=%d, want 1", h.NewStart)
	}
	if len(h.Lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %+v", len(h.Lines), h.Lines)
	}
	if h.Lines[0] != (DiffLine{NewLineNo: 1, Added: false, Text: "ctx"}) {
		t.Fatalf("line0=%+v", h.Lines[0])
	}
	if h.Lines[1] != (DiffLine{NewLineNo: 2, Added: true, Text: "added"}) {
		t.Fatalf("line1=%+v", h.Lines[1])
	}
}

func TestParseHunks_EmptyOrBinary(t *testing.T) {
	if ParseHunks("") != nil {
		t.Fatal("empty patch must yield nil hunks")
	}
}

func TestRenderAnnotatedHunks(t *testing.T) {
	// Format is deterministic ("%5d %s %s\n" per line): right-aligned new-side line
	// number, a space, the +/space marker, a space, then the text. Assert it exactly.
	patch := "@@ -1,1 +1,2 @@\n ctx\n+added\n"
	want := "@@ 1-2 @@\n    1   ctx\n    2 + added\n"
	if got := RenderAnnotatedHunks(patch); got != want {
		t.Fatalf("render mismatch:\nwant: %q\ngot:  %q", want, got)
	}
	if RenderAnnotatedHunks("") != "" {
		t.Fatal("empty patch must render empty string")
	}
}
