package postgres

import (
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers the pgx5:// driver
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/umutsahin222/insider/migrations"
)

// Migrate applies every pending up-migration from the embedded schema.
func Migrate(databaseURL string) error {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("opening migrations: %w", err)
	}
	// golang-migrate selects its driver by URL scheme.
	url := databaseURL
	for _, p := range []string{"postgres://", "postgresql://"} {
		if strings.HasPrefix(url, p) {
			url = "pgx5://" + strings.TrimPrefix(url, p)
		}
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, url)
	if err != nil {
		return fmt.Errorf("initialising migrations: %w", err)
	}
	upErr := m.Up()
	srcErr, dbErr := m.Close()
	if upErr != nil && !errors.Is(upErr, migrate.ErrNoChange) {
		return fmt.Errorf("applying migrations: %w", upErr)
	}
	if err := errors.Join(srcErr, dbErr); err != nil {
		return fmt.Errorf("closing migrator: %w", err)
	}
	return nil
}
