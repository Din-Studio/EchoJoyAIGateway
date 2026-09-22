package control

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"gorm.io/gorm"

	"gpt-load/internal/cluster"
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/storage/models"
)

// clusterConfigRevisionKey stores the monotonically increasing configuration
// revision that cluster peers compare against. The _internal. prefix keeps the
// row out of the compiled ConfigSnapshot.
const clusterConfigRevisionKey = models.InternalSystemSettingPrefix + "cluster.config_revision"

// configEventPublisher is the control-plane view of cluster.ConfigEventBus.
type configEventPublisher interface {
	Publish(context.Context, cluster.ConfigChange) error
	InstanceID() string
}

// bumpClusterConfigRevision increments the revision inside the committing
// transaction so the new value is only visible together with the change. The
// upserted row is locked for the rest of the transaction, which serializes
// cluster control-plane commits and keeps the revision strictly monotonic.
func bumpClusterConfigRevision(tx *gorm.DB, nowMS int64) (uint64, error) {
	var raw string
	err := tx.Raw(
		`INSERT INTO system_settings (key, value, updated_at_ms) VALUES (?, '1', ?)
		 ON CONFLICT (key) DO UPDATE
		 SET value = CAST(CAST(system_settings.value AS BIGINT) + 1 AS TEXT),
		     updated_at_ms = excluded.updated_at_ms
		 RETURNING value`,
		clusterConfigRevisionKey, nowMS,
	).Scan(&raw).Error
	if err != nil {
		return 0, fmt.Errorf("bump cluster config revision: %w", app_errors.ParseDBError(err))
	}
	revision, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bump cluster config revision: invalid stored value %q", raw)
	}
	return revision, nil
}

// readClusterConfigRevision returns the committed revision, or 0 when no
// cluster commit has happened yet.
func readClusterConfigRevision(ctx context.Context, db *gorm.DB) (uint64, error) {
	var setting models.SystemSetting
	err := db.WithContext(ctx).
		Where("key = ?", clusterConfigRevisionKey).
		Take(&setting).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read cluster config revision: %w", app_errors.ParseDBError(err))
	}
	revision, err := strconv.ParseUint(setting.Value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("read cluster config revision: invalid stored value %q", setting.Value)
	}
	return revision, nil
}
