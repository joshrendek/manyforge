// Nested module so the deliberately-buggy fixture below is NOT part of the repo's
// `go build ./...` / `go vet ./...` (the parent build stops at this go.mod). The
// code is review fodder for local models, not real code — it need not compile.
module evalfixture

go 1.23
