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
	maxInput := int64(ticketing.MaxHistoricalAttachmentBytes*4/3 + (24 << 20))
	limited := &io.LimitedReader{R: input, N: maxInput + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	var snapshot ticketing.HistoricalTicket
	if err := decoder.Decode(&snapshot); err != nil || decoder.Decode(new(any)) != io.EOF || limited.N == 0 {
		return errors.New("snapshot must be one bounded historical ticket JSON object")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	database, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return errors.New("cannot open the support database")
	}
	defer database.Close()
	svc := ticketing.PortalService{
		DB: database, ReplyTokenKey: cfg.InboundReplyTokenSecret, SystemDomain: cfg.InboundSystemDomain,
		Resolve: func(ctx context.Context, tx pgx.Tx, principal, business uuid.UUID) (httpx.Permissions, error) {
			return authz.Resolve(ctx, tx, principal, business)
		},
	}
	if cfg.BlobURL != "" {
		bucket, err := blob.Open(ctx, cfg.BlobURL)
		if err != nil {
			return ticketing.ErrAttachmentUnavailable
		}
		defer func() { _ = bucket.Close() }()
		svc.Blob = bucket
	}
	result, err := svc.ImportHistoricalTicket(ctx, principal, business, snapshot)
	if err != nil {
		// Database error details can include source text. Report known, safe
		// errors and keep the raw database output out of operator logs.
		for _, expected := range []error{errs.ErrValidation, errs.ErrConflict, errs.ErrNotFound,
			errs.ErrTenantMaintenance, ticketing.ErrPortalUnavailable, ticketing.ErrAttachmentUnavailable} {
			if errors.Is(err, expected) {
				return err
			}
		}
		return errors.New("could not persist historical ticket")
	}
	return json.NewEncoder(output).Encode(result)
}
