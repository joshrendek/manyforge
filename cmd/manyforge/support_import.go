package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/authz"
	"github.com/manyforge/manyforge/internal/platform/blob"
	"github.com/manyforge/manyforge/internal/platform/config"
	"github.com/manyforge/manyforge/internal/platform/db"
	"github.com/manyforge/manyforge/internal/platform/errs"
	"github.com/manyforge/manyforge/internal/platform/httpx"
	"github.com/manyforge/manyforge/internal/ticketing"
)

func importSupportTicket(cfg config.Config, args []string, input io.Reader, output io.Writer) error {
	const usage = "usage: manyforge import-support-ticket OWNER_PRINCIPAL_ID BUSINESS_ID < snapshot.json"
	if len(args) != 2 {
		return errors.New(usage)
	}
	principal, principalErr := uuid.Parse(args[0])
	business, businessErr := uuid.Parse(args[1])
	if principalErr != nil || businessErr != nil || principal == uuid.Nil || business == uuid.Nil {
		return errors.New(usage)
	}
	var snapshot ticketing.HistoricalTicket
	if !decodeBoundedJSON(input, maxHistoricalSnapshotJSON, &snapshot) {
		return errors.New("snapshot must be one bounded historical ticket JSON object")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	svc, closeService, err := openHistoricalImportService(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeService()
	result, err := svc.ImportHistoricalTicket(ctx, principal, business, snapshot)
	if err != nil {
		return safeHistoricalImportError(err)
	}
	return json.NewEncoder(output).Encode(result)
}

const maxHistoricalSnapshotJSON = int64(ticketing.MaxHistoricalAttachmentBytes*4/3 + (24 << 20))

func decodeBoundedJSON(input io.Reader, maxBytes int64, out any) bool {
	limited := &io.LimitedReader{R: input, N: maxBytes + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	return decoder.Decode(out) == nil && decoder.Decode(new(any)) == io.EOF && limited.N > 0
}

func openHistoricalImportService(ctx context.Context, cfg config.Config) (*ticketing.PortalService, func(), error) {
	database, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, nil, errors.New("cannot open the support database")
	}
	svc := &ticketing.PortalService{
		DB: database, ReplyTokenKey: cfg.InboundReplyTokenSecret, SystemDomain: cfg.InboundSystemDomain,
		Resolve: func(ctx context.Context, tx pgx.Tx, principal, business uuid.UUID) (httpx.Permissions, error) {
			return authz.Resolve(ctx, tx, principal, business)
		},
	}
	if cfg.BlobURL != "" {
		bucket, err := blob.Open(ctx, cfg.BlobURL)
		if err != nil {
			database.Close()
			return nil, nil, ticketing.ErrAttachmentUnavailable
		}
		svc.Blob = bucket
	}
	return svc, func() {
		if svc.Blob != nil {
			_ = svc.Blob.Close()
		}
		database.Close()
	}, nil
}

func safeHistoricalImportError(err error) error {
	// A database error can include snapshot data. Only emit controlled errors.
	for _, expected := range []error{errs.ErrValidation, errs.ErrConflict, errs.ErrNotFound,
		errs.ErrTenantMaintenance, ticketing.ErrPortalUnavailable, ticketing.ErrAttachmentUnavailable,
		ticketing.ErrHistoricalReconciliation, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, expected) {
			return expected
		}
	}
	return errors.New("historical ticket operation failed")
}
