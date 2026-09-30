package pgtest

import (
	"fmt"
	"strings"
	"testing"

	"gorm.io/gorm"
)

// FailOn makes a row-level event ("INSERT", "UPDATE", "DELETE", or an
// "UPDATE OF column" form) on table raise message before it is applied. A
// non-empty when is a trigger WHEN condition over NEW/OLD, e.g.
// "NEW.key = 'request_timeout'". The returned func removes the fault.
func FailOn(t testing.TB, db *gorm.DB, table, event, when, message string) func() {
	t.Helper()
	function := faultName("fail_on", table)
	condition := ""
	if when != "" {
		condition = " WHEN (" + when + ")"
	}
	statements := []string{
		fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	RAISE EXCEPTION %s;
END
$$`, function, quoteLiteral(message)),
		fmt.Sprintf(`CREATE TRIGGER %s BEFORE %s ON %s FOR EACH ROW%s EXECUTE FUNCTION %s()`,
			function, event, quoteIdentifier(table), condition, function),
	}
	execAll(t, db, statements)
	return removeFault(t, db, function)
}

// FailOnCommit makes any transaction that inserts, updates, or deletes a row in
// table fail at COMMIT, after every statement inside it has succeeded. The
// returned func removes the fault.
func FailOnCommit(t testing.TB, db *gorm.DB, table, message string) func() {
	t.Helper()
	function := faultName("fail_on_commit", table)
	statements := []string{
		fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	RAISE EXCEPTION %s;
END
$$`, function, quoteLiteral(message)),
		fmt.Sprintf(`CREATE CONSTRAINT TRIGGER %s AFTER INSERT OR UPDATE OR DELETE ON %s
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION %s()`,
			function, quoteIdentifier(table), function),
	}
	execAll(t, db, statements)
	return removeFault(t, db, function)
}

// DropConstraints drops table constraints of the given pg_constraint kinds
// ('c' check, 'f' foreign key) so a test can insert rows the schema rejects.
func DropConstraints(t testing.TB, db *gorm.DB, table string, kinds ...byte) {
	t.Helper()
	contypes := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		contypes = append(contypes, string(kind))
	}
	var names []string
	if err := db.Raw(`
		SELECT conname FROM pg_constraint
		WHERE conrelid = ?::regclass AND contype::text IN ?
		ORDER BY conname
	`, table, contypes).Scan(&names).Error; err != nil {
		t.Fatalf("list %s constraints: %v", table, err)
	}
	statements := make([]string, 0, len(names))
	for _, name := range names {
		statements = append(statements, fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s",
			quoteIdentifier(table), quoteIdentifier(name)))
	}
	execAll(t, db, statements)
}

// removeFault drops the fault function and, through CASCADE, its trigger.
func removeFault(t testing.TB, db *gorm.DB, function string) func() {
	return func() {
		t.Helper()
		execAll(t, db, []string{"DROP FUNCTION " + function + "() CASCADE"})
	}
}

func execAll(t testing.TB, db *gorm.DB, statements []string) {
	t.Helper()
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("pgtest fault setup %q: %v", statement, err)
		}
	}
}

func faultName(kind, table string) string {
	return quoteIdentifier("pgtest_" + kind + "_" + table + "_" + randomSuffix()[:8])
}

func quoteLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
