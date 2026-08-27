package mailing

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/events"
)

const (
	maxCSVBytes  = 5 << 20
	maxCSVRows   = 50_000
	importBatch  = 1_000
	maxRowErrors = 100
)

type ImportError struct {
	Row     int    `json:"row"`
	Message string `json:"message"`
}

type ImportResult struct {
	Imported int           `json:"imported"`
	Skipped  int           `json:"skipped"`
	Errors   []ImportError `json:"errors"`
}

type csvSubscriber struct {
	email, first, last string
	attributes         []byte
	tags               []string
	contactID          uuid.UUID
}

type countingReader struct {
	r io.Reader
	n int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}

func parseSubscriberCSV(r io.Reader) ([]csvSubscriber, []ImportError, int, error) {
	limited := &countingReader{r: io.LimitReader(r, maxCSVBytes+1)}
	br := bufio.NewReader(limited)
	sniff, err := br.Peek(512)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, bufio.ErrBufferFull) {
		return nil, nil, 0, validation("cannot read CSV")
	}
	if len(sniff) == 0 {
		return nil, nil, 0, validation("CSV is empty")
	}
	contentType := http.DetectContentType(sniff)
	if !strings.HasPrefix(contentType, "text/") && contentType != "application/octet-stream" {
		return nil, nil, 0, validation("file must be text CSV")
	}
	cr := csv.NewReader(br)
	cr.FieldsPerRecord = -1
	header, err := cr.Read()
	if err != nil {
		return nil, nil, 0, validation("CSV header is required")
	}
	columns := map[string]int{}
	for i, value := range header {
		columns[strings.ToLower(strings.TrimSpace(value))] = i
	}
	emailColumn, ok := columns["email"]
	if !ok {
		return nil, nil, 0, validation("CSV header must contain email")
	}
	rows := make([]csvSubscriber, 0)
	rowErrors := make([]ImportError, 0)
	processed := 0
	invalid := 0
	for rowNumber := 2; ; rowNumber++ {
		record, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		processed++
		if processed > maxCSVRows {
			return nil, nil, 0, validation("CSV exceeds 50000 rows")
		}
		if err != nil {
			invalid++
			addImportError(&rowErrors, rowNumber, "invalid CSV row")
			continue
		}
		get := func(name string) string {
			i, exists := columns[name]
			if !exists || i >= len(record) {
				return ""
			}
			return strings.TrimSpace(record[i])
		}
		if emailColumn >= len(record) {
			invalid++
			addImportError(&rowErrors, rowNumber, "email is required")
			continue
		}
		email, err := normalizeEmail(record[emailColumn])
		if err != nil {
			invalid++
			addImportError(&rowErrors, rowNumber, "invalid email")
			continue
		}
		attrs := []byte(`{}`)
		if raw := get("attributes"); raw != "" {
			var obj map[string]any
			if json.Unmarshal([]byte(raw), &obj) != nil {
				invalid++
				addImportError(&rowErrors, rowNumber, "attributes must be a JSON object")
				continue
			}
			attrs, err = jsonObject(obj)
			if err != nil {
				invalid++
				addImportError(&rowErrors, rowNumber, "attributes exceed 64 KiB")
				continue
			}
		}
		var tags []string
		if raw := get("tags"); raw != "" {
			tags, err = normalizeTags(strings.Split(raw, ";"))
			if err != nil {
				invalid++
				addImportError(&rowErrors, rowNumber, "invalid tags")
				continue
			}
		}
		rows = append(rows, csvSubscriber{email: email, first: get("first_name"), last: get("last_name"), attributes: attrs, tags: tags})
	}
	if limited.n > maxCSVBytes {
		return nil, nil, 0, validation("CSV exceeds 5 MiB")
	}
	return rows, rowErrors, invalid, nil
}

func (s *Service) ImportCSV(ctx context.Context, principalID, businessID, listID uuid.UUID, reader io.Reader, skipConfirmation bool) (ImportResult, error) {
	rows, rowErrors, invalid, err := parseSubscriberCSV(reader)
	if err != nil {
		return ImportResult{}, err
	}
	result := ImportResult{Errors: rowErrors, Skipped: invalid}
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		list, err := loadActiveList(ctx, q, businessID, root, listID)
		if err != nil {
			return err
		}
		status := dbgen.MailingSubscriberStatusPending
		if skipConfirmation || !list.DoubleOptIn {
			status = dbgen.MailingSubscriberStatusActive
		}
		for start := 0; start < len(rows); start += importBatch {
			end := start + importBatch
			if end > len(rows) {
				end = len(rows)
			}
			inserted, err := insertSubscriberBatch(ctx, tx, q, principalID, businessID, root, listID, status, dbgen.MailingConsentSourceCsvImport, rows[start:end])
			if err != nil {
				return err
			}
			result.Imported += inserted
		}
		result.Skipped += len(rows) - result.Imported
		return auditMutation(ctx, tx, principalID, businessID, root, "mailing.subscribers.imported", "mailing_list", listID, map[string]any{"imported": result.Imported, "skipped": result.Skipped})
	})
	return result, mapErr(err)
}

