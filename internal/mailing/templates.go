package mailing

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/manyforge/manyforge/internal/platform/db/dbgen"
)

const templateCursorKind = "mailing-template"

func validateTemplate(name, subject, body string) error {
	name = strings.TrimSpace(name)
	subject = strings.TrimSpace(subject)
	if name == "" || len(name) > 200 {
		return validation("template name is required and must not exceed 200 characters")
	}
	if subject == "" || len(subject) > 500 {
		return validation("subject is required and must not exceed 500 characters")
	}
	if len(body) > 1<<20 {
		return validation("body_markdown exceeds 1 MiB")
	}
	return nil
}

func (s *Service) CreateTemplate(ctx context.Context, principalID, businessID uuid.UUID, in TemplateInput) (Template, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Subject = strings.TrimSpace(in.Subject)
	if err := validateTemplate(in.Name, in.Subject, in.BodyMarkdown); err != nil {
		return Template{}, err
	}
	var out Template
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		row, err := q.InsertMailingTemplate(ctx, dbgen.InsertMailingTemplateParams{ID: uuid.New(), BusinessID: businessID, TenantRootID: root, Name: in.Name, Subject: in.Subject, Preheader: cleanOptional(in.Preheader), BodyMarkdown: in.BodyMarkdown, TrackOpens: in.TrackOpens, TrackClicks: in.TrackClicks})
		if err != nil {
			return err
		}
		if err = auditMutation(ctx, tx, principalID, businessID, root, "mailing.template.created", "mailing_template", row.ID, map[string]any{"track_opens": row.TrackOpens, "track_clicks": row.TrackClicks}); err != nil {
			return err
		}
		out = toTemplate(row)
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) GetTemplate(ctx context.Context, principalID, businessID, templateID uuid.UUID) (Template, error) {
	var out Template
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		row, err := loadTemplate(ctx, q, businessID, root, templateID)
		if err != nil {
			return err
		}
		out = toTemplate(row)
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) ListTemplates(ctx context.Context, principalID, businessID uuid.UUID, cursor string, limit int) (Page[Template], error) {
	lim := clampLimit(limit)
	var out Page[Template]
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		var rows []dbgen.MailingTemplate
		if cursor == "" {
			rows, err = q.ListMailingTemplates(ctx, dbgen.ListMailingTemplatesParams{BusinessID: businessID, TenantRootID: root, Limit: int32(lim + 1)})
		} else {
			at, id, derr := decodeTimeCursor(templateCursorKind, cursor)
			if derr != nil {
				return derr
			}
			rows, err = q.ListMailingTemplatesAfter(ctx, dbgen.ListMailingTemplatesAfterParams{BusinessID: businessID, TenantRootID: root, CurCreated: at, CurID: id, Lim: int32(lim + 1)})
		}
		if err != nil {
			return err
		}
		more := len(rows) > lim
		if more {
			rows = rows[:lim]
		}
		out.Items = make([]Template, 0, len(rows))
		for _, row := range rows {
			out.Items = append(out.Items, toTemplate(row))
		}
		if more {
			last := rows[len(rows)-1]
			out.NextCursor = strPtr(encodeTimeCursor(templateCursorKind, last.CreatedAt, last.ID))
		}
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) UpdateTemplate(ctx context.Context, principalID, businessID, templateID uuid.UUID, in TemplateUpdate) (Template, error) {
	if in.Name != nil {
		v := strings.TrimSpace(*in.Name)
		in.Name = &v
	}
	if in.Subject != nil {
		v := strings.TrimSpace(*in.Subject)
		in.Subject = &v
	}
	if in.Name != nil && (*in.Name == "" || len(*in.Name) > 200) {
		return Template{}, validation("template name is required and must not exceed 200 characters")
	}
	if in.Subject != nil && (*in.Subject == "" || len(*in.Subject) > 500) {
		return Template{}, validation("subject is required and must not exceed 500 characters")
	}
	if in.BodyMarkdown != nil && len(*in.BodyMarkdown) > 1<<20 {
		return Template{}, validation("body_markdown exceeds 1 MiB")
	}
	var out Template
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		if _, err = loadTemplate(ctx, q, businessID, root, templateID); err != nil {
			return err
		}
		set := in.SetPreheader
		row, err := q.UpdateMailingTemplate(ctx, dbgen.UpdateMailingTemplateParams{Name: in.Name, Subject: in.Subject, SetPreheader: &set, Preheader: cleanOptional(in.Preheader), BodyMarkdown: in.BodyMarkdown, TrackOpens: in.TrackOpens, TrackClicks: in.TrackClicks, ID: templateID, TenantRootID: root})
		if err != nil {
			return err
		}
		if err = auditMutation(ctx, tx, principalID, businessID, root, "mailing.template.updated", "mailing_template", templateID, map[string]any{"track_opens": row.TrackOpens, "track_clicks": row.TrackClicks}); err != nil {
			return err
		}
		out = toTemplate(row)
		return nil
	})
	return out, mapErr(err)
}

func (s *Service) DeleteTemplate(ctx context.Context, principalID, businessID, templateID uuid.UUID) error {
	err := s.DB.WithPrincipal(ctx, principalID, func(tx pgx.Tx) error {
		q := dbgen.New(tx)
		root, err := resolveTenantRoot(ctx, q, businessID)
		if err != nil {
			return err
		}
		if _, err = loadTemplate(ctx, q, businessID, root, templateID); err != nil {
			return err
		}
		row, err := q.DeleteMailingTemplate(ctx, dbgen.DeleteMailingTemplateParams{ID: templateID, TenantRootID: root})
		if err != nil {
			return err
		}
		if row.BusinessID != businessID {
			return pgx.ErrNoRows
		}
		return auditMutation(ctx, tx, principalID, businessID, root, "mailing.template.deleted", "mailing_template", templateID, map[string]any{})
	})
	return mapErr(err)
}

func loadTemplate(ctx context.Context, q *dbgen.Queries, businessID, root, id uuid.UUID) (dbgen.MailingTemplate, error) {
	row, err := q.GetMailingTemplate(ctx, dbgen.GetMailingTemplateParams{ID: id, TenantRootID: root})
	if err != nil {
		return dbgen.MailingTemplate{}, err
	}
	if row.BusinessID != businessID {
		return dbgen.MailingTemplate{}, pgx.ErrNoRows
	}
	return row, nil
}
func toTemplate(r dbgen.MailingTemplate) Template {
	return Template{ID: r.ID, BusinessID: r.BusinessID, TenantRootID: r.TenantRootID, Name: r.Name, Subject: r.Subject, Preheader: r.Preheader, BodyMarkdown: r.BodyMarkdown, TrackOpens: r.TrackOpens, TrackClicks: r.TrackClicks, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}
