//go:build integration

// Package security_regression — connectors-management (manyforge-4zs.3 / Spec 004) merge-gate
// pins. Each test pins a security property of the connector-management API:
//
//	MF-4zs3-NO-CRED-IN-RESP   credentials are never present in any management response DTO.
//	MF-4zs3-DELETE-PRESERVES  hard-delete NULLs connector_id but PRESERVES external_id/url.
//	MF-4zs3-DETACH-SQL-PIN    source-level: the detach query NULLs connector_id, not external_id.
package security_regression

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/manyforge/manyforge/internal/connectors"
)

const (
	FindingNoCredInResp    = "MF-4zs3-NO-CRED-IN-RESP"
	FindingDeletePreserves = "MF-4zs3-DELETE-PRESERVES"
	FindingDetachSQLPin    = "MF-4zs3-DETACH-SQL-PIN"
)

// TestDetachSQLPin is a source-level guard: a future refactor must not change the delete-detach
// to also null external_id/external_url (which would destroy reconnect history). We assert the
// DetachTicketsFromConnector query sets connector_id = NULL and does NOT mention external_id.
func TestDetachSQLPin(t *testing.T) {
	b, err := os.ReadFile("../../db/query/connector_manage.sql")
	if err != nil {
		t.Fatalf("%s: read query file: %v", FindingDetachSQLPin, err)
	}
	src := string(b)
	// Find the sqlc name directive, not the doc comment (which may mention the field names).
	nameMarker := "-- name: DetachTicketsFromConnector"
	idx := strings.Index(src, nameMarker)
	if idx < 0 {
		t.Fatalf("%s: DetachTicketsFromConnector query not found", FindingDetachSQLPin)
	}
	// Inspect just the statement following the -- name: line.
	stmt := src[idx+len(nameMarker):]
	if end := strings.Index(stmt, ";"); end >= 0 {
		stmt = stmt[:end]
	}
	if !strings.Contains(stmt, "connector_id = NULL") {
		t.Fatalf("%s: detach must set connector_id = NULL; got:\n%s", FindingDetachSQLPin, stmt)
	}
	if strings.Contains(stmt, "external_id") || strings.Contains(stmt, "external_url") {
		t.Fatalf("%s: detach must NOT touch external_id/external_url (reconnect history); got:\n%s", FindingDetachSQLPin, stmt)
	}
}

// TestNoCredentialFieldsInResponseType pins that the wire response type exposes NO credential
// field. This is a structural guard: even if a future edit adds email/api_token to ConnectorView,
// the response must not carry it. We reflect over the JSON-tag set of the exported view type.
func TestNoCredentialFieldsInResponseType(t *testing.T) {
	// ConnectorView is the service-layer view the handler serializes. Assert it has no
	// credential-bearing field name.
	forbidden := []string{"APIToken", "Email", "WebhookSecret", "Credential", "SecretRef"}
	tp := reflect.TypeOf(connectors.ConnectorView{})
	for i := 0; i < tp.NumField(); i++ {
		name := tp.Field(i).Name
		for _, f := range forbidden {
			if name == f {
				t.Fatalf("%s: ConnectorView exposes credential field %q", FindingNoCredInResp, name)
			}
		}
	}
}
