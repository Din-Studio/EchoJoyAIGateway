package storage

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gorm.io/gorm"

	migrationfiles "gpt-load/internal/storage/migrations"
)

const (
	migrationLedgerTable       = "schema_migrations"
	initialSchemaSentinelTable = "groups"
)

type schemaMigration struct {
	ID string `gorm:"column:id;type:varchar(255);primaryKey;not null"`
}

func (schemaMigration) TableName() string {
	return migrationLedgerTable
}

type migration struct {
	ID              string
	Up              func(*gorm.DB) error
	Validate        func(*gorm.DB) error
	ValidateCurrent func(*gorm.DB) error
}

// migrationTrack is one independently numbered migration chain with its own
// ledger. IDs are Prefix followed by a contiguous 4-digit number starting at
// 0001, so a deployment can add its own track after the core one without
// taking core numbers. Tracks run in order under one lock.
type migrationTrack struct {
	Ledger string
	Prefix string
	// FreshSentinel, when set, is a table whose presence without the ledger
	// means the schema was not created by this track; the track refuses to
	// adopt it. Only the core track owns such a table.
	FreshSentinel string
	Entries       []migration
}

func coreMigrationTrack(entries []migration) migrationTrack {
	return migrationTrack{
		Ledger:        migrationLedgerTable,
		FreshSentinel: initialSchemaSentinelTable,
		Entries:       entries,
	}
}

var migrations = []migration{
	{ID: migrationfiles.ID0001, Up: migrationfiles.Up0001, Validate: migrationfiles.Validate0001, ValidateCurrent: migrationfiles.ValidateCurrent0001},
	{ID: migrationfiles.ID0002, Up: migrationfiles.Up0002, Validate: migrationfiles.Validate0002, ValidateCurrent: migrationfiles.ValidateCurrent0002},
	{ID: migrationfiles.ID0003, Up: migrationfiles.Up0003, Validate: migrationfiles.Validate0003},
	{ID: migrationfiles.ID0004, Up: migrationfiles.Up0004, Validate: migrationfiles.Validate0004},
	{ID: migrationfiles.ID0005, Up: migrationfiles.Up0005, Validate: migrationfiles.Validate0005},
	{ID: migrationfiles.ID0006, Up: migrationfiles.Up0006, Validate: migrationfiles.Validate0006},
	{ID: migrationfiles.ID0007, Up: migrationfiles.Up0007, Validate: migrationfiles.Validate0007},
	{ID: migrationfiles.ID0008, Up: migrationfiles.Up0008, Validate: migrationfiles.Validate0008},
	{ID: migrationfiles.ID0009, Up: migrationfiles.Up0009, Validate: migrationfiles.Validate0009},
	{ID: migrationfiles.ID0010, Up: migrationfiles.Up0010, Validate: migrationfiles.Validate0010},
	{ID: migrationfiles.ID0011, Up: migrationfiles.Up0011, Validate: migrationfiles.Validate0011},
	{ID: migrationfiles.ID0012, Up: migrationfiles.Up0012, Validate: migrationfiles.Validate0012},
	{ID: migrationfiles.ID0013, Up: migrationfiles.Up0013, Validate: migrationfiles.Validate0013},
	{ID: migrationfiles.ID0014, Up: migrationfiles.Up0014, Validate: migrationfiles.Validate0014},
	{ID: migrationfiles.ID0015, Up: migrationfiles.Up0015, Validate: migrationfiles.Validate0015},
	{ID: migrationfiles.ID0016, Up: migrationfiles.Up0016, Validate: migrationfiles.Validate0016},
	{ID: migrationfiles.ID0017, Up: migrationfiles.Up0017, Validate: migrationfiles.Validate0017},
	{ID: migrationfiles.ID0018, Up: migrationfiles.Up0018, Validate: migrationfiles.Validate0018},
	{ID: migrationfiles.ID0019, Up: migrationfiles.Up0019, Validate: migrationfiles.Validate0019},
	{ID: migrationfiles.ID0020, Up: migrationfiles.Up0020, Validate: migrationfiles.Validate0020},
	{ID: migrationfiles.ID0021, Up: migrationfiles.Up0021, Validate: migrationfiles.Validate0021},
	{ID: migrationfiles.ID0022, Up: migrationfiles.Up0022, Validate: migrationfiles.Validate0022},
	{ID: migrationfiles.ID0023, Up: migrationfiles.Up0023, Validate: migrationfiles.Validate0023},
	{ID: migrationfiles.ID0024, Up: migrationfiles.Up0024, Validate: migrationfiles.Validate0024},
	{ID: migrationfiles.ID0025, Up: migrationfiles.Up0025, Validate: migrationfiles.Validate0025},
}

func applyMigrations(db *gorm.DB) error {
	return applyMigrationTracks(db, coreMigrationTrack(migrations), lyMigrationTrack)
}

