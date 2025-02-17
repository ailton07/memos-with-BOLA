package mysql

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"

	"github.com/pkg/errors"

	"github.com/usememos/memos/server/version"
)

const (
	latestSchemaFileName = "LATEST__SCHEMA.sql"
)

//go:embed migration
var migrationFS embed.FS

func (d *Driver) Migrate(ctx context.Context) error {
	if d.profile.IsDev() {
		return d.nonProdMigrate(ctx)
	}

	return d.prodMigrate(ctx)
}

func (d *Driver) nonProdMigrate(ctx context.Context) error {
	buf, err := migrationFS.ReadFile("migration/dev/" + latestSchemaFileName)
	if err != nil {
		return errors.Errorf("failed to read latest schema file: %s", err)
	}

	stmt := string(buf)
	if _, err := d.db.ExecContext(ctx, stmt); err != nil {
		return errors.Errorf("failed to exec SQL %s: %s", stmt, err)
	}

	// In demo mode, we should seed the database.
	if d.profile.Mode == "demo" {
		if err := d.seed(ctx); err != nil {
			return errors.Wrap(err, "failed to seed")
		}
	}
	return nil
}

func (d *Driver) prodMigrate(ctx context.Context) error {
	currentVersion := version.GetCurrentVersion(d.profile.Mode)
	migrationHistoryList, err := d.FindMigrationHistoryList(ctx, &MigrationHistoryFind{})
	if err != nil {
		return errors.Wrap(err, "failed to find migration history")
	}
	// If there is no migration history, we should apply the latest schema.
	if len(migrationHistoryList) == 0 {
		buf, err := migrationFS.ReadFile("migration/prod/" + latestSchemaFileName)
		if err != nil {
			return errors.Errorf("failed to read latest schema file: %s", err)
		}

		stmt := string(buf)
		if _, err := d.db.ExecContext(ctx, stmt); err != nil {
			return errors.Errorf("failed to exec SQL %s: %s", stmt, err)
		}
		if _, err := d.UpsertMigrationHistory(ctx, &MigrationHistoryUpsert{
			Version: currentVersion,
		}); err != nil {
			return errors.Wrap(err, "failed to upsert migration history")
		}
		return nil
	}

	migrationHistoryVersionList := []string{}
	for _, migrationHistory := range migrationHistoryList {
		migrationHistoryVersionList = append(migrationHistoryVersionList, migrationHistory.Version)
	}
	sort.Sort(version.SortVersion(migrationHistoryVersionList))
	latestMigrationHistoryVersion := migrationHistoryVersionList[len(migrationHistoryVersionList)-1]
	if !version.IsVersionGreaterThan(version.GetSchemaVersion(currentVersion), latestMigrationHistoryVersion) {
		return nil
	}

	println("start migrate")
	for _, minorVersion := range getMinorVersionList() {
		normalizedVersion := minorVersion + ".0"
		if version.IsVersionGreaterThan(normalizedVersion, latestMigrationHistoryVersion) && version.IsVersionGreaterOrEqualThan(currentVersion, normalizedVersion) {
			println("applying migration for", normalizedVersion)
			if err := d.applyMigrationForMinorVersion(ctx, minorVersion); err != nil {
				return errors.Wrap(err, "failed to apply minor version migration")
			}
		}
	}
	println("end migrate")
	return nil
}

func (d *Driver) applyMigrationForMinorVersion(ctx context.Context, minorVersion string) error {
	filenames, err := fs.Glob(migrationFS, fmt.Sprintf("migration/prod/%s/*.sql", minorVersion))
	if err != nil {
		return errors.Wrap(err, "failed to read ddl files")
	}

	sort.Strings(filenames)
	// Loop over all migration files and execute them in order.
	for _, filename := range filenames {
		buf, err := migrationFS.ReadFile(filename)
		if err != nil {
			return errors.Wrapf(err, "failed to read minor version migration file, filename=%s", filename)
		}
		for _, stmt := range strings.Split(string(buf), ";") {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := d.db.ExecContext(ctx, stmt); err != nil {
				return errors.Wrapf(err, "migrate error: %s", stmt)
			}
		}
	}

	// Upsert the newest version to migration_history.
	version := minorVersion + ".0"
	if _, err = d.UpsertMigrationHistory(ctx, &MigrationHistoryUpsert{Version: version}); err != nil {
		return errors.Wrapf(err, "failed to upsert migration history with version: %s", version)
	}

	return nil
}

//go:embed seed
var seedFS embed.FS

func (d *Driver) seed(ctx context.Context) error {
	filenames, err := fs.Glob(seedFS, "seed/*.sql")
	if err != nil {
		return errors.Wrap(err, "failed to read seed files")
	}

	sort.Strings(filenames)
	// Loop over all seed files and execute them in order.
	for _, filename := range filenames {
		buf, err := seedFS.ReadFile(filename)
		if err != nil {
			return errors.Wrapf(err, "failed to read seed file, filename=%s", filename)
		}

		for _, stmt := range strings.Split(string(buf), ";") {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := d.db.ExecContext(ctx, stmt); err != nil {
				return errors.Wrapf(err, "seed error: %s", stmt)
			}
		}
	}
	return nil
}

// minorDirRegexp is a regular expression for minor version directory.
var minorDirRegexp = regexp.MustCompile(`^migration/prod/[0-9]+\.[0-9]+$`)

func getMinorVersionList() []string {
	minorVersionList := []string{}

	if err := fs.WalkDir(migrationFS, "migration", func(path string, file fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if file.IsDir() && minorDirRegexp.MatchString(path) {
			minorVersionList = append(minorVersionList, file.Name())
		}

		return nil
	}); err != nil {
		panic(err)
	}

	sort.Sort(version.SortVersion(minorVersionList))

	return minorVersionList
}
