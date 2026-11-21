package store

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
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
	m, redact, err := newMigrator(databaseURL)
	if err != nil {
		return err
	}
	defer closeMigrator(m)
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("applying migrations: %s", redact.Replace(err.Error()))
	}
	return nil
}

// MigrateDown reverts every embedded migration. Intended for tests and local
// development only; it drops all data.
func MigrateDown(databaseURL string) error {
	m, redact, err := newMigrator(databaseURL)
	if err != nil {
		return err
	}
	defer closeMigrator(m)
	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("reverting migrations: %s", redact.Replace(err.Error()))
	}
	return nil
}

func newMigrator(databaseURL string) (*migrate.Migrate, *strings.Replacer, error) {
	u, err := url.Parse(databaseURL)
	if err != nil {
		// url.Error embeds the full URL (and password); do not wrap it.
		return nil, nil, errors.New("parsing database URL: malformed URL")
	}
	switch u.Scheme {
	case "postgres", "postgresql":
		u.Scheme = "pgx5"
	case "pgx5":
	default:
		return nil, nil, fmt.Errorf("unsupported database URL scheme %q", u.Scheme)
	}
	redact := migrationPasswordRedactor(u)
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return nil, nil, fmt.Errorf("loading embedded migrations: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, u.String())
	if err != nil {
		return nil, nil, fmt.Errorf("initialising migrator: %s", redact.Replace(err.Error()))
	}
	return m, redact, nil
}

// migrationPasswordRedactor covers decoded values and URL-encoded passwords.
func migrationPasswordRedactor(u *url.URL) *strings.Replacer {
	var secrets []string
	if password, ok := u.User.Password(); ok {
		encoded := strings.TrimPrefix(url.UserPassword("", password).String(), ":")
		secrets = append(secrets, password, encoded)
	}
	for _, password := range u.Query()["password"] {
		secrets = append(secrets, password, url.QueryEscape(password))
	}
	// Match longer values first when one password is a prefix of another.
	slices.SortFunc(secrets, func(a, b string) int { return len(b) - len(a) })
	var replacements []string
	for _, secret := range secrets {
		if secret != "" {
			replacements = append(replacements, secret, "xxxxx")
		}
	}
	return strings.NewReplacer(replacements...)
}

func closeMigrator(m *migrate.Migrate) {
	// Close errors on a finished migration carry no actionable information.
	_, _ = m.Close()
}
