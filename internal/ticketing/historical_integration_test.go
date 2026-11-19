//go:build integration

package ticketing

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/blob"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/events"
	"github.com/manyforge/manyforge/internal/platform/notify"
)

func TestHistoricalTicketAndAttachmentVisibility(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	store, err := blob.Open(ctx, "file://"+t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service := &PortalService{DB: tdb.App, Resolve: portalTestResolver, ReplyTokenKey: replyKey, SystemDomain: "example.com", Blob: store}
	staff := &Service{DB: tdb.App, Blob: store}
	created := time.Date(2021, 3, 14, 12, 0, 0, 0, time.UTC)
	updated := created.Add(time.Hour)
	resolution := "Example resolution"
	public := AttachmentInput{Filename: "example.txt", Content: []byte("Synthetic public attachment")}
	archive := AttachmentInput{Filename: "example.bin", Content: bytes.Repeat([]byte{0}, 11<<20)}
	snapshot := HistoricalTicket{
		Source: "fixture", ID: "example-42", Subject: "Synthetic conversation",
		Status: "solved", Priority: "normal", SourceStatus: "Example state", SourceResolution: &resolution,
		CreatedAt: created, UpdatedAt: updated, RequesterEmail: "fixture@example.com", RequesterName: "Example Customer",
		Owner: &HistoricalOwner{Namespace: "example-app", Subject: "42"},
		Messages: []HistoricalMessage{
			{ID: "public-in", Direction: "inbound", Body: "Synthetic request", CreatedAt: updated, UpdatedAt: updated,
				AuthorID: "customer-42", AuthorName: "Example Customer", AuthorKind: "customer",
				Attachments: []HistoricalAttachment{{AttachmentInput: public, ID: "public-file", CreatedAt: created, CustomerVisible: true}}},
			{ID: "public-out", Direction: "outbound", Body: "Synthetic answer", CreatedAt: updated, UpdatedAt: updated,
				AuthorID: "staff-24", AuthorName: "Example Agent", AuthorKind: "staff",
				Attachments: []HistoricalAttachment{{AttachmentInput: archive, ID: "large-file", CreatedAt: updated, CustomerVisible: true}}},
			{ID: "private", Direction: "note", Body: "Synthetic note", CreatedAt: updated, UpdatedAt: updated,
				AuthorName: "Example Agent", AuthorKind: "staff",
				Attachments: []HistoricalAttachment{{AttachmentInput: public, ID: "private-file", CreatedAt: updated, CustomerVisible: true}}},
		},
	}
	result, err := service.ImportHistoricalTicket(ctx, rt.owner, rt.master, snapshot)
	if err != nil || !result.Created {
		t.Fatalf("import: %+v, %v", result, err)
	}
	id := result.TicketID
	replayed, err := service.ImportHistoricalTicket(ctx, rt.owner, rt.master, snapshot)
	if err != nil || replayed.Created || replayed.TicketID != id {
		t.Fatalf("replay: %+v, %v", replayed, err)
	}
	snapshot.Subject += " changed"
	if _, err := service.ImportHistoricalTicket(ctx, rt.owner, rt.master, snapshot); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("changed replay: %v", err)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM outbox WHERE tenant_root_id=$1", rt.master); n != 0 {
		t.Fatalf("history emitted %d events", n)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM audit_entry WHERE target_id=$1 AND action='ticket.history_imported'", id); n != 1 {
		t.Fatalf("history has %d audit entries", n)
	}
	var gotStatus, gotResolution, author string
	var gotCreated, gotUpdated time.Time
	err = tdb.Super.QueryRow(ctx, `
		SELECT t.historical_metadata->>'status',t.historical_metadata->>'resolution',m.historical_metadata->>'author_name',t.created_at,t.updated_at
		FROM ticket t JOIN ticket_message m ON m.ticket_id=t.id WHERE t.id=$1 AND m.historical_id='public-out'`, id).
		Scan(&gotStatus, &gotResolution, &author, &gotCreated, &gotUpdated)
	if err != nil || gotStatus != snapshot.SourceStatus || gotResolution != resolution || author != "Example Agent" ||
		!gotCreated.Equal(created) || !gotUpdated.Equal(updated) {
		t.Fatalf("history provenance: %s %s %s %s %s, %v", gotStatus, gotResolution, author, gotCreated, gotUpdated, err)
	}
	key, err := service.CreateKeyForNamespace(ctx, rt.owner, rt.master, "Example integration", 1, "example-app")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := service.Authenticate(ctx, key.Token, "42", "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := service.Authenticate(ctx, key.Token, "43", "")
	if err != nil {
		t.Fatal(err)
	}
	thread, err := service.Thread(ctx, owner, id, "")
	if err != nil || len(thread.Messages) != 2 {
		t.Fatalf("public thread: %+v, %v", thread, err)
	}
	if thread.Messages[0].Body != snapshot.Messages[1].Body || thread.Messages[1].Body != snapshot.Messages[0].Body {
		t.Fatal("portal thread is not in newest-first source order")
	}
	var publicID uuid.UUID
	for _, message := range thread.Messages {
		switch message.Direction {
		case "inbound":
			if len(message.Attachments) != 1 {
				t.Fatalf("missing public file: %+v", message)
			}
			publicID = message.Attachments[0].ID
		case "outbound":
			if len(message.Attachments) != 0 {
				t.Fatal("unsupported archive file visible in portal")
			}
		default:
			t.Fatal("private note visible in portal")
		}
	}
	_, content, err := service.DownloadAttachment(ctx, owner, id, publicID)
	checkDownloadedSupportFile(t, content, err, public.Content)
	if _, _, err := service.DownloadAttachment(ctx, other, id, publicID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("other customer download: %v", err)
	}
	router := chi.NewRouter()
	NewPortalHandler(service, nil).CustomerRoutes(router)
	download := func(customer string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/support/portal/tickets/"+id.String()+"/attachments/"+publicID.String(), nil)
		request.Header.Set("Authorization", "Bearer "+key.Token)
		request.Header.Set("X-MF-External-User", customer)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}
	response := download("42")
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), public.Content) ||
		response.Header().Get("Content-Type") != "application/octet-stream" ||
		response.Header().Get("Cache-Control") != "private, no-store" ||
		!strings.HasPrefix(response.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatalf("unexpected authenticated attachment response: %d", response.Code)
	}
	if response := download("43"); response.Code != http.StatusNotFound {
		t.Fatalf("other customer attachment request returned %d", response.Code)
	}
	staffMessages, err := staff.ListMessages(ctx, rt.reader, rt.master, id, "", 10)
	if err != nil || len(staffMessages.Items) != 3 {
		t.Fatalf("staff thread: %+v, %v", staffMessages, err)
	}
	for i, message := range staffMessages.Items {
		if message.BodyText == nil || *message.BodyText != snapshot.Messages[i].Body {
			t.Fatalf("message %d is out of source order", i)
		}
		if message.Direction != "inbound" && message.HistoricalAuthorName != "Example Agent" {
			t.Fatalf("missing historical author on %s message", message.Direction)
		}
	}
	for name, want := range map[string][]byte{"private-file": public.Content, "large-file": archive.Content} {
		var fileID uuid.UUID
		err := tdb.Super.QueryRow(ctx, `
			SELECT a.id FROM attachment a JOIN ticket_message m ON m.id=a.ticket_message_id
			WHERE m.ticket_id=$1 AND a.historical_id=$2`, id, name).Scan(&fileID)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := service.DownloadAttachment(ctx, owner, id, fileID); !errors.Is(err, errs.ErrNotFound) {
			t.Fatalf("restricted download: %v", err)
		}
		_, content, err := staff.DownloadAttachment(ctx, rt.reader, rt.master, id, fileID)
		checkDownloadedSupportFile(t, content, err, want)
	}
	if err := staff.RedactTicket(ctx, rt.owner, rt.master, id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := staff.DownloadAttachment(ctx, rt.reader, rt.master, id, publicID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("download after redaction: %v", err)
	}
	if n := countSuper(ctx, t, tdb.Super, "SELECT count(*) FROM ticket_message WHERE ticket_id=$1 AND historical_metadata IS NOT NULL", id); n != 0 {
		t.Fatalf("redaction preserved metadata on %d messages", n)
	}
}

