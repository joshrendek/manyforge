// Package payments is an eval fixture for local code-review models. It contains
// four DELIBERATELY PLANTED issues (see EXPECTED-FINDINGS.md). Do not "fix" them.
package payments

import (
	"database/sql"
	"fmt"
	"net/http"
)

// PLANTED #3 (hardcoded credential): a live-looking API key committed in source.
const stripeKey = "sk_live_51H8xqL2eZvKYlo3a7d9f2k1mSecretDoNotCommit"

type User struct {
	ID   int
	Name string
}

// findUser returns nil when the user is not found.
func findUser(db *sql.DB, id int) *User {
	u := &User{}
	if err := db.QueryRow("SELECT id, name FROM users WHERE id=$1", id).Scan(&u.ID, &u.Name); err != nil {
		return nil
	}
	return u
}

// Greet builds a greeting for a user.
// PLANTED #1 (nil-pointer dereference): findUser can return nil, but u.Name is
// dereferenced unconditionally → panic when the user does not exist.
func Greet(db *sql.DB, id int) string {
	u := findUser(db, id)
	return "Hello, " + u.Name + "!"
}

// statusHandler writes a health response.
// PLANTED #2 (ignored error): the error from w.Write is discarded, so a failed
// write is silently swallowed.
func statusHandler(w http.ResponseWriter, _ *http.Request) {
	w.Write([]byte(`{"status":"ok"}`))
}

// uploadHandler reads an uploaded body into memory.
// PLANTED #4 (unbounded/unsafe input): it allocates a buffer sized by the
// attacker-controlled Content-Length and reads the body with no size cap — a
// trivial memory-exhaustion DoS.
func uploadHandler(w http.ResponseWriter, r *http.Request) {
	buf := make([]byte, r.ContentLength)
	_, _ = r.Body.Read(buf)
	fmt.Fprintf(w, "received %d bytes with key %s", len(buf), stripeKey)
}
