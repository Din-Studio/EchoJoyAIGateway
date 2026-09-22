package app

import (
	"context"
	"fmt"
	"sync"

	"gorm.io/gorm"

	"gpt-load/internal/cluster"
)

// readinessProbe pings the database and Redis in parallel. Cluster mode makes
// both hard dependencies, so /health only reports ready when both answer.
type readinessProbe struct {
	db     *gorm.DB
	client *cluster.Client
}

// NewReadinessProbe returns nil outside cluster mode so /health keeps its
// static single-instance response.
func NewReadinessProbe(db *gorm.DB, client *cluster.Client) ReadinessProbe {
	if client == nil {
		return nil
	}
	return readinessProbe{db: db, client: client}
}

func (probe readinessProbe) Check(ctx context.Context) map[string]error {
	checks := map[string]func(context.Context) error{
		"database": probe.pingDatabase,
		"redis":    probe.client.Ping,
	}
	results := make(map[string]error, len(checks))
	var mu sync.Mutex
	var wait sync.WaitGroup
	for name, check := range checks {
		wait.Add(1)
		go func() {
			defer wait.Done()
			err := check(ctx)
			mu.Lock()
			results[name] = err
			mu.Unlock()
		}()
	}
	wait.Wait()
	return results
}

func (probe readinessProbe) pingDatabase(ctx context.Context) error {
	if probe.db == nil {
		return fmt.Errorf("database is unavailable")
	}
	sqlDB, err := probe.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.PingContext(ctx)
}
