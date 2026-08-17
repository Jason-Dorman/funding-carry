package db

import (
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"net/url"

	"github.com/golang-migrate/migrate/v4"
	// Registers the "pgx5" database driver golang-migrate resolves by URL scheme.
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// migrationFS carries the SQL into every binary that imports this package. The
// schema is therefore whatever the compiled code was built against — there is no
// second copy to fall out of step with it, and `make migrate` needs no files on
// disk and no migration CLI installed.
//
//go:embed migrations/*.sql
var migrationFS embed.FS

// migrateScheme is the URL scheme golang-migrate's pgx v5 driver registers
// itself under. DATABASE_URL is a normal `postgres://` string everywhere else in
// the system, so this is the one place it is rewritten.
const migrateScheme = "pgx5"

// Migrate applies every pending migration, in version order, and reports the
// schema version it ended on.
//
// Each file runs as a single multi-statement query, which Postgres executes in
// an implicit transaction: a migration either lands whole or not at all. A
// session-level advisory lock makes concurrent migrators wait rather than race,
// so a Compose restart that starts two of them is not a hazard.
//
// It takes no context: golang-migrate exposes none, and this runs as a one-shot
// command. Cancelling it means killing the process, which drops the connection
// and releases the lock.
func Migrate(databaseURL string, log *slog.Logger) error {
	source, err := iofs.New(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("open embedded migrations: %w", err)
	}

	target, err := migrateURL(databaseURL)
	if err != nil {
		return err
	}

	m, err := migrate.NewWithSourceInstance("iofs", source, target)
	if err != nil {
		return fmt.Errorf("open migrator: %w", err)
	}
	// Close reports a source error and a database error separately; both are
	// worth surfacing, since a failure to release the advisory lock would block
	// the next run.
	defer func() {
		if srcErr, dbErr := m.Close(); srcErr != nil || dbErr != nil {
			log.Error("closing migrator", "source_error", srcErr, "database_error", dbErr)
		}
	}()

	switch upErr := m.Up(); {
	case errors.Is(upErr, migrate.ErrNoChange):
		log.Info("schema already up to date")
	case upErr != nil:
		return fmt.Errorf("apply migrations: %w", upErr)
	}

	version, dirty, err := m.Version()
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	// Dirty means a migration failed partway and the version marker was left
	// behind. It needs a human, not a retry loop.
	if dirty {
		return fmt.Errorf("schema version %d is dirty: a migration failed and must be resolved by hand", version)
	}
	log.Info("migrations applied", "schema_version", version)
	return nil
}

// migrateURL swaps the scheme for the one golang-migrate's pgx v5 driver is
// registered under, leaving the rest of the connection string untouched.
func migrateURL(databaseURL string) (string, error) {
	u, err := url.Parse(databaseURL)
	if err != nil {
		// url.Parse errors quote the input, which would put the password in the
		// message. Report the failure without it.
		return "", errors.New("parse database url: not a valid url")
	}
	u.Scheme = migrateScheme
	return u.String(), nil
}
