package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/umutsahin222/insider/internal/domain"
)

// CreateTemplate stores t. A name that is already taken is an ErrConflict.
func (s *Store) CreateTemplate(ctx context.Context, t domain.Template) (domain.Template, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO templates (id, name, channel, body) VALUES ($1, $2, $3::text::notification_channel, $4)
		RETURNING id, name, channel::text, body, created_at`,
		t.ID, t.Name, string(t.Channel), t.Body)
	created, err := scanTemplate(row)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return domain.Template{}, domain.Conflict(`a template named "` + t.Name + `" already exists`)
	}
	if err != nil {
		return domain.Template{}, fmt.Errorf("creating template: %w", err)
	}
	return created, nil
}

// TemplatesByName returns the templates with the given names, keyed by name; names that do
// not exist are simply absent. One query serves a whole batch.
func (s *Store) TemplatesByName(ctx context.Context, names []string) (map[string]domain.Template, error) {
	out := make(map[string]domain.Template, len(names))
	if len(names) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, channel::text, body, created_at FROM templates WHERE name = ANY($1)`, names)
	if err != nil {
		return nil, fmt.Errorf("loading templates: %w", err)
	}
	templates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Template, error) { return scanTemplate(row) })
	if err != nil {
		return nil, fmt.Errorf("scanning templates: %w", err)
	}
	for _, t := range templates {
		out[t.Name] = t
	}
	return out, nil
}

// ListTemplates returns every template, ordered by name.
func (s *Store) ListTemplates(ctx context.Context) ([]domain.Template, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, channel::text, body, created_at FROM templates ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("listing templates: %w", err)
	}
	templates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Template, error) { return scanTemplate(row) })
	if err != nil {
		return nil, fmt.Errorf("scanning templates: %w", err)
	}
	return templates, nil
}

func scanTemplate(row pgx.Row) (domain.Template, error) {
	var (
		t       domain.Template
		channel string
	)
	if err := row.Scan(&t.ID, &t.Name, &channel, &t.Body, &t.CreatedAt); err != nil {
		return domain.Template{}, err
	}
	t.Channel = domain.Channel(channel)
	return t, nil
}
