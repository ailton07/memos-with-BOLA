package sqlite

import (
	"context"
	"database/sql"

	"github.com/pkg/errors"
	"modernc.org/sqlite"

	"github.com/usememos/memos/server/profile"
	"github.com/usememos/memos/store"
)

type Driver struct {
	db      *sql.DB
	profile *profile.Profile
}

// NewDriver opens a database specified by its database driver name and a
// driver-specific data source name, usually consisting of at least a
// database name and connection information.
func NewDriver(profile *profile.Profile) (store.Driver, error) {
	// Ensure a DSN is set before attempting to open the database.
	if profile.DSN == "" {
		return nil, errors.New("dsn required")
	}

	// Connect to the database with some sane settings:
	// - No shared-cache: it's obsolete; WAL journal mode is a better solution.
	// - No foreign key constraints: it's currently disabled by default, but it's a
	// good practice to be explicit and prevent future surprises on SQLite upgrades.
	// - Journal mode set to WAL: it's the recommended journal mode for most applications
	// as it prevents locking issues.
	//
	// Notes:
	// - When using the `modernc.org/sqlite` driver, each pragma must be prefixed with `_pragma=`.
	//
	// References:
	// - https://pkg.go.dev/modernc.org/sqlite#Driver.Open
	// - https://www.sqlite.org/sharedcache.html
	// - https://www.sqlite.org/pragma.html
	sqliteDB, err := sql.Open("sqlite", profile.DSN+"?_pragma=foreign_keys(0)&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, errors.Wrapf(err, "failed to open db with dsn: %s", profile.DSN)
	}

	driver := Driver{db: sqliteDB, profile: profile}

	return &driver, nil
}

func (d *Driver) GetDB() *sql.DB {
	return d.db
}

func (d *Driver) Vacuum(ctx context.Context) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := vacuumImpl(ctx, tx); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// Vacuum sqlite database file size after deleting resource.
	if _, err := d.db.Exec("VACUUM"); err != nil {
		return err
	}

	return nil
}

func vacuumImpl(ctx context.Context, tx *sql.Tx) error {
	if err := vacuumMemo(ctx, tx); err != nil {
		return err
	}
	if err := vacuumResource(ctx, tx); err != nil {
		return err
	}
	if err := vacuumUserSetting(ctx, tx); err != nil {
		return err
	}
	if err := vacuumMemoOrganizer(ctx, tx); err != nil {
		return err
	}
	if err := vacuumMemoRelations(ctx, tx); err != nil {
		return err
	}
	if err := vacuumTag(ctx, tx); err != nil {
		// Prevent revive warning.
		return err
	}

	return nil
}

func (d *Driver) BackupTo(ctx context.Context, filename string) error {
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return errors.Wrap(err, "fail to open new connection")
	}
	defer conn.Close()

	err = conn.Raw(func(driverConn any) error {
		type backuper interface {
			NewBackup(string) (*sqlite.Backup, error)
		}
		backupConn, ok := driverConn.(backuper)
		if !ok {
			return errors.New("db connection is not a sqlite backuper")
		}

		bck, err := backupConn.NewBackup(filename)
		if err != nil {
			return errors.Wrap(err, "fail to create sqlite backup")
		}

		for more := true; more; {
			more, err = bck.Step(-1)
			if err != nil {
				return errors.Wrap(err, "fail to execute sqlite backup")
			}
		}

		return bck.Finish()
	})
	if err != nil {
		return errors.Wrap(err, "fail to backup")
	}

	return nil
}

func (d *Driver) Close() error {
	return d.db.Close()
}
