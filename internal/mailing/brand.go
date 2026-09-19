package mailing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"strings"
	"time"

	// Logo dimension checks decode the config of the three formats the standard library
	// ships; WebP stays sniff-only and keeps the stored width.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	mailrender "github.com/manyforge/manyforge/internal/mailing/render"
	"github.com/manyforge/manyforge/internal/platform/blob"
	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
	"github.com/manyforge/manyforge/internal/platform/errs"
)

const (
	maxBrandLogoBytes     int64 = 512 << 10
	maxBrandLogoDimension       = 2000
	maxBrandFooterBytes         = 4096
	defaultBrandLogoWidth       = 160
	naturalBrandLogoWidth       = 240
	minBrandLogoWidth           = 40
	maxBrandLogoWidth           = 600
)

// normalizeBrandInput trims, lowercases, and defaults the editable brand fields so a
// PUT is a full replacement that never depends on what the client omitted.
func normalizeBrandInput(in *BrandInput) error {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len(in.Name) > 200 {
		return validation("brand name is required and must not exceed 200 characters")
	}
	for _, c := range []struct {
		field *string
		def   string
		name  string
	}{
		{&in.Colors.Background, mailrender.DefaultColors.Background, "background"},
		{&in.Colors.Surface, mailrender.DefaultColors.Surface, "surface"},
		{&in.Colors.Text, mailrender.DefaultColors.Text, "text"},
		{&in.Colors.Accent, mailrender.DefaultColors.Accent, "accent"},
		{&in.Colors.HeaderBackground, mailrender.DefaultColors.HeaderBackground, "header_background"},
		{&in.Colors.HeaderText, mailrender.DefaultColors.HeaderText, "header_text"},
	} {
		v := strings.ToLower(strings.TrimSpace(*c.field))
		if v == "" {
			v = c.def
		}
		if !mailrender.ValidColor(v) {
			return validation("colors." + c.name + " must be a #rrggbb hex color")
		}
		*c.field = v
	}
	switch in.FontStack = strings.ToLower(strings.TrimSpace(in.FontStack)); in.FontStack {
	case "":
		in.FontStack = mailrender.FontStackSystem
	case mailrender.FontStackSystem, mailrender.FontStackSerif, mailrender.FontStackMono:
	default:
		return validation("font_stack must be one of system, serif, mono")
	}
	if len(in.FooterMarkdown) > maxBrandFooterBytes {
		return validation("footer_markdown must not exceed 4096 bytes")
	}
	if in.LogoWidth != nil && (*in.LogoWidth < minBrandLogoWidth || *in.LogoWidth > maxBrandLogoWidth) {
		return validation("logo_width must be between 40 and 600 pixels")
	}
	return nil
}

// GetBrand returns the business brand or errs.ErrNotFound when none is configured.
func (s *Service) GetBrand(ctx context.Context, principalID, businessID uuid.UUID) (Brand, error) {
	var out Brand
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		row, err := q.GetMailingBrand(ctx, dbgen.GetMailingBrandParams{BusinessID: businessID, TenantRootID: root})
		if err != nil {
			return err
		}
		out = s.toBrand(row)
		return nil
	})
	return out, mapErr(err)
}

