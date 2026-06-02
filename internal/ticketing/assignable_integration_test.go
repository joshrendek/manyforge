//go:build integration

package ticketing

import (
	"testing"
)

// TestListAssignableMembers (manyforge-64s) — the endpoint that powers the assignee
// picker returns the business's human members ordered by display name. seedReadTenant
// seeds three human members of rt.master: owner ("O"), reader ("R"), and noReader
// ("N"). All three are MEMBERS, so all three are assignable (eligibility is membership,
// not permission); they come back ordered N, O, R with emails populated.
func TestListAssignableMembers(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	svc := newReadService(tdb)

	members, err := svc.ListAssignableMembers(ctx, rt.reader, rt.master, 50)
	if err != nil {
		t.Fatalf("ListAssignableMembers: %v", err)
	}
	if len(members) != 3 {
		t.Fatalf("got %d assignable members, want 3 (owner, reader, noReader)", len(members))
	}
	gotOrder := []string{members[0].DisplayName, members[1].DisplayName, members[2].DisplayName}
	wantOrder := []string{"N", "O", "R"}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Errorf("display-name order = %v, want %v (by display_name asc)", gotOrder, wantOrder)
			break
		}
	}
	for _, m := range members {
		if m.Email == "" {
			t.Errorf("member %q has empty email — the account join did not populate it", m.DisplayName)
		}
		if m.ID.String() == "00000000-0000-0000-0000-000000000000" {
			t.Errorf("member %q has nil principal id", m.DisplayName)
		}
	}
}