func TestLiveSupportAttachmentsRoundTrip(t *testing.T) {
	ctx, tdb := startReadDB(t)
	rt := seedReadTenant(ctx, t, tdb)
	portalTestSender(t, ctx, tdb, rt)
	portalTestSQL(t, ctx, tdb, `INSERT INTO inbound_address(business_id,tenant_root_id,address,kind) VALUES($1,$1,'support@inbound.example.com','system')`, rt.master)
	store, err := blob.Open(ctx, "file://"+t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	mailer := &portalTestMailer{}
	service := &PortalService{DB: tdb.App, Resolve: portalTestResolver, Mailer: mailer, ReplyTokenKey: replyKey, SystemDomain: "example.com", Blob: store}
	staff := &Service{DB: tdb.App, ReplyTokenKey: replyKey, SystemDomain: "example.com", Blob: store}
	key, err := service.CreateKeyForNamespace(ctx, rt.owner, rt.master, "Example integration", 1, "example-app")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := service.Authenticate(ctx, key.Token, "42", "")
	if err != nil {
		t.Fatal(err)
	}
	portalTestVerify(t, ctx, service, owner, mailer, "fixture@example.com")
	files := []AttachmentInput{{Filename: "example.txt", Content: []byte("Synthetic attachment")}}
	keyID := uuid.New()
	ticket, err := service.CreateTicketWithAttachments(ctx, owner, "Example request", "Synthetic message", keyID, files)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := service.CreateTicketWithAttachments(ctx, owner, "Example request", "Synthetic message", keyID, files); err != nil || again.ID != ticket.ID {
		t.Fatalf("upload retry: %+v, %v", again, err)
	}
	changed := []AttachmentInput{{Filename: files[0].Filename, Content: []byte("Changed contents")}}
	if _, err := service.CreateTicketWithAttachments(ctx, owner, "Example request", "Synthetic message", keyID, changed); !errors.Is(err, ErrPortalConflict) {
		t.Fatalf("changed upload retry: %v", err)
	}
	if message, err := service.AddMessageWithAttachments(ctx, owner, ticket.ID, "Synthetic follow-up", uuid.New(), files); err != nil || len(message.Attachments) != 1 {
		t.Fatalf("public follow-up with file: %+v, %v", message, err)
	}
	html := "<p>Synthetic reply</p>"
	reply, err := staff.Reply(ctx, rt.reader, rt.master, ticket.ID, ReplyInput{BodyText: "Synthetic reply", BodyHTML: &html, Attachments: files})
	if err != nil || len(reply.Attachments) != 1 {
		t.Fatalf("staff reply: %+v, %v", reply, err)
	}
	_, content, err := service.DownloadAttachment(ctx, owner, ticket.ID, reply.Attachments[0].ID)
	checkDownloadedSupportFile(t, content, err, files[0].Content)
	event := events.Event{TenantRootID: rt.master, Topic: events.TopicTicketReplied}
	var payload json.RawMessage
	if err := tdb.Super.QueryRow(ctx, "SELECT payload FROM outbox WHERE tenant_root_id=$1 AND topic=$2", event.TenantRootID, event.Topic).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	event.Payload = payload
	sender := notify.SendSubscriber{ProfileSender: mailer, Blob: store}
	send := func(tx pgx.Tx) error { return sender.Handle(ctx, tx, event) }
	if err := tdb.App.WithTx(ctx, send); err != nil {
		t.Fatal(err)
	}
	if err := tdb.App.WithTx(ctx, send); err != nil {
		t.Fatal(err)
	}
	if len(mailer.mails) != 2 || len(mailer.mails[1].Attachments) != 1 {
		t.Fatalf("expected one verification and one reply with a file, got %d mail(s)", len(mailer.mails))
	}
	testSupportAttachmentMIME(t, mailer.mails[1], files[0])
}

func checkDownloadedSupportFile(t *testing.T, reader io.ReadCloser, downloadErr error, want []byte) {
	t.Helper()
	if downloadErr != nil {
		t.Fatalf("download: %v", downloadErr)
	}
	defer func() { _ = reader.Close() }()
	content, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(content, want) {
		t.Fatalf("unexpected downloaded bytes: %v", err)
	}
}

func testSupportAttachmentMIME(t *testing.T, reply notify.Mail, file AttachmentInput) {
	t.Helper()
	raw, err := notify.BuildMIME(reply)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/mixed" {
		t.Fatalf("mixed mail: %s, %v", mediaType, err)
	}
	reader := multipart.NewReader(msg.Body, params["boundary"])
	body, err := reader.NextPart()
	if err != nil || !strings.HasPrefix(body.Header.Get("Content-Type"), "multipart/alternative") {
		t.Fatalf("reply body MIME: %v", err)
	}
	part, err := reader.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, part))
	if err != nil || part.FileName() != file.Filename || !bytes.Equal(content, file.Content) {
		t.Fatalf("reply attachment MIME: %v", err)
	}
}