// PutBrand creates or fully replaces the editable brand fields. The stored logo is
// kept; a nil LogoWidth keeps the stored width.
func (s *Service) PutBrand(ctx context.Context, principalID, businessID uuid.UUID, in BrandInput) (Brand, error) {
	if err := normalizeBrandInput(&in); err != nil {
		return Brand{}, err
	}
	var out Brand
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		params := dbgen.UpsertMailingBrandParams{
			ID: uuid.New(), BusinessID: businessID, TenantRootID: root, Name: in.Name, LogoWidth: defaultBrandLogoWidth,
			ColorBackground: in.Colors.Background, ColorSurface: in.Colors.Surface, ColorText: in.Colors.Text,
			ColorAccent: in.Colors.Accent, ColorHeaderBg: in.Colors.HeaderBackground, ColorHeaderText: in.Colors.HeaderText,
			FontStack: in.FontStack, FooterMarkdown: in.FooterMarkdown,
		}
		if in.LogoWidth != nil {
			params.LogoWidth = int32(*in.LogoWidth)
			params.SetLogoWidth = true
		}
		row, err := q.UpsertMailingBrand(ctx, params)
		if err != nil {
			return err
		}
		if err = auditMutation(ctx, tx, principalID, businessID, root, "mailing.brand.updated", "mailing_brand", row.ID, map[string]any{"font_stack": row.FontStack, "logo_width": row.LogoWidth, "has_logo": row.LogoBlobKey != nil}); err != nil {
			return err
		}
		out = s.toBrand(row)
		return nil
	})
	return out, mapErr(err)
}

// DeleteBrand removes the brand row and, after the row is gone, its logo object.
func (s *Service) DeleteBrand(ctx context.Context, principalID, businessID uuid.UUID) error {
	var logoKey *string
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		current, err := q.GetMailingBrand(ctx, dbgen.GetMailingBrandParams{BusinessID: businessID, TenantRootID: root})
		if err != nil {
			return err
		}
		row, err := q.DeleteMailingBrand(ctx, dbgen.DeleteMailingBrandParams{ID: current.ID, TenantRootID: root})
		if err != nil {
			return err
		}
		logoKey = row.LogoBlobKey
		return auditMutation(ctx, tx, principalID, businessID, root, "mailing.brand.deleted", "mailing_brand", row.ID, map[string]any{"had_logo": row.LogoBlobKey != nil})
	})
	if err != nil {
		return mapErr(err)
	}
	s.deleteBrandLogoObject(ctx, logoKey)
	return nil
}

// PutBrandLogo sniffs, bounds, and stores a logo for an existing brand. The declared
// Content-Type is never consulted; only the bytes decide. The object is written under
// a key that is stable per brand so a re-upload overwrites in place.
func (s *Service) PutBrandLogo(ctx context.Context, principalID, businessID uuid.UUID, content []byte) (Brand, error) {
	if s.Blob == nil {
		return Brand{}, validation("logo storage is not configured")
	}
	if len(content) == 0 {
		return Brand{}, validation("logo is empty")
	}
	if int64(len(content)) > maxBrandLogoBytes {
		return Brand{}, validation("logo must not exceed 512 KiB")
	}
	contentType, err := blob.Sniff(content)
	if err != nil || !strings.HasPrefix(contentType, "image/") {
		return Brand{}, validation("logo must be a PNG, JPEG, GIF, or WebP image")
	}
	naturalWidth := 0
	if cfg, _, decodeErr := image.DecodeConfig(bytes.NewReader(content)); decodeErr == nil {
		if cfg.Width > maxBrandLogoDimension || cfg.Height > maxBrandLogoDimension {
			return Brand{}, validation("logo must not exceed 2000 by 2000 pixels")
		}
		naturalWidth = max(min(cfg.Width, naturalBrandLogoWidth), minBrandLogoWidth)
	}
	sum := sha256.Sum256(content)
	var out Brand
	err = s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		current, err := q.GetMailingBrand(ctx, dbgen.GetMailingBrandParams{BusinessID: businessID, TenantRootID: root})
		if err != nil {
			return err
		}
		logoWidth := current.LogoWidth
		if naturalWidth > 0 {
			logoWidth = int32(naturalWidth)
		}
		key := blob.BrandLogoKey(root, businessID, current.ID)
		// Inside the tx like inbox attachments: a Put failure rolls the row back.
		if err := s.Blob.Put(ctx, key, content, contentType); err != nil {
			return fmt.Errorf("mailing: store brand logo: %w", err)
		}
		row, err := q.SetMailingBrandLogo(ctx, dbgen.SetMailingBrandLogoParams{
			LogoBlobKey: &key, LogoContentType: &contentType, LogoSha256: sum[:], LogoWidth: logoWidth,
			ID: current.ID, TenantRootID: root,
		})
		if err != nil {
			return err
		}
		if err = auditMutation(ctx, tx, principalID, businessID, root, "mailing.brand.logo_updated", "mailing_brand", row.ID, map[string]any{"content_type": contentType, "bytes": len(content), "sha256": hex.EncodeToString(sum[:]), "logo_width": row.LogoWidth}); err != nil {
			return err
		}
		out = s.toBrand(row)
		return nil
	})
	return out, mapErr(err)
}

