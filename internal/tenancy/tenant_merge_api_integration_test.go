//go:build integration

package tenancy_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/manyforge/manyforge/internal/platform/auth"
	mfblob "github.com/manyforge/manyforge/internal/platform/blob"
	"github.com/manyforge/manyforge/internal/platform/db/testdb"
	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/tenancy"
)

const tenantMergeTestPassword = "tenant-merge-test-password"

type tenantMergeMemoryBlob struct {
	values       map[string][]byte
	contentTypes map[string]string
}

func newTenantMergeMemoryBlob() *tenantMergeMemoryBlob {
	return &tenantMergeMemoryBlob{
		values:       map[string][]byte{},
		contentTypes: map[string]string{},
	}
}

func (s *tenantMergeMemoryBlob) Put(
	_ context.Context,
	key string,
	content []byte,
	contentType string,
) error {
	s.values[key] = append([]byte(nil), content...)
	s.contentTypes[key] = contentType
	return nil
}

func (s *tenantMergeMemoryBlob) Get(
	_ context.Context,
	key string,
) ([]byte, error) {
	content, ok := s.values[key]
	if !ok {
		return nil, fmt.Errorf("blob %q not found", key)
	}
	return append([]byte(nil), content...), nil
}

func (s *tenantMergeMemoryBlob) Delete(
	_ context.Context,
	key string,
) error {
	delete(s.values, key)
	delete(s.contentTypes, key)
	return nil
}

func (s *tenantMergeMemoryBlob) Close() error { return nil }

func tenantMergeTestRouter(
	t *testing.T,
	svc *tenancy.Service,
) (*chi.Mux, *auth.KeyRing) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	ring, err := auth.NewKeyRing(
		"manyforge", "manyforge-api", "merge-test", privateKey,
		map[string]ed25519.PublicKey{"merge-test": publicKey},
	)
	if err != nil {
		t.Fatalf("key ring: %v", err)
	}
	router := httpx.NewRouter(ring)
	handler := tenancy.NewHandler(svc)
	router.Route("/api/v1", func(r chi.Router) {
		r.Use(httpx.RequireAuth)
		handler.TenantMergeRoutes(r)
	})
	return router, ring
}

func tenantMergeToken(
	t *testing.T,
	ring *auth.KeyRing,
	principalID uuid.UUID,
) string {
	t.Helper()
	token, err := ring.Sign(principalID, time.Hour, time.Now())
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return token
}

func tenantMergeRequest(
	t *testing.T,
	router http.Handler,
	token, method, path, idempotencyKey string,
	body any,
) (int, []byte) {
	t.Helper()
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("encode request: %v", err)
		}
	}
	request := httptest.NewRequest(
		method, "/api/v1"+path, bytes.NewReader(encoded),
	)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response.Code, response.Body.Bytes()
}

func decodeTenantMergeResponse(
	t *testing.T,
	body []byte,
) tenancy.TenantMergeOperation {
	t.Helper()
	var operation tenancy.TenantMergeOperation
	if err := json.Unmarshal(body, &operation); err != nil {
		t.Fatalf("decode operation %s: %v", body, err)
	}
	return operation
}

func decodeTenantMergeError(
	t *testing.T,
	body []byte,
) httpx.ErrorBody {
	t.Helper()
	var response httpx.ErrorBody
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode error %s: %v", body, err)
	}
	return response
}

