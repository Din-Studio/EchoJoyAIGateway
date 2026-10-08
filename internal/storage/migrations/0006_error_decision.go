package migrations

import (
	"fmt"
	"strings"

	"gorm.io/gorm"
)

const ID0006 = "0006_error_decision"

const (
	requestLogAttemptTable0006 = "request_log_attempts"

	failureCategoryConstraint0006 = "chk_request_log_attempt_failure_category"
	failureOriginConstraint0006   = "chk_request_log_attempt_failure_origin"
	failureScopeConstraint0006    = "chk_request_log_attempt_failure_scope"
	retryDirectiveConstraint0006  = "chk_request_log_attempt_retry_directive"
	effectConstraint0006          = "chk_request_log_attempt_effect"
)

var requestLogAttemptDecisionColumns0006 = []struct {
	field  string
	column string
}{
	{field: "FailureOrigin", column: "failure_origin"},
	{field: "FailureScope", column: "failure_scope"},
	{field: "RetryDirective", column: "retry_directive"},
	{field: "Effect", column: "effect"},
	{field: "RuleID", column: "rule_id"},
}

var requestLogAttemptIndexes0006 = []string{
	"idx_request_log_attempts_group_completed_request",
	"idx_request_log_attempts_channel_completed_request",
	"idx_request_log_attempts_credential_completed_request",
	"idx_request_log_attempts_model_completed_request",
	"idx_request_log_attempts_status_completed_request",
	"idx_request_log_attempts_failure_completed_request",
	"idx_request_log_attempts_error_completed_request",
}

type requestLogAttemptDecision0006 struct {
	FailureOrigin  string `gorm:"column:failure_origin;type:varchar(16);not null;default:''"`
	FailureScope   string `gorm:"column:failure_scope;type:varchar(16);not null;default:''"`
	RetryDirective string `gorm:"column:retry_directive;type:varchar(32);not null;default:''"`
	Effect         string `gorm:"column:effect;type:varchar(32);not null;default:''"`
	RuleID         string `gorm:"column:rule_id;type:varchar(128);not null;default:''"`
}

func (requestLogAttemptDecision0006) TableName() string { return requestLogAttemptTable0006 }

var decisionConstraints0006 = []struct {
	name       string
	expression string
}{
	{
		name:       failureOriginConstraint0006,
		expression: "failure_origin IN ('','client','upstream','downstream','internal')",
	},
	{
		name:       failureScopeConstraint0006,
		expression: "failure_scope IN ('','request','model','credential','group')",
	},
	{
		name:       retryDirectiveConstraint0006,
		expression: "retry_directive IN ('','none','refresh_credential','next_candidate')",
	},
	{
		name:       effectConstraint0006,
		expression: "effect IN ('','none','cooldown_credential','record_credential_failure','skip_group')",
	},
}

const failureCategoryExpression0006 = "failure_category IN ('ok','rate_limited','model_unavailable','invalid_key','upstream_host_error','client_error','conversion_unsupported','downstream_cancel','authentication_required','ambiguous')"

// Up0006 adds the normalized Judge decision fields and extends the retained
// legacy failure category with authentication_required.
func Up0006(db *gorm.DB) error {
	if !db.Migrator().HasTable(requestLogAttemptTable0006) {
		return fmt.Errorf("add error decision: table %q is missing", requestLogAttemptTable0006)
	}
	model := &requestLogAttemptDecision0006{}
	for _, column := range requestLogAttemptDecisionColumns0006 {
		if db.Migrator().HasColumn(model, column.column) {
			continue
		}
		if err := db.Migrator().AddColumn(model, column.field); err != nil {
			return fmt.Errorf("add request_log_attempts.%s: %w", column.column, err)
		}
	}
	if db.Migrator().HasConstraint(requestLogAttemptTable0006, failureCategoryConstraint0006) {
		if err := dropCheckConstraint0006(db, failureCategoryConstraint0006); err != nil {
			return fmt.Errorf("replace request log failure category constraint: %w", err)
		}
	}
	if err := createCheckConstraint0006(db, failureCategoryConstraint0006, failureCategoryExpression0006); err != nil {
		return err
	}
	for _, constraint := range decisionConstraints0006 {
		if db.Migrator().HasConstraint(requestLogAttemptTable0006, constraint.name) {
			continue
		}
		if err := createCheckConstraint0006(db, constraint.name, constraint.expression); err != nil {
			return err
		}
	}
	return nil
}

