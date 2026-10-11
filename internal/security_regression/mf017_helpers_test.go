package security_regression

import (
	"strings"
	"testing"
)

// mf017Slice returns src from the first start marker up to (not including) the
// first end marker after it. A missing marker fails the test instead of panicking.
func mf017Slice(t *testing.T, src, start, end string) string {
	t.Helper()
	i := strings.Index(src, start)
	if i < 0 {
		t.Fatalf("marker %q not found", start)
	}
	j := strings.Index(src[i:], end)
	if j < 0 {
		t.Fatalf("end marker %q not found after %q", end, start)
	}
	return src[i : i+j]
}

// mf017SQLFunction returns one 0139 function definition, up to its closing $$;.
func mf017SQLFunction(t *testing.T, mig, name string) string {
	t.Helper()
	return mf017Slice(t, mig, "CREATE FUNCTION "+name+"(", "$$;")
}