func TestTenantMergeAPIConfirmationStatusAndManifest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	tdb, err := testdb.Start(ctx)
	if err != nil {
		t.Fatalf("start testdb: %v", err)
	}
	t.Cleanup(func() { tdb.Close(context.Background()) })
	blobStore := newTenantMergeMemoryBlob()
	svc := &tenancy.Service{DB: tdb.App, Blob: blobStore}
	router, ring := tenantMergeTestRouter(t, svc)

	actor, sourceRoot := seedFounder(
		ctx, t, tdb, "merge-api-source@x.test",
	)
	outsider, _ := seedFounder(
		ctx, t, tdb, "merge-api-outsider@x.test",
	)
	_, destinationRoot := seedFounder(
		ctx, t, tdb, "merge-api-destination@x.test",
	)
	addDirectOwner(ctx, t, tdb, actor, destinationRoot)
	destinationParent, err := svc.CreateSubBusiness(
		ctx, actor, destinationRoot, "Destination company",
	)
	if err != nil {
		t.Fatalf("create destination parent: %v", err)
	}
	archivedDestination, err := svc.CreateSubBusiness(
		ctx, actor, destinationRoot, "Archived destination",
	)
	if err != nil {
		t.Fatalf("create archived destination: %v", err)
	}
	if err := svc.Archive(ctx, actor, archivedDestination.ID); err != nil {
		t.Fatalf("archive destination: %v", err)
	}
	archivedSource, err := svc.CreateMasterBusiness(
		ctx, actor, "Archived source master",
	)
	if err != nil {
		t.Fatalf("create archived source: %v", err)
	}
	if err := svc.Archive(ctx, actor, archivedSource.ID); err != nil {
		t.Fatalf("archive source: %v", err)
	}
	if _, err := tdb.Super.Exec(ctx,
		"UPDATE business SET name='Source company' WHERE id=$1",
		sourceRoot,
	); err != nil {
		t.Fatalf("name source: %v", err)
	}

	requesterID := uuid.New()
	ticketID := uuid.New()
	messageID := uuid.New()
	attachmentID := uuid.New()
	attachmentContent := []byte("tenant merge attachment bytes")
	sourceBlobKey := mfblob.Key(
		sourceRoot, sourceRoot, ticketID, attachmentID,
	)
	destinationBlobKey := mfblob.Key(
		destinationRoot, sourceRoot, ticketID, attachmentID,
	)
	execTenantMergeSeed(ctx, t, tdb, "seed attachment",
		tenantMergeSeedStatement{sql: `
			INSERT INTO requester (
			    id, business_id, tenant_root_id, email
			) VALUES ($1, $2, $2, 'merge-attachment@x.test')`,
			args: []any{requesterID, sourceRoot}},
		tenantMergeSeedStatement{sql: `
			INSERT INTO ticket (
			    id, business_id, tenant_root_id, requester_id, subject,
			    reply_token
			) VALUES ($1, $2, $2, $3, 'Merge attachment', $4)`,
			args: []any{
				ticketID, sourceRoot, requesterID,
				"merge-reply-" + ticketID.String(),
			}},
		tenantMergeSeedStatement{sql: `
			INSERT INTO ticket_message (
			    id, ticket_id, business_id, tenant_root_id, direction,
			    message_id, body_text
			) VALUES ($1, $2, $3, $3, 'inbound', $4, 'attachment')`,
			args: []any{
				messageID, ticketID, sourceRoot,
				"<merge-" + messageID.String() + "@x.test>",
			}},
		tenantMergeSeedStatement{sql: `
			INSERT INTO attachment (
			    id, ticket_message_id, business_id, tenant_root_id,
			    blob_key, filename, content_type, size
			) VALUES (
			    $1, $2, $3, $3, $4, 'merge.txt', 'text/plain', $5
			)`,
			args: []any{
				attachmentID, messageID, sourceRoot, sourceBlobKey,
				len(attachmentContent),
			}},
	)
	if err := blobStore.Put(
		ctx, sourceBlobKey, attachmentContent, "text/plain",
	); err != nil {
		t.Fatalf("seed attachment blob: %v", err)
	}
	passwordHash, err := auth.HashPassword(tenantMergeTestPassword)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := tdb.Super.Exec(ctx, `
		UPDATE account
		SET password_hash = $2
		WHERE id = (
		    SELECT account_id FROM principal WHERE id = $1
		)`,
		actor, passwordHash,
	); err != nil {
		t.Fatalf("set actor password: %v", err)
	}

	actorToken := tenantMergeToken(t, ring, actor)
	outsiderToken := tenantMergeToken(t, ring, outsider)
	optionsStatus, optionsBody := tenantMergeRequest(
		t, router, actorToken, http.MethodGet, "/tenant-merge-options", "", nil,
	)
	var optionsResponse struct {
		Sources []tenancy.TenantMergeSourceOptions `json:"sources"`
	}
	if err := json.Unmarshal(optionsBody, &optionsResponse); err != nil {
		t.Fatalf("decode merge options %s: %v", optionsBody, err)
	}
	if optionsStatus != http.StatusOK {
		t.Fatalf("merge options status=%d body=%s", optionsStatus, optionsBody)
	}
	var sourceOptions *tenancy.TenantMergeSourceOptions
	for index := range optionsResponse.Sources {
		if optionsResponse.Sources[index].SourceRootID == archivedSource.ID {
			t.Fatalf("merge options include archived source: %+v",
				optionsResponse.Sources[index])
		}
		if optionsResponse.Sources[index].SourceRootID == sourceRoot {
			sourceOptions = &optionsResponse.Sources[index]
			break
		}
	}
	if sourceOptions == nil {
		t.Fatalf("merge options omit eligible source %s: %+v",
			sourceRoot, optionsResponse.Sources)
	}
	foundDestinationParent := false
	for _, destination := range sourceOptions.Destinations {
		if destination.TenantRootID == sourceRoot ||
			destination.TenantRootID == archivedDestination.TenantRootID &&
				destination.ID == archivedDestination.ID {
			t.Fatalf("merge options include same-root or archived destination: %+v",
				destination)
		}
		if destination.ID == destinationParent.ID {
			foundDestinationParent = true
			if destination.TenantRootID != destinationRoot ||
				destination.TenantRootName == "" ||
				!strings.HasSuffix(
					destination.HierarchyPath,
					" / Destination company",
				) {
				t.Fatalf("destination option lacks root/path labels: %+v",
					destination)
			}
		}
	}
	if !foundDestinationParent {
		t.Fatalf("merge options omit destination parent %s: %+v",
			destinationParent.ID, sourceOptions.Destinations)
	}

	outsiderOptionsStatus, outsiderOptionsBody := tenantMergeRequest(
		t, router, outsiderToken, http.MethodGet, "/tenant-merge-options", "", nil,
	)
	var outsiderOptions struct {
		Sources []tenancy.TenantMergeSourceOptions `json:"sources"`
	}
	if err := json.Unmarshal(outsiderOptionsBody, &outsiderOptions); err != nil {
		t.Fatalf("decode outsider merge options %s: %v", outsiderOptionsBody, err)
	}
	if outsiderOptionsStatus != http.StatusOK || len(outsiderOptions.Sources) != 0 {
		t.Fatalf("single-root owner options = status %d response %+v",
			outsiderOptionsStatus, outsiderOptions.Sources)
	}

	createPath := "/businesses/" + sourceRoot.String() + "/tenant-merges"
	createBody := map[string]any{
		"destination_parent_id": destinationParent.ID,
	}
	status, body := tenantMergeRequest(
		t, router, actorToken, http.MethodPost, createPath,
		"tenant-merge-forbidden-root", map[string]any{
			"destination_parent_id": destinationParent.ID,
			"tenant_root_id":        destinationRoot,
		},
	)
	if response := decodeTenantMergeError(t, body); status != http.StatusBadRequest ||
		response.Code != "VALIDATION" {
		t.Fatalf("caller-supplied tenant root = status %d response %+v",
			status, response)
	}
	status, body = tenantMergeRequest(
		t, router, actorToken, http.MethodPost, createPath,
		"tenant-merge-api-success", createBody,
	)
	if status != http.StatusCreated {
		t.Fatalf("create/preflight status=%d body=%s", status, body)
	}
	operation := decodeTenantMergeResponse(t, body)
	if operation.Status != "ready" || operation.CorrelationID == uuid.Nil {
		t.Fatalf("created operation status/correlation = %q/%s conflicts=%+v",
			operation.Status, operation.CorrelationID, operation.Conflicts)
	}
	if operation.AttachmentCount != 1 ||
		operation.AttachmentBytes != int64(len(attachmentContent)) {
		t.Fatalf("attachment preflight count/bytes = %d/%d",
			operation.AttachmentCount, operation.AttachmentBytes)
	}

	status, body = tenantMergeRequest(
		t, router, actorToken, http.MethodPost, createPath,
		"tenant-merge-api-success", createBody,
	)
	replay := decodeTenantMergeResponse(t, body)
	if status != http.StatusCreated || replay.ID != operation.ID {
		t.Fatalf("idempotent create = status %d id %s, want %s",
			status, replay.ID, operation.ID)
	}

	statusPath := "/tenant-merges/" + operation.ID.String()
	unknownPath := "/tenant-merges/" + uuid.NewString()
	unknownStatus, unknownBody := tenantMergeRequest(
		t, router, outsiderToken, http.MethodGet, unknownPath, "", nil,
	)
	hiddenStatus, hiddenBody := tenantMergeRequest(
		t, router, outsiderToken, http.MethodGet, statusPath, "", nil,
	)
	if unknownStatus != http.StatusNotFound ||
		hiddenStatus != http.StatusNotFound ||
		!reflect.DeepEqual(unknownBody, hiddenBody) {
		t.Fatalf("unknown/hidden status/body = %d %q / %d %q",
			unknownStatus, unknownBody, hiddenStatus, hiddenBody)
	}

	confirmPath := statusPath + "/confirm"
	confirmBody := map[string]any{
		"source_name":      "Source company",
		"destination_name": "Destination company",
		"password":         tenantMergeTestPassword,
	}
	if _, err := tdb.Super.Exec(ctx, `
		DELETE FROM membership
		WHERE principal_id = $1
		  AND business_id = $2
		  AND tenant_root_id = $2`,
		actor, destinationRoot,
	); err != nil {
		t.Fatalf("remove destination ownership: %v", err)
	}
	status, body = tenantMergeRequest(
		t, router, actorToken, http.MethodPost, confirmPath, "", confirmBody,
	)
	if response := decodeTenantMergeError(t, body); status != http.StatusNotFound ||
		response.Code != "NOT_FOUND" {
		t.Fatalf("lost direct ownership = status %d response %+v", status, response)
	}
	addDirectOwner(ctx, t, tdb, actor, destinationRoot)
	status, body = tenantMergeRequest(
		t, router, actorToken, http.MethodPost, statusPath+"/preflight", "", nil,
	)
	if refreshed := decodeTenantMergeResponse(t, body); status != http.StatusOK ||
		refreshed.Status != "ready" {
		t.Fatalf("preflight after owner restore = status %d operation=%+v",
			status, refreshed)
	}

	confirmBody["password"] = "wrong-password"
	status, body = tenantMergeRequest(
		t, router, actorToken, http.MethodPost, confirmPath, "", confirmBody,
	)
	if response := decodeTenantMergeError(t, body); status != http.StatusUnauthorized ||
		response.Code != "REAUTHENTICATION_FAILED" {
		t.Fatalf("wrong password = status %d response %+v", status, response)
	}

	confirmBody["password"] = tenantMergeTestPassword
	confirmBody["source_name"] = "source company"
	status, body = tenantMergeRequest(
		t, router, actorToken, http.MethodPost, confirmPath, "", confirmBody,
	)
	if response := decodeTenantMergeError(t, body); status != http.StatusBadRequest ||
		response.Code != "VALIDATION" {
		t.Fatalf("mistyped name = status %d response %+v", status, response)
	}

	if _, err := tdb.Super.Exec(ctx,
		"UPDATE business SET name='Renamed source company' WHERE id=$1",
		sourceRoot,
	); err != nil {
		t.Fatalf("stale source: %v", err)
	}
	confirmBody["source_name"] = "Source company"
	status, body = tenantMergeRequest(
		t, router, actorToken, http.MethodPost, confirmPath, "", confirmBody,
	)
	if response := decodeTenantMergeError(t, body); status != http.StatusPreconditionFailed ||
		response.Code != "STALE_PREFLIGHT" {
		t.Fatalf("stale confirmation = status %d response %+v", status, response)
	}
	stale, err := svc.GetTenantMergeOperation(ctx, actor, operation.ID)
	if err != nil || stale.Status != "preflight_required" {
		t.Fatalf("stale durable status=%q err=%v", stale.Status, err)
	}

	status, body = tenantMergeRequest(
		t, router, actorToken, http.MethodPost, statusPath+"/preflight", "", nil,
	)
	current := decodeTenantMergeResponse(t, body)
	if status != http.StatusOK || current.Status != "ready" {
		t.Fatalf("fresh preflight = status %d operation=%+v", status, current)
	}

	confirmBody["source_name"] = "Renamed source company"
	status, body = tenantMergeRequest(
		t, router, actorToken, http.MethodPost, confirmPath, "", confirmBody,
	)
	succeeded := decodeTenantMergeResponse(t, body)
	if status != http.StatusOK || succeeded.Status != "succeeded" {
		t.Fatalf("confirm = status %d operation=%+v body=%s",
			status, succeeded, body)
	}
	if succeeded.Manifest == nil ||
		succeeded.Manifest.OperationID != operation.ID ||
		succeeded.Manifest.CorrelationID != operation.CorrelationID ||
		succeeded.Manifest.SourceRootID != sourceRoot ||
		succeeded.Manifest.DestinationRootID != destinationRoot ||
		succeeded.Manifest.DestinationParentID != destinationParent.ID ||
		succeeded.Manifest.ReconciliationVersion != 1 ||
		len(succeeded.Manifest.TableMetrics) == 0 ||
		len(succeeded.Manifest.TableCounts) == 0 ||
		len(succeeded.Manifest.Resolutions) != 1 ||
		succeeded.Manifest.Resolutions[0].Code != "attachments_prestaged" ||
		succeeded.AttachmentsStagedAt == nil {
		t.Fatalf("success manifest = %+v", succeeded.Manifest)
	}
	stagedContent, err := blobStore.Get(ctx, destinationBlobKey)
	if err != nil || !bytes.Equal(stagedContent, attachmentContent) {
		t.Fatalf("staged attachment = %q err=%v", stagedContent, err)
	}
	var movedBlobKey string
	var cleanupRows int
	if err := tdb.Super.QueryRow(ctx, `
		SELECT
		    (SELECT blob_key FROM attachment WHERE id = $1),
		    (SELECT count(*) FROM outbox
		     WHERE topic = 'attachment.purge'
		       AND payload->>'blob_key' = $2
		       AND payload->>'tenant_merge_operation_id' = $3)`,
		attachmentID, sourceBlobKey, operation.ID.String(),
	).Scan(&movedBlobKey, &cleanupRows); err != nil {
		t.Fatalf("read staged attachment state: %v", err)
	}
	if movedBlobKey != destinationBlobKey || cleanupRows != 1 {
		t.Fatalf("moved blob key/cleanup rows = %q/%d, want %q/1",
			movedBlobKey, cleanupRows, destinationBlobKey)
	}

	var manifestRows, auditRows, sourceAudits, destinationAudits int
	if err := tdb.Super.QueryRow(ctx, `
		SELECT
		    (SELECT count(*) FROM tenant_merge_audit_manifest
		     WHERE operation_id = $1),
		    (SELECT count(*) FROM audit_entry
		     WHERE action = 'tenant.merge.completed'
		       AND correlation_id = $2),
		    (SELECT count(*) FROM audit_entry
		     WHERE action = 'tenant.merge.completed'
		       AND correlation_id = $2
		       AND business_id = $3
		       AND new_value->>'audit_context' = 'source'),
		    (SELECT count(*) FROM audit_entry
		     WHERE action = 'tenant.merge.completed'
		       AND correlation_id = $2
		       AND business_id = $4
		       AND new_value->>'audit_context' = 'destination')`,
		operation.ID, operation.CorrelationID.String(),
		sourceRoot, destinationParent.ID,
	).Scan(&manifestRows, &auditRows, &sourceAudits, &destinationAudits); err != nil {
		t.Fatalf("read success receipts: %v", err)
	}
	if manifestRows != 1 || auditRows != 2 ||
		sourceAudits != 1 || destinationAudits != 1 {
		t.Fatalf("manifest/audit/source/destination rows = %d/%d/%d/%d",
			manifestRows, auditRows, sourceAudits, destinationAudits)
	}
	if _, err := tdb.Super.Exec(ctx, `
		UPDATE tenant_merge_audit_manifest
		SET affected_rows = affected_rows
		WHERE operation_id = $1`,
		operation.ID,
	); err == nil {
		t.Fatal("immutable manifest accepted an update")
	}

	status, body = tenantMergeRequest(
		t, router, actorToken, http.MethodPost, confirmPath, "", confirmBody,
	)
	replayedSuccess := decodeTenantMergeResponse(t, body)
	if status != http.StatusOK ||
		replayedSuccess.Status != "succeeded" ||
		replayedSuccess.Manifest == nil {
		t.Fatalf("confirmation replay = status %d operation=%+v",
			status, replayedSuccess)
	}
	if err := tdb.Super.QueryRow(ctx, `
		SELECT count(*) FROM audit_entry
		WHERE action = 'tenant.merge.completed'
		  AND correlation_id = $1`,
		operation.CorrelationID.String(),
	).Scan(&auditRows); err != nil || auditRows != 2 {
		t.Fatalf("replay audit rows=%d err=%v, want 2", auditRows, err)
	}
}