func createCheckConstraint0006(db *gorm.DB, name, expression string) error {
	if err := db.Exec(fmt.Sprintf(
		`ALTER TABLE "%s" ADD CONSTRAINT "%s" CHECK (%s)`, requestLogAttemptTable0006, name, expression,
	)).Error; err != nil {
		return fmt.Errorf("create request log decision constraint %q: %w", name, err)
	}
	return nil
}

func dropCheckConstraint0006(db *gorm.DB, name string) error {
	return db.Migrator().DropConstraint(requestLogAttemptTable0006, name)
}

// ValidateRecoverable0006 checks that any decision column already present has
// the expected text type and NOT NULL shape; Validate0006 builds on it.
func ValidateRecoverable0006(db *gorm.DB) error {
	if !db.Migrator().HasTable(requestLogAttemptTable0006) {
		return fmt.Errorf("validate recoverable error decision: table %q is missing", requestLogAttemptTable0006)
	}
	columns, err := db.Migrator().ColumnTypes(requestLogAttemptTable0006)
	if err != nil {
		return fmt.Errorf("inspect recoverable error decision columns: %w", err)
	}
	newColumns := make(map[string]struct{}, len(requestLogAttemptDecisionColumns0006))
	for _, column := range requestLogAttemptDecisionColumns0006 {
		newColumns[column.column] = struct{}{}
	}
	for _, column := range columns {
		if _, relevant := newColumns[strings.ToLower(column.Name())]; !relevant {
			continue
		}
		typeName := strings.ToLower(column.DatabaseTypeName())
		if !strings.Contains(typeName, "char") && !strings.Contains(typeName, "text") {
			return fmt.Errorf("column %q has an incompatible type", column.Name())
		}
		if nullable, known := column.Nullable(); known && nullable {
			return fmt.Errorf("column %q is nullable", column.Name())
		}
	}
	return nil
}

// Validate0006 verifies the normalized decision schema and retained indexes.
func Validate0006(db *gorm.DB) error {
	if err := ValidateRecoverable0006(db); err != nil {
		return err
	}
	for _, column := range requestLogAttemptDecisionColumns0006 {
		if !db.Migrator().HasColumn(requestLogAttemptTable0006, column.column) {
			return fmt.Errorf("validate error decision: column %q is missing", column.column)
		}
	}
	for _, name := range append([]string{failureCategoryConstraint0006}, decisionConstraintNames0006()...) {
		if !db.Migrator().HasConstraint(requestLogAttemptTable0006, name) {
			return fmt.Errorf("validate error decision: constraint %q is missing", name)
		}
	}
	for _, index := range requestLogAttemptIndexes0006 {
		if !db.Migrator().HasIndex(requestLogAttemptTable0006, index) {
			return fmt.Errorf("validate error decision: index %q is missing", index)
		}
	}
	definition, err := failureCategoryConstraintDefinition0006(db)
	if err != nil {
		return err
	}
	if !strings.Contains(strings.ToLower(definition), "authentication_required") {
		return fmt.Errorf("validate error decision: failure category constraint is stale")
	}
	return nil
}

func decisionConstraintNames0006() []string {
	result := make([]string, 0, len(decisionConstraints0006))
	for _, constraint := range decisionConstraints0006 {
		result = append(result, constraint.name)
	}
	return result
}

func failureCategoryConstraintDefinition0006(db *gorm.DB) (string, error) {
	var definition string
	if err := db.Raw(
		"SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = ? AND conrelid = 'request_log_attempts'::regclass",
		failureCategoryConstraint0006,
	).Scan(&definition).Error; err != nil {
		return "", fmt.Errorf("inspect PostgreSQL failure category constraint: %w", err)
	}
	if strings.TrimSpace(definition) == "" {
		return "", fmt.Errorf("failure category constraint definition is missing")
	}
	return definition, nil
}
