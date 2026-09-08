package manyforge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestNullableEnumPresenceRoundTrip(t *testing.T) {
	var patch PatchTicket
	if err := json.Unmarshal([]byte(`{"priority":null,"status":null,"tags":[],"assignee_principal_id":null}`), &patch); err != nil {
		t.Fatal(err)
	}
	if !patch.Priority.IsNull() || !patch.Status.IsNull() || !patch.AssigneePrincipalId.IsNull() {
		t.Fatal("explicit null fields lost their presence")
	}
	encoded, err := json.Marshal(patch)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["priority"]) != "null" || string(fields["tags"]) != "[]" {
		t.Fatalf("nullable enum or empty replacement array changed on the wire: %s", encoded)
	}
	if err := json.Unmarshal([]byte(`{}`), &patch); err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(patch)
	if err != nil || string(encoded) != "{}" {
		t.Fatalf("omitted fields must remain absent: %s, %v", encoded, err)
	}
}

func TestMissingTerminalCursorEndsIteration(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithAccessToken("fixture-access"))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	iterator := client.Business("fixture-business").Contacts.Iter(context.Background(), BusinessContactsListParams{Limit: 1})
	if iterator.Next() || iterator.Err() != nil {
		t.Fatalf("an omitted terminal cursor must end a valid empty page: %v", iterator.Err())
	}
	if calls.Load() != 1 {
		t.Fatalf("terminal page caused additional HTTP requests: %d", calls.Load())
	}
}