func (s *Service) AddSubscribersFromContacts(ctx context.Context, principalID, businessID, listID uuid.UUID, contactIDs []uuid.UUID, skipConfirmation bool) (ImportResult, error) {
	if len(contactIDs) == 0 || len(contactIDs) > maxCSVRows {
		return ImportResult{}, validation("contact_ids must contain 1 to 50000 IDs")
	}
	seen := make(map[uuid.UUID]struct{}, len(contactIDs))
	unique := make([]uuid.UUID, 0, len(contactIDs))
	for _, id := range contactIDs {
		if id == uuid.Nil {
			return ImportResult{}, validation("invalid contact ID")
		}
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			unique = append(unique, id)
		}
	}
	result := ImportResult{Skipped: len(contactIDs) - len(unique)}
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		list, err := loadActiveList(ctx, q, businessID, root, listID)
		if err != nil {
			return err
		}
		contacts, err := q.GetContactsByIDs(ctx, dbgen.GetContactsByIDsParams{Ids: unique, TenantRootID: root})
		if err != nil {
			return err
		}
		if len(contacts) != len(unique) {
			return validation("one or more contacts were not found")
		}
		rows := make([]csvSubscriber, 0, len(contacts))
		for _, contact := range contacts {
			email, err := normalizeEmail(contact.PrimaryEmail)
			if err != nil {
				return validation("contact has invalid email")
			}
			first, last := splitDisplayName(contact.DisplayName)
			rows = append(rows, csvSubscriber{email: email, first: first, last: last, attributes: []byte(`{}`), contactID: contact.ID})
		}
		status := dbgen.MailingSubscriberStatusPending
		if skipConfirmation || !list.DoubleOptIn {
			status = dbgen.MailingSubscriberStatusActive
		}
		for start := 0; start < len(rows); start += importBatch {
			end := start + importBatch
			if end > len(rows) {
				end = len(rows)
			}
			n, err := insertSubscriberBatch(ctx, tx, q, principalID, businessID, root, listID, status, dbgen.MailingConsentSourceCrm, rows[start:end])
			if err != nil {
				return err
			}
			result.Imported += n
		}
		result.Skipped += len(rows) - result.Imported
		return auditMutation(ctx, tx, principalID, businessID, root, "mailing.subscribers.added_from_contacts", "mailing_list", listID, map[string]any{"imported": result.Imported, "skipped": result.Skipped})
	})
	return result, mapErr(err)
}

func insertSubscriberBatch(ctx context.Context, tx pgx.Tx, q *dbgen.Queries, principalID, businessID, root, listID uuid.UUID, status dbgen.MailingSubscriberStatus, source dbgen.MailingConsentSource, rows []csvSubscriber) (int, error) {
	p := dbgen.InsertSubscribersBatchParams{BusinessID: businessID, TenantRootID: root, ListID: listID, Status: status, ConsentSource: source, ConsentAttestedBy: principalID}
	byEmail := make(map[string]csvSubscriber, len(rows))
	for _, row := range rows {
		p.Ids = append(p.Ids, uuid.New())
		p.Emails = append(p.Emails, row.email)
		p.FirstNames = append(p.FirstNames, row.first)
		p.LastNames = append(p.LastNames, row.last)
		p.Attributes = append(p.Attributes, row.attributes)
		p.ContactIds = append(p.ContactIds, row.contactID)
		if _, exists := byEmail[row.email]; !exists {
			byEmail[row.email] = row
		}
	}
	inserted, err := q.InsertSubscribersBatch(ctx, p)
	if err != nil {
		return 0, err
	}
	for _, row := range inserted {
		input := byEmail[row.Email]
		if err := replaceTags(ctx, tx, q, row, input.tags, nil); err != nil {
			return 0, err
		}
		if status == dbgen.MailingSubscriberStatusActive {
			if err := enqueueSubscriberEvent(ctx, tx, row, events.TopicMailingSubscriberActivated, "", "", ""); err != nil {
				return 0, err
			}
		}
	}
	return len(inserted), nil
}

func addImportError(errs *[]ImportError, row int, message string) {
	if len(*errs) < maxRowErrors {
		*errs = append(*errs, ImportError{Row: row, Message: message})
	}
}

func splitDisplayName(name *string) (string, string) {
	if name == nil {
		return "", ""
	}
	fields := strings.Fields(*name)
	if len(fields) == 0 {
		return "", ""
	}
	if len(fields) == 1 {
		return fields[0], ""
	}
	return fields[0], strings.Join(fields[1:], " ")
}
