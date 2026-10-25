//go:build integration

package ticketing

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/platform/httpx"
)

func TestBusinessOperatorSharedRequesterHTTP(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	foreign := seedReadTenant(ctx, t, tdb)
	rootTicket := uuid.New()
	seedTicket(ctx, t, tdb, rt, rootTicket, "open", "normal", "Sibling private ticket", nil, nil, -time.Hour)
	foreignTicket := uuid.New()
	seedTicket(ctx, t, tdb, foreign, foreignTicket, "open", "normal", "Foreign tenant", nil, nil, -time.Hour)
	child := uuid.New()
	first, second := uuid.New(), uuid.New()
	portalTestSQL(t, ctx, tdb, `INSERT INTO business(id,parent_id,tenant_root_id,name,status) VALUES($1,$2,$2,'Support business','active')`, child, rt.master)
	portalTestSQL(t, ctx, tdb, `INSERT INTO business_closure(ancestor_id,descendant_id,depth,tenant_root_id) VALUES($1,$1,0,$2),($2,$1,1,$2)`, child, rt.master)
	portalTestSQL(t, ctx, tdb, `DELETE FROM membership WHERE principal_id=$1 AND business_id=$2`, rt.reader, rt.master)
	portalTestSQL(t, ctx, tdb, `INSERT INTO membership(principal_id,business_id,tenant_root_id,role_id) VALUES($1,$2,$3,$4)`, rt.reader, child, rt.master, presetRole(ctx, t, tdb, "member"))
	contact := uuid.New()
	portalTestSQL(t, ctx, tdb, `INSERT INTO contact(id,tenant_root_id,primary_email,display_name,created_at,updated_at) VALUES($1,$2,'sibling-crm@example.test','PRIVATE CRM',now(),now())`, contact, rt.master)
	portalTestSQL(t, ctx, tdb, `UPDATE requester SET contact_id=$2 WHERE id=$1`, rt.requester, contact)
	portalTestSQL(t, ctx, tdb, `UPDATE requester SET display_name='SIBLING DIRECTORY SECRET', first_seen_at=now()-interval '30 days',last_seen_at=now()+interval '1 day' WHERE id=$1`, rt.requester)
	for _, id := range []uuid.UUID{first, second} {
		portalTestSQL(t, ctx, tdb, `INSERT INTO ticket(id,business_id,tenant_root_id,requester_id,subject,reply_token) VALUES($1,$2,$3,$4,'Child support',$5)`, id, child, rt.master, rt.requester, "token-"+id.String())
	}
	// Use real signed authentication and permission middleware for both reads and replies.
	_, ring := newTicketReadRouter(t, tdb)
	resolve := func(ctx context.Context, tx pgx.Tx, pid, bid uuid.UUID) (httpx.Permissions, error) {
		return authz.Resolve(ctx, tx, pid, bid)
	}
	svc := &Service{DB: tdb.App, ReplyTokenKey: replyKey, SystemDomain: "support.test"}
	h := NewHandler(svc, tdb.App, resolve)
	router := chi.NewRouter()
	router.Use(httpx.AuthToPrincipal(ring))
	router.Use(httpx.RequireAuth)
	gate := func(permission string) func(http.Handler) http.Handler {
		return httpx.RequirePermission(tdb.App, resolve, permission, func(r *http.Request) (uuid.UUID, error) { return uuid.Parse(chi.URLParam(r, "id")) })
	}
	router.Group(func(r chi.Router) { r.Use(gate("tickets.read")); h.ProtectedRoutes(r) })
	router.Group(func(r chi.Router) { r.Use(gate("tickets.reply")); h.WriteRoutes(r) })
	server := httptest.NewServer(router)
	defer server.Close()
	bearer := mintBearer(t, ring, rt.reader)
	base := "/businesses/" + child.String()
	status, raw := getRaw(t, server, bearer, base+"/tickets/"+first.String())
	if status != 200 {
		t.Fatalf("child ticket HTTP%d: %s", status, raw)
	}
	var ticket ticketResp
	if err := json.Unmarshal([]byte(raw), &ticket); err != nil {
		t.Fatal(err)
	}
	if ticket.Requester.ID != rt.requester.String() || ticket.Requester.Email != "ada@example.com" {
		t.Fatal("authorized ticket notification identity missing")
	}
	if ticket.Requester.DisplayName != nil || ticket.Requester.ContactID != nil || strings.Contains(raw, "SIBLING DIRECTORY SECRET") || ticket.Requester.FirstSeenAt != ticket.CreatedAt {
		t.Fatal("sibling directory metadata leaked")
	}
	status, raw = getRaw(t, server, bearer, base+"/tickets?limit=1")
	if status != 200 {
		t.Fatalf("list HTTP%d: %s", status, raw)
	}
	var page struct {
		Items      []ticketResp `json:"items"`
		NextCursor *string      `json:"next_cursor"`
	}
	if err := json.Unmarshal([]byte(raw), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.NextCursor == nil {
		t.Fatalf("missing child ticket page: %s", raw)
	}
	prior := page.Items[0].ID
	status, raw = getRaw(t, server, bearer, base+"/tickets?limit=1&cursor="+*page.NextCursor)
	if status != 200 {
		t.Fatalf("continuation HTTP%d", status)
	}
	if err := json.Unmarshal([]byte(raw), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ID == prior {
		t.Fatal("shared requester duplicated or hid a ticket")
	}
	for _, path := range []string{
		"/businesses/" + rt.master.String() + "/tickets/" + rootTicket.String(),
		"/businesses/" + foreign.master.String() + "/tickets/" + foreignTicket.String(),
		base + "/tickets/" + rootTicket.String(),
		base + "/requesters/" + rt.requester.String(),
	} {
		if code, _ := getRaw(t, server, bearer, path); code != 404 {
			t.Fatalf("foreign path HTTP%d", code)
		}
	}
	req, _ := http.NewRequest("POST", server.URL+base+"/tickets/"+first.String()+"/reply", strings.NewReader(`{"body_text":"Authorized business-only reply"}`))
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	response.Body.Close()
	if response.StatusCode != 201 {
		t.Fatalf("reply HTTP%d: %s", response.StatusCode, body)
	}
	if countSuper(ctx, t, tdb.Super, `SELECT count(*) FROM ticket_message WHERE ticket_id=$1 AND direction='outbound'`, first) != 1 {
		t.Fatal("reply not persisted")
	}
	if err := tdb.App.WithPrincipal(ctx, rt.reader, func(tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM requester WHERE id=$1`, rt.requester).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatal("requester directory RLS widened")
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ticket_requester_context WHERE ticket_id IN($1,$2)`, rootTicket, foreignTicket).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatal("foreign ticket exposed through requester context")
		}
		changed, err := tx.Exec(ctx, `UPDATE requester SET display_name='overwrite' WHERE id=$1`, rt.requester)
		if err != nil {
			return err
		}
		if changed.RowsAffected() != 0 {
			t.Fatal("sibling requester directory became writable")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := tdb.App.WithPrincipal(ctx, rt.noReader, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ticket_requester_context WHERE business_id=$1`, child).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Fatal("principal without support permission read projection")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := tdb.App.WithPrincipal(ctx, rt.reader, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE ticket_requester_context SET display_name='overwrite' WHERE ticket_id=$1`, first)
		return err
	}); err == nil {
		t.Fatal("requester projection is writable")
	}
	portalTestSQL(t, ctx, tdb, `UPDATE ticket SET redacted_at=now() WHERE id=$1`, first)
	if code, _ := getRaw(t, server, bearer, base+"/tickets/"+first.String()); code != 404 {
		t.Fatal("redacted ticket exposed")
	}
}