// DeleteBrandLogo clears the stored logo reference and removes the object.
func (s *Service) DeleteBrandLogo(ctx context.Context, principalID, businessID uuid.UUID) (Brand, error) {
	var out Brand
	var logoKey *string
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		current, err := q.GetMailingBrand(ctx, dbgen.GetMailingBrandParams{BusinessID: businessID, TenantRootID: root})
		if err != nil {
			return err
		}
		logoKey = current.LogoBlobKey
		row, err := q.ClearMailingBrandLogo(ctx, dbgen.ClearMailingBrandLogoParams{ID: current.ID, TenantRootID: root})
		if err != nil {
			return err
		}
		if err = auditMutation(ctx, tx, principalID, businessID, root, "mailing.brand.logo_deleted", "mailing_brand", row.ID, map[string]any{"had_logo": logoKey != nil}); err != nil {
			return err
		}
		out = s.toBrand(row)
		return nil
	})
	if err != nil {
		return Brand{}, mapErr(err)
	}
	s.deleteBrandLogoObject(ctx, logoKey)
	return out, nil
}

// deleteBrandLogoObject runs after the row change committed. The key is stable per
// brand and the next upload overwrites it, so an orphaned object is logged, not fatal.
func (s *Service) deleteBrandLogoObject(ctx context.Context, key *string) {
	if key == nil || s.Blob == nil {
		return
	}
	if err := s.Blob.Delete(ctx, *key); err != nil && !errors.Is(err, errs.ErrNotFound) && s.Logger != nil {
		s.Logger.WarnContext(ctx, "mailing brand logo delete failed", "key", *key, "err", err)
	}
}

