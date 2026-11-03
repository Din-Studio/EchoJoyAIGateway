package storage

import (
	"fmt"
	"testing"

	"gorm.io/gorm"
)

func TestExternalClientIPMigrationContract(t *testing.T) {
	testClientIPMigration(t, externalMigrationContract(t))
}

func testClientIPMigration(t *testing.T, open func(*testing.T) *gorm.DB) {
	for _, scenario := range []string{"fresh", "upgrade", "interrupted_column", "interrupted_complete"} {
		t.Run(scenario, func(t *testing.T) {
			db := open(t)
			if len(migrations) < 30 {
				t.Fatal("client IP migration is missing")
			}
			if scenario != "fresh" {
				if err := applyMigrationRegistry(db, migrations[:29]); err != nil {
					t.Fatal(err)
				}
				if err := db.Table("request_logs").Create(map[string]any{
					"id": "existing", "first_output_ms": 100, "last_output_ms": 800, "completed_at_ms": 1, "access_key_id": 1, "protocol": "openai-responses", "client_model": "test", "upstream_model": "test", "status": "success", "status_code": 200, "duration_ms": 900, "error_summary": "",
				}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "interrupted_column" || scenario == "interrupted_complete" {
				entry := migrations[29]
				up := entry.Up
				entry.Up = func(tx *gorm.DB) error {
					if scenario == "interrupted_column" {
						if err := tx.Exec("ALTER TABLE request_logs ADD COLUMN client_ip VARCHAR(45) NULL").Error; err != nil {
							return err
						}
					} else if err := up(tx); err != nil {
						return err
					}
					return fmt.Errorf("interrupt client IP DDL")
				}
				registry := append(append([]migration(nil), migrations[:29]...), entry)
				if err := applyMigrationRegistry(db, registry); err == nil {
					t.Fatal("expected migration interruption")
				}
				if db.Migrator().HasColumn("request_logs", "client_ip") {
					t.Fatal("interrupted PostgreSQL migration did not roll back")
				}
			}
			if err := AutoMigrate(db); err != nil {
				t.Fatal(err)
			}
			if !db.Migrator().HasColumn("request_logs", "client_ip") || !db.Migrator().HasIndex("request_logs", "idx_request_logs_ip_completed_id") {
				t.Fatal("client IP schema is incomplete")
			}
			if scenario != "fresh" {
				var row struct {
					ClientIP      *string
					DurationMs    int64
					FirstOutputMs int64
					LastOutputMs  int64
				}
				if err := db.Table("request_logs").Where("id = ?", "existing").Take(&row).Error; err != nil {
					t.Fatal(err)
				}
				if row.ClientIP != nil || row.DurationMs != 900 || row.FirstOutputMs != 100 || row.LastOutputMs != 800 {
					t.Fatalf("historical data changed: %+v", row)
				}
				for _, ip := range []string{"192.0.2.1", "2001:db8:1234:5678:9abc:def0:1234:5678"} {
					if err := db.Table("request_logs").Where("id = ?", "existing").Update("client_ip", ip).Error; err != nil {
						t.Fatal(err)
					}
					var count int64
					if err := db.Table("request_logs").Where("client_ip = ?", ip).Count(&count).Error; err != nil || count != 1 {
						t.Fatalf("exact IP query count=%d, error=%v", count, err)
					}
				}
			}
			if err := AutoMigrate(db); err != nil {
				t.Fatal(err)
			}
		})
	}
}
