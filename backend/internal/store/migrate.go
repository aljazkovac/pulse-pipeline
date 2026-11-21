package store

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers the pgx5:// driver
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/aljazkovac/pulse-pipeline/backend/migrations"
)

// golang-migrate has no context-aware API, so these functions take no ctx.
// Errors never include the database password.

// MigrateUp applies all pending embedded migrations to databaseURL
// (a postgres:// or postgresql:// URL).
func MigrateUp(databaseURL string) error {
	m, password, err := newMigrator(databaseURL)
	if err != nil {
		return err
	}
	defer closeMigrator(m)
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("applying migrations: %s", redact(err, password))
	}
	return nil
}

// MigrateDown reverts every embedded migration. Intended for tests and local
// development only; it drops all data.
func MigrateDown(databaseURL string) error {
	m, password, err := newMigrator(databaseURL)
	if err != nil {
		return err
	}
	defer closeMigrator(m)
	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("reverting migrations: %s", redact(err, password))
	}
	return nil
}

func newMigrator(databaseURL string) (*migrate.Migrate, string, error) {
	u, err := url.Parse(databaseURL)
	if err != nil {
		// url.Error embeds the full URL (and password); do not wrap it.
		return nil, "", errors.New("parsing database URL: malformed URL")
	}
	password, _ := u.User.Password()
	switch u.Scheme {
	case "postgres", "postgresql":
		u.Scheme = "pgx5"
	case "pgx5":
	default:
		return nil, "", fmt.Errorf("unsupported database URL scheme %q", u.Scheme)
	}
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return nil, "", fmt.Errorf("loading embedded migrations: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, u.String())
	if err != nil {
		return nil, "", fmt.Errorf("initialising migrator for %s: %s", u.Redacted(), redact(err, password))
	}
	return m, password, nil
}

// redact removes the password from an error message.
func redact(err error, password string) string {
	if password == "" {
		return err.Error()
	}
	return strings.ReplaceAll(err.Error(), password, "xxxxx")
}

func closeMigrator(m *migrate.Migrate) {
	// Close errors on a finished migration carry no actionable information.
	_, _ = m.Close()
}