// PublicBrandLogo serves logo bytes to email clients by brand id (the only capability
// they hold). errs.ErrNotFound covers unknown ids and brands without a logo alike.
func (s *Service) PublicBrandLogo(ctx context.Context, brandID uuid.UUID) ([]byte, string, []byte, error) {
	if s.Blob == nil {
		return nil, "", nil, mapErr(errs.ErrNotFound)
	}
	var key, contentType string
	var sha []byte
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT logo_blob_key, logo_content_type, logo_sha256 FROM mailing_public_brand_logo($1)`, brandID).
			Scan(&key, &contentType, &sha)
	})
	if err != nil {
		return nil, "", nil, mapErr(err)
	}
	content, err := s.Blob.Get(ctx, key)
	if err != nil {
		return nil, "", nil, fmt.Errorf("mailing: read brand logo: %w", err)
	}
	return content, contentType, sha, nil
}

// PublicBrandForKey resolves the brand behind a publishable list key. Unknown or
// revoked keys and businesses without a brand return nil, nil so hosted pages get a
// uniform response that is not a key-validity oracle.
func (s *Service) PublicBrandForKey(ctx context.Context, key string) (*PublicBrand, error) {
	var out *PublicBrand
	err := s.DB.WithTx(ctx, func(tx pgx.Tx) error {
		list, found, err := resolvePublicList(ctx, tx, key)
		if err != nil || !found {
			return err
		}
		wb, err := s.queryBrandContext(ctx, tx, list.businessID)
		if err != nil || wb == nil {
			return err
		}
		out = &PublicBrand{Name: wb.render.Name, Colors: BrandColors(wb.render.Colors)}
		if wb.render.LogoURL != "" {
			out.LogoURL = strPtr(wb.render.LogoURL)
		}
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return out, nil
}

// workerBrand is the principal-less brand snapshot consumed by the send worker,
// the confirmation mail, and the public brand-by-key route.
type workerBrand struct {
	id        uuid.UUID
	updatedAt time.Time
	render    mailrender.Brand
}

// queryBrandContext reads the brand through the mailing_brand_context DEFINER on tx.
// It returns nil, nil when the business has no brand.
func (s *Service) queryBrandContext(ctx context.Context, tx pgx.Tx, businessID uuid.UUID) (*workerBrand, error) {
	var row dbgen.MailingBrand
	err := tx.QueryRow(ctx, `
		SELECT brand_id, updated_at, name, logo_blob_key, logo_content_type, logo_sha256, logo_width,
		       color_background, color_surface, color_text, color_accent, color_header_bg, color_header_text,
		       font_stack, footer_markdown
		FROM mailing_brand_context($1)`, businessID,
	).Scan(&row.ID, &row.UpdatedAt, &row.Name, &row.LogoBlobKey, &row.LogoContentType, &row.LogoSha256, &row.LogoWidth,
		&row.ColorBackground, &row.ColorSurface, &row.ColorText, &row.ColorAccent, &row.ColorHeaderBg, &row.ColorHeaderText,
		&row.FontStack, &row.FooterMarkdown)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &workerBrand{id: row.ID, updatedAt: row.UpdatedAt, render: s.renderBrand(&row, nil)}, nil
}

// renderBrand converts a stored row plus an optional unsaved override into the render
// brand. The override replaces every editable field; the stored logo is always kept.
// A nil row with a nil override yields the zero Brand, i.e. the default layout.
func (s *Service) renderBrand(row *dbgen.MailingBrand, override *BrandInput) mailrender.Brand {
	var out mailrender.Brand
	if row != nil {
		out = mailrender.Brand{
			Name: row.Name, LogoWidth: int(row.LogoWidth), FontStack: row.FontStack, FooterMarkdown: row.FooterMarkdown,
			Colors: mailrender.Colors{
				Background: row.ColorBackground, Surface: row.ColorSurface, Text: row.ColorText,
				Accent: row.ColorAccent, HeaderBackground: row.ColorHeaderBg, HeaderText: row.ColorHeaderText,
			},
		}
		if row.LogoBlobKey != nil {
			out.LogoURL = s.brandLogoURL(row.ID, row.LogoSha256)
		}
	}
	if override != nil {
		out.Name = override.Name
		out.Colors = mailrender.Colors(override.Colors)
		out.FontStack = override.FontStack
		out.FooterMarkdown = override.FooterMarkdown
		if override.LogoWidth != nil {
			out.LogoWidth = *override.LogoWidth
		}
	}
	return out
}

// brandLogoURL is the absolute, cache-busted public logo URL embedded in mail.
func (s *Service) brandLogoURL(brandID uuid.UUID, sha []byte) string {
	return strings.TrimRight(s.PublicBaseURL, "/") + "/m/b/" + brandID.String() + "/logo?v=" + hex.EncodeToString(sha[:min(len(sha), 8)])
}

func (s *Service) toBrand(r dbgen.MailingBrand) Brand {
	out := Brand{
		ID: r.ID, BusinessID: r.BusinessID, TenantRootID: r.TenantRootID, Name: r.Name,
		LogoWidth: int(r.LogoWidth), FontStack: r.FontStack, FooterMarkdown: r.FooterMarkdown,
		Colors: BrandColors{
			Background: r.ColorBackground, Surface: r.ColorSurface, Text: r.ColorText,
			Accent: r.ColorAccent, HeaderBackground: r.ColorHeaderBg, HeaderText: r.ColorHeaderText,
		},
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if r.LogoBlobKey != nil {
		out.Logo = &BrandLogo{URL: s.brandLogoURL(r.ID, r.LogoSha256), ContentType: stringValue(r.LogoContentType)}
	}
	return out
}