// applyMigrationRegistry applies entries as the core track.
func applyMigrationRegistry(db *gorm.DB, entries []migration) error {
	return applyMigrationTracks(db, coreMigrationTrack(entries))
}

func applyMigrationTracks(db *gorm.DB, tracks ...migrationTrack) error {
	if db == nil {
		return fmt.Errorf("apply migrations: db is nil")
	}
	for _, track := range tracks {
		if err := validateMigrationRegistry(track); err != nil {
			return err
		}
	}

	return db.Connection(func(connection *gorm.DB) error {
		if err := acquireMigrationLock(connection); err != nil {
			return err
		}
		// Raw lock acquisition and Scan may populate GORM's statement schema
		// with the scalar result type. Start fresh sessions for migration and
		// release so that state cannot leak into the schema/table operations.
		var operationErr error
		for _, track := range tracks {
			if operationErr = applyMigrationsLocked(
				connection.Session(&gorm.Session{NewDB: true}),
				track,
			); operationErr != nil {
				break
			}
		}
		releaseErr := releaseMigrationLock(connection.Session(&gorm.Session{NewDB: true}))
		return errors.Join(operationErr, releaseErr)
	})
}

func validateMigrationRegistry(track migrationTrack) error {
	label := strings.TrimSpace(track.Prefix + " migration registry")
	pattern := regexp.MustCompile(`^` + regexp.QuoteMeta(track.Prefix) + `(\d{4})_[a-z0-9]+(?:_[a-z0-9]+)*$`)
	for index, entry := range track.Entries {
		position := index + 1
		matches := pattern.FindStringSubmatch(entry.ID)
		if len(matches) != 2 {
			return fmt.Errorf("%s entry %d has invalid ID %q", label, position, entry.ID)
		}
		number, err := strconv.Atoi(matches[1])
		if err != nil || number != position {
			return fmt.Errorf("%s entry %d has non-contiguous ID %q", label, position, entry.ID)
		}
		if entry.Up == nil || entry.Validate == nil {
			return fmt.Errorf("%s entry %d (%s) is incomplete", label, position, entry.ID)
		}
	}
	return nil
}

func applyMigrationsLocked(db *gorm.DB, track migrationTrack) error {
	entries := track.Entries
	// 账本表刚建时 applied 为空，不会读到 inspection 里过期的表清单。
	inspection, err := newMigrationInspection(db)
	if err != nil {
		return err
	}
	hadLedger, err := inspection.hasTable(track.Ledger)
	if err != nil {
		return err
	}
	if !hadLedger {
		if track.FreshSentinel != "" {
			hadFreshSentinel, err := inspection.hasTable(track.FreshSentinel)
			if err != nil {
				return err
			}
			if hadFreshSentinel {
				return fmt.Errorf(
					"initialize database schema: %s table already exists",
					track.FreshSentinel,
				)
			}
		}
		if err := db.Table(track.Ledger).AutoMigrate(&schemaMigration{}); err != nil {
			return fmt.Errorf("create %s: %w", track.Ledger, err)
		}
	}

	var applied []string
	if err := db.Table(track.Ledger).Order("id ASC").Pluck("id", &applied).Error; err != nil {
		return fmt.Errorf("read %s: %w", track.Ledger, err)
	}
	for index, id := range applied {
		if index >= len(entries) || entries[index].ID != id {
			return fmt.Errorf("%s contains unknown or non-contiguous migration %q", track.Ledger, id)
		}
	}
	for index, id := range applied {
		validator := entries[index].Validate
		if entries[index].ValidateCurrent != nil {
			validator = entries[index].ValidateCurrent
		}
		if err := inspection.validate(validator); err != nil {
			return fmt.Errorf("validate applied migration %s: %w", id, err)
		}
	}

	for _, entry := range entries[len(applied):] {
		if err := applyMigration(db, track.Ledger, entry); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration runs one migration, its validation, and its ledger row in a
// single transaction; PostgreSQL DDL is transactional, so a failed migration
// leaves no partial schema behind.
func applyMigration(db *gorm.DB, ledger string, entry migration) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := entry.Up(tx); err != nil {
			return fmt.Errorf("apply migration %s: %w", entry.ID, err)
		}
		if err := validateMigrationAfterDDL(tx, entry.Validate); err != nil {
			return fmt.Errorf("validate migration %s: %w", entry.ID, err)
		}
		if err := tx.Table(ledger).Create(&schemaMigration{ID: entry.ID}).Error; err != nil {
			return fmt.Errorf("record migration %s: %w", entry.ID, err)
		}
		return nil
	})
}

// AutoMigrate applies every pending migration before the application starts.
func AutoMigrate(db *gorm.DB) error {
	return applyMigrations(db)
}
