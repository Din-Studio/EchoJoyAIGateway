package migrations

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

const (
	// ID0003 removes the retired time-based freshness marker from credential observations.
	ID0003 = "0003_remove_observation_fresh_until"
)

const (
	credentialObservationTable0003      = "credential_observations"
	credentialObservationFreshUntil0003 = "fresh_until_ms"
	credentialObservationFreshCheck0003 = "chk_credential_observation_fresh_until"
)

type credentialObservation0003 struct {
	CredentialID uint   `gorm:"column:credential_id;primaryKey"`
	FreshUntilMS *int64 `gorm:"column:fresh_until_ms;check:chk_credential_observation_fresh_until,fresh_until_ms IS NULL OR fresh_until_ms >= 0"`
}

func (credentialObservation0003) TableName() string {
	return credentialObservationTable0003
}

// Up0003 physically removes the retired observation freshness column.
func Up0003(db *gorm.DB) error {
	if !db.Migrator().HasTable(&credentialObservation0003{}) {
		return fmt.Errorf("remove observation freshness: table %q is missing", credentialObservationTable0003)
	}
	if !db.Migrator().HasColumn(&credentialObservation0003{}, credentialObservationFreshUntil0003) {
		return nil
	}
	if err := db.Migrator().DropColumn(&credentialObservation0003{}, credentialObservationFreshUntil0003); err != nil {
		return fmt.Errorf("remove observation freshness column: %w", err)
	}
	return nil
}

// Validate0003 verifies that the retired column is absent and the remaining
// initial schema is intact.
func Validate0003(db *gorm.DB) error {
	if db.Migrator().HasColumn(&credentialObservation0003{}, credentialObservationFreshUntil0003) {
		return fmt.Errorf(
			"validate observation freshness removal: column %q.%q still exists",
			credentialObservationTable0003,
			credentialObservationFreshUntil0003,
		)
	}
	return validateInitialSchemaAfter0003(db)
}

// ValidateCurrent0001 preserves the frozen 0001 validator before 0003 removes
// the freshness check constraint, and validates the same schema without it
// once removed. Dropping the column also drops its check constraint, so the
// constraint's absence is the signal that 0003 has been applied.
func ValidateCurrent0001(db *gorm.DB) error {
	if db.Migrator().HasConstraint(&credentialObservation0003{}, credentialObservationFreshCheck0003) {
		return Validate0001(db)
	}
	return validateInitialSchemaAfter0003(db)
}

func validateInitialSchemaAfter0003(db *gorm.DB) error {
	definitions, err := initialSchemaDefinitions(db)
	if err != nil {
		return err
	}
	for _, definition := range definitions {
		if !db.Migrator().HasTable(definition.model) {
			return fmt.Errorf("validate current initial schema: table %q is missing", definition.table)
		}
		for column := range definition.columns {
			if definition.table == credentialObservationTable0003 &&
				strings.EqualFold(column, credentialObservationFreshUntil0003) {
				continue
			}
			if !db.Migrator().HasColumn(definition.model, column) {
				return fmt.Errorf(
					"validate current initial schema: column %q.%q is missing",
					definition.table,
					column,
				)
			}
		}
		for _, index := range definition.indexes {
			if !db.Migrator().HasIndex(definition.model, index) {
				return fmt.Errorf(
					"validate current initial schema: index %q on %q is missing",
					index,
					definition.table,
				)
			}
		}
		for _, constraint := range definition.constraints {
			if definition.table == credentialObservationTable0003 &&
				strings.EqualFold(constraint, credentialObservationFreshCheck0003) {
				continue
			}
			if !db.Migrator().HasConstraint(definition.model, constraint) {
				return fmt.Errorf(
					"validate current initial schema: constraint %q on %q is missing",
					constraint,
					definition.table,
				)
			}
		}
	}
	return nil
}
