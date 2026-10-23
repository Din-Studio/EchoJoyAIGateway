package storage

import (
	"fmt"

	gormpostgres "gorm.io/driver/postgres"
	"gorm.io/gorm"
)

const (
	migrationColumnsQuery     = "SELECT column_name FROM information_schema.columns WHERE table_schema = ? AND table_name = ?"
	migrationIndexesQuery     = "SELECT indexname FROM pg_indexes WHERE schemaname = ? AND tablename = ?"
	migrationConstraintsQuery = "SELECT constraint_name FROM information_schema.table_constraints WHERE constraint_schema = ? AND table_name = ?"
)

// migrationInspection 在单次只读校验阶段复用元数据。迁移 Up 使用原始 DB，
// 每步 DDL 后再创建新的校验视图，避免读取过期结构。与 GORM 的 Has* 不同，
// 元数据读取失败会记录下来并让校验失败，不会被当作对象不存在。
type migrationInspection struct {
	db          *gorm.DB
	dialector   *gormpostgres.Dialector
	database    string
	schema      string
	tables      map[string]struct{}
	columns     map[string]map[string]struct{}
	indexes     map[string]map[string]struct{}
	constraints map[string]map[string]struct{}
	columnTypes map[string][]gorm.ColumnType
	err         error
}

func newMigrationInspection(db *gorm.DB) (*migrationInspection, error) {
	dialector, ok := db.Dialector.(*gormpostgres.Dialector)
	if !ok {
		return nil, fmt.Errorf("inspect migration schema: unsupported dialector %T", db.Dialector)
	}
	var current struct {
		Database string
		Schema   string
	}
	if err := db.Raw("SELECT CURRENT_DATABASE() AS database, CURRENT_SCHEMA() AS schema").Scan(&current).Error; err != nil {
		return nil, fmt.Errorf("inspect migration schema: %w", err)
	}
	if current.Schema == "" {
		return nil, fmt.Errorf("inspect migration schema: current schema is empty")
	}
	return &migrationInspection{
		db:          db,
		dialector:   dialector,
		database:    current.Database,
		schema:      current.Schema,
		columns:     make(map[string]map[string]struct{}),
		indexes:     make(map[string]map[string]struct{}),
		constraints: make(map[string]map[string]struct{}),
		columnTypes: make(map[string][]gorm.ColumnType),
	}, nil
}

func (inspection *migrationInspection) validate(validator func(*gorm.DB) error) error {
	db := inspection.db.Session(&gorm.Session{NewDB: true})
	db.Dialector = &migrationInspectionDialector{Dialector: inspection.dialector, inspection: inspection}
	validateErr := validator(db)
	if inspection.err != nil {
		return inspection.err
	}
	return validateErr
}

func validateMigrationAfterDDL(db *gorm.DB, validator func(*gorm.DB) error) error {
	inspection, err := newMigrationInspection(db)
	if err != nil {
		return err
	}
	return inspection.validate(validator)
}

func (inspection *migrationInspection) record(err error) bool {
	if err != nil && inspection.err == nil {
		inspection.err = err
	}
	return false
}

func (inspection *migrationInspection) tableNames() (map[string]struct{}, error) {
	if inspection.tables != nil {
		return inspection.tables, nil
	}
	var names []string
	if err := inspection.db.Raw(
		"SELECT table_name FROM information_schema.tables WHERE table_schema = ? AND table_type = 'BASE TABLE'",
		inspection.schema,
	).Scan(&names).Error; err != nil {
		return nil, fmt.Errorf("inspect migration tables: %w", err)
	}
	inspection.tables = nameSet(names)
	return inspection.tables, nil
}

func (inspection *migrationInspection) hasTable(table string) (bool, error) {
	names, err := inspection.tableNames()
	if err != nil {
		return false, err
	}
	_, exists := names[table]
	return exists, nil
}

