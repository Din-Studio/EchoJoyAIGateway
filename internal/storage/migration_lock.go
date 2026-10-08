package storage

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
)

const (
	migrationLockName    = "gpt-load:schema-migrations:v2"
	migrationLockTimeout = time.Minute
	migrationLockRetry   = 100 * time.Millisecond
)

func acquireMigrationLock(db *gorm.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), migrationLockTimeout)
	defer cancel()
	return acquirePostgresMigrationLock(ctx, db, migrationLockRetry)
}

func acquirePostgresMigrationLock(ctx context.Context, db *gorm.DB, retryInterval time.Duration) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("acquire PostgreSQL migration lock: %w", err)
	}
	if db == nil {
		return fmt.Errorf("acquire PostgreSQL migration lock: database is nil")
	}
	if retryInterval <= 0 {
		return fmt.Errorf("acquire PostgreSQL migration lock: retry interval must be positive")
	}

	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()
	for {
		var acquired bool
		if err := db.WithContext(ctx).
			Raw("SELECT pg_try_advisory_lock(hashtext(?))", migrationLockName).
			Scan(&acquired).Error; err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fmt.Errorf("acquire PostgreSQL migration lock: %w", ctxErr)
			}
			return fmt.Errorf("acquire PostgreSQL migration lock: %w", err)
		}
		if acquired {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("acquire PostgreSQL migration lock: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func releaseMigrationLock(db *gorm.DB) error {
	var released bool
	if err := db.Raw("SELECT pg_advisory_unlock(hashtext(?))", migrationLockName).Scan(&released).Error; err != nil {
		return fmt.Errorf("release PostgreSQL migration lock: %w", err)
	}
	if !released {
		return fmt.Errorf("release PostgreSQL migration lock: lock was not held")
	}
	return nil
}
