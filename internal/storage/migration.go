package storage

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"gorm.io/gorm"

	migrationfiles "gpt-load/internal/storage/migrations"
)

const (
	migrationLedgerTable       = "schema_migrations"
	initialSchemaSentinelTable = "groups"
)

var migrationIDPattern = regexp.MustCompile(`^(\d{4})_[a-z0-9]+(?:_[a-z0-9]+)*$`)

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

var migrations = []migration{
	{ID: migrationfiles.ID0001, Up: migrationfiles.Up0001, Validate: migrationfiles.Validate0001, ValidateCurrent: migrationfiles.ValidateCurrent0001},
	{ID: migrationfiles.ID0002, Up: migrationfiles.Up0002, Validate: migrationfiles.Validate0002},
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
}

func applyMigrations(db *gorm.DB) error {
	return applyMigrationRegistry(db, migrations)
}

func applyMigrationRegistry(db *gorm.DB, entries []migration) error {
	if db == nil {
		return fmt.Errorf("apply migrations: db is nil")
	}
	if db.Dialector == nil {
		return fmt.Errorf("apply migrations: database dialector is nil")
	}
	if err := validateMigrationRegistry(entries); err != nil {
		return err
	}

	return db.Connection(func(connection *gorm.DB) error {
		if err := acquireMigrationLock(connection); err != nil {
			return err
		}
		// Raw lock acquisition and Scan may populate GORM's statement schema
		// with the scalar result type. Start fresh sessions for migration and
		// release so that state cannot leak into the schema/table operations.
		operationErr := applyMigrationsLocked(
			connection.Session(&gorm.Session{NewDB: true}),
			entries,
		)
		releaseErr := releaseMigrationLock(connection.Session(&gorm.Session{NewDB: true}))
		return errors.Join(operationErr, releaseErr)
	})
}

func validateMigrationRegistry(entries []migration) error {
	for index, entry := range entries {
		position := index + 1
		matches := migrationIDPattern.FindStringSubmatch(entry.ID)
		if len(matches) != 2 {
			return fmt.Errorf("migration registry entry %d has invalid ID %q", position, entry.ID)
		}
		number, err := strconv.Atoi(matches[1])
		if err != nil || number != position {
			return fmt.Errorf(
				"migration registry entry %d has non-contiguous ID %q",
				position,
				entry.ID,
			)
		}
		if entry.Up == nil || entry.Validate == nil {
			return fmt.Errorf("migration registry entry %d (%s) is incomplete", position, entry.ID)
		}
	}
	return nil
}

func applyMigrationsLocked(db *gorm.DB, entries []migration) error {
	hadMigrationLedger := db.Migrator().HasTable(migrationLedgerTable)
	if !hadMigrationLedger {
		if db.Migrator().HasTable(initialSchemaSentinelTable) {
			return fmt.Errorf(
				"initialize database schema: %s table already exists",
				initialSchemaSentinelTable,
			)
		}
		if err := db.AutoMigrate(&schemaMigration{}); err != nil {
			return fmt.Errorf("create schema_migrations: %w", err)
		}
	}

	var applied []string
	if err := db.Table(migrationLedgerTable).Order("id ASC").Pluck("id", &applied).Error; err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	for index, id := range applied {
		if index >= len(entries) || entries[index].ID != id {
			return fmt.Errorf("schema_migrations contains unknown or non-contiguous migration %q", id)
		}
	}
	for index, id := range applied {
		validator := entries[index].Validate
		if entries[index].ValidateCurrent != nil {
			validator = entries[index].ValidateCurrent
		}
		if err := validator(db); err != nil {
			return fmt.Errorf("validate applied migration %s: %w", id, err)
		}
	}

	for _, entry := range entries[len(applied):] {
		if err := applyMigration(db, entry); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration runs one migration, its validation, and its ledger row in a
// single transaction; PostgreSQL DDL is transactional, so a failed migration
// leaves no partial schema behind.
func applyMigration(db *gorm.DB, entry migration) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := entry.Up(tx); err != nil {
			return fmt.Errorf("apply migration %s: %w", entry.ID, err)
		}
		if err := entry.Validate(tx); err != nil {
			return fmt.Errorf("validate migration %s: %w", entry.ID, err)
		}
		if err := tx.Create(&schemaMigration{ID: entry.ID}).Error; err != nil {
			return fmt.Errorf("record migration %s: %w", entry.ID, err)
		}
		return nil
	})
}

// AutoMigrate applies every pending migration before the application starts.
func AutoMigrate(db *gorm.DB) error {
	return applyMigrations(db)
}