func (inspection *migrationInspection) objectNames(cache map[string]map[string]struct{}, query, table string) (map[string]struct{}, error) {
	if names, ok := cache[table]; ok {
		return names, nil
	}
	var names []string
	if err := inspection.db.Raw(query, inspection.schema, table).Scan(&names).Error; err != nil {
		return nil, fmt.Errorf("inspect migration metadata on %q: %w", table, err)
	}
	cache[table] = nameSet(names)
	return cache[table], nil
}

func nameSet(names []string) map[string]struct{} {
	result := make(map[string]struct{}, len(names))
	for _, name := range names {
		result[name] = struct{}{}
	}
	return result
}

type migrationInspectionDialector struct {
	*gormpostgres.Dialector
	inspection *migrationInspection
}

func (dialector *migrationInspectionDialector) Migrator(db *gorm.DB) gorm.Migrator {
	return &migrationInspectionMigrator{
		Migrator:   dialector.Dialector.Migrator(db).(gormpostgres.Migrator),
		inspection: dialector.inspection,
	}
}

type migrationInspectionMigrator struct {
	gormpostgres.Migrator
	inspection *migrationInspection
}

func (migrator *migrationInspectionMigrator) CurrentDatabase() string {
	return migrator.inspection.database
}

func (migrator *migrationInspectionMigrator) HasTable(value interface{}) bool {
	stmt, err := migrator.statement(value)
	if err != nil {
		return migrator.inspection.record(err)
	}
	exists, err := migrator.inspection.hasTable(stmt.Table)
	if err != nil {
		return migrator.inspection.record(err)
	}
	return exists
}

func (migrator *migrationInspectionMigrator) HasColumn(value interface{}, field string) bool {
	stmt, err := migrator.statement(value)
	if err != nil {
		return migrator.inspection.record(err)
	}
	if stmt.Schema != nil {
		if parsed := stmt.Schema.LookUpField(field); parsed != nil {
			field = parsed.DBName
		}
	}
	return migrator.hasObject(migrator.inspection.columns, migrationColumnsQuery, stmt.Table, field)
}

func (migrator *migrationInspectionMigrator) HasIndex(value interface{}, name string) bool {
	stmt, err := migrator.statement(value)
	if err != nil {
		return migrator.inspection.record(err)
	}
	if stmt.Schema != nil {
		if index := stmt.Schema.LookIndex(name); index != nil {
			name = index.Name
		}
	}
	return migrator.hasObject(migrator.inspection.indexes, migrationIndexesQuery, stmt.Table, name)
}

func (migrator *migrationInspectionMigrator) HasConstraint(value interface{}, name string) bool {
	stmt, err := migrator.statement(value)
	if err != nil {
		return migrator.inspection.record(err)
	}
	constraint, table := migrator.GuessConstraintInterfaceAndTable(stmt, name)
	if constraint != nil {
		name = constraint.GetName()
	}
	return migrator.hasObject(migrator.inspection.constraints, migrationConstraintsQuery, table, name)
}

func (migrator *migrationInspectionMigrator) hasObject(cache map[string]map[string]struct{}, query, table, name string) bool {
	names, err := migrator.inspection.objectNames(cache, query, table)
	if err != nil {
		return migrator.inspection.record(err)
	}
	_, exists := names[name]
	return exists
}

func (migrator *migrationInspectionMigrator) ColumnTypes(value interface{}) ([]gorm.ColumnType, error) {
	stmt, err := migrator.statement(value)
	if err != nil {
		migrator.inspection.record(err)
		return nil, err
	}
	if columns, ok := migrator.inspection.columnTypes[stmt.Table]; ok {
		return columns, nil
	}
	columns, err := migrator.Migrator.ColumnTypes(value)
	if err != nil {
		migrator.inspection.record(err)
		return nil, err
	}
	migrator.inspection.columnTypes[stmt.Table] = columns
	return columns, nil
}

func (migrator *migrationInspectionMigrator) statement(value interface{}) (*gorm.Statement, error) {
	var result *gorm.Statement
	err := migrator.RunWithValue(value, func(stmt *gorm.Statement) error {
		result = stmt
		return nil
	})
	return result, err
}
