package migrations_test

import (
	"testing"

	"gorm.io/gorm"

	"gpt-load/internal/storage/migrations"
)

func TestRequestAuditMigrationIsIdempotentAndEnforcesNonNegativeCosts(t *testing.T) {
	t.Parallel()
	db := openInitialTestDatabase(t)
	for _, up := range []func(*gorm.DB) error{
		migrations.Up0001, migrations.Up0002, migrations.Up0003, migrations.Up0004, migrations.Up0005,
		migrations.Up0006, migrations.Up0007, migrations.Up0008, migrations.Up0009, migrations.Up0010,
		migrations.Up0011, migrations.Up0012, migrations.Up0013, migrations.Up0014, migrations.Up0015,
		migrations.Up0016, migrations.Up0017, migrations.Up0018, migrations.Up0019, migrations.Up0020,
		migrations.Up0021, migrations.Up0022, migrations.Up0023, migrations.Up0024,
	} {
		if err := up(db); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrations.Validate0025(db); err == nil {
		t.Fatal("missing request audit columns accepted")
	}
	for range 2 {
		if err := migrations.Up0025(db); err != nil {
			t.Fatal(err)
		}
	}
	for _, column := range []string{"audit_cost_nano_usd", "audit_cost_nano"} {
		var count int64
		if err := db.Raw("SELECT count(*) FROM pg_constraint WHERE conrelid = 'request_logs'::regclass AND pg_get_constraintdef(oid) LIKE ?", "%"+column+" >= 0%").Scan(&count).Error; err != nil || count != 1 {
			t.Fatalf("%s check constraint count = %d, %v", column, count, err)
		}
	}
}
