package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	gormpostgres "gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"gpt-load/internal/platform/config"
)

var databaseLogger = newDatabaseLogger(os.Stdout)

func newDatabaseLogger(output io.Writer) logger.Interface {
	base := logger.New(log.New(output, "\r\n", log.LstdFlags), logger.Config{
		SlowThreshold:             200 * time.Millisecond,
		LogLevel:                  logger.Warn,
		IgnoreRecordNotFoundError: true,
		ParameterizedQueries:      true,
		Colorful:                  true,
	})
	return databaseLogFilter{Interface: base}
}

type databaseLogFilter struct {
	logger.Interface
}

func (filter databaseLogFilter) LogMode(level logger.LogLevel) logger.Interface {
	return databaseLogFilter{Interface: filter.Interface.LogMode(level)}
}

func (filter databaseLogFilter) Trace(
	ctx context.Context,
	begin time.Time,
	query func() (string, int64),
	err error,
) {
	if ctx != nil &&
		errors.Is(ctx.Err(), context.Canceled) &&
		errors.Is(err, context.Canceled) {
		return
	}
	filter.Interface.Trace(ctx, begin, query, err)
}

func (filter databaseLogFilter) ParamsFilter(
	ctx context.Context,
	query string,
	params ...interface{},
) (string, []interface{}) {
	if paramsFilter, ok := filter.Interface.(gorm.ParamsFilter); ok {
		return paramsFilter.ParamsFilter(ctx, query, params...)
	}
	return query, nil
}

// Open opens the PostgreSQL database at dsn with the default pool limits.
func Open(dsn string) (*gorm.DB, error) {
	return openPostgreSQL(dsn, config.DefaultDatabasePoolConfig())
}

// OpenConfigured opens the database using the process configuration resolved
// by platform/config. Storage never reads environment variables directly.
func OpenConfigured(cfg *config.Config) (*gorm.DB, error) {
	if cfg == nil {
		return nil, fmt.Errorf("open database: configuration is unavailable")
	}
	return openPostgreSQL(cfg.DatabaseDSN, cfg.DatabasePool)
}

func openPostgreSQL(rawDSN string, pool config.DatabasePoolConfig) (*gorm.DB, error) {
	dsn, err := config.ParseDatabaseDSN(rawDSN)
	if err != nil {
		return nil, err
	}
	// Schema migrations rename/rebuild tables while the process is running.
	// pgx's implicit statement cache otherwise can retain a result shape from
	// the legacy table and fail the first query against the rebuilt table.
	dialector := gormpostgres.New(gormpostgres.Config{
		DSN:                  dsn,
		PreferSimpleProtocol: true,
	})
	db, err := gorm.Open(dialector, &gorm.Config{
		Logger:         databaseLogger,
		TranslateError: true,
	})
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL database: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get PostgreSQL connection pool: %w", err)
	}
	sqlDB.SetMaxOpenConns(pool.MaxOpenConnections)
	sqlDB.SetMaxIdleConns(pool.MaxIdleConnections)
	if err := sqlDB.PingContext(context.Background()); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping PostgreSQL database: %w", err)
	}
	return db, nil
}
