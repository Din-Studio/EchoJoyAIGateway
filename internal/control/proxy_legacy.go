package control

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"gorm.io/gorm"

	"gpt-load/internal/outboundproxy"
	stateloader "gpt-load/internal/state/loader"
	"gpt-load/internal/storage/models"
)

// migrateLegacyProxyOverrides runs before Loader. Catalog mutations use the
// same transaction lock, so concurrent startups reuse the first committed IDs.
// Existing ciphertext is copied, not re-encrypted, into the catalog.
func (s *Service) migrateLegacyProxyOverrides(ctx context.Context) error {
	noChanges := errors.New("no legacy proxy overrides")
	err := s.withControlTransaction(ctx, func(tx *gorm.DB) error {
		if err := lockProxyCatalog(tx); err != nil {
			return err
		}
		catalog, err := stateloader.LoadProxyCatalog(ctx, tx, s.encryption)
		if err != nil {
			return err
		}
		byFingerprint := make(map[string]uint, len(catalog))
		for id, value := range catalog {
			byFingerprint[value.Row.Fingerprint] = id
		}
		convert := func(ciphertext string) (string, bool, error) {
			if s.encryption == nil {
				return "", false, fmt.Errorf("legacy proxy encryption is unavailable")
			}
			plaintext, err := s.encryption.Decrypt(ciphertext)
			if err != nil {
				return "", false, fmt.Errorf("decrypt legacy proxy")
			}
			config, err := outboundproxy.Decode(plaintext)
			if err != nil {
				return "", false, fmt.Errorf("decode legacy proxy")
			}
			if config.Mode != outboundproxy.ModeCustom || config.ProxyID != 0 {
				return ciphertext, false, nil
			}
			identity, err := outboundproxy.CanonicalConnection(config)
			if err != nil {
				return "", false, err
			}
			canonical, err := outboundproxy.Encode(identity)
			if err != nil {
				return "", false, err
			}
			fingerprint := s.encryption.Hash(canonical)
			id := byFingerprint[fingerprint]
			if id == 0 {
				endpoint, err := url.Parse(config.URL)
				if err != nil {
					return "", false, fmt.Errorf("parse legacy proxy")
				}
				row := models.Proxy{Name: catalog.SuggestedName(endpoint.Host), Config: ciphertext, Fingerprint: fingerprint, Enabled: true}
				if err := tx.Create(&row).Error; err != nil {
					return "", false, err
				}
				id = row.ID
				byFingerprint[fingerprint] = id
				catalog[id] = stateloader.ManagedProxy{Row: row}
			}
			reference, err := outboundproxy.Encode(outboundproxy.Config{Mode: outboundproxy.ModeCustom, ProxyID: id})
			if err != nil {
				return "", false, err
			}
			encoded, err := s.encryption.Encrypt(reference)
			return encoded, true, err
		}
		changedAny := false
		var settings []models.SystemSetting
		if err := tx.Where(&models.SystemSetting{Key: outboundproxy.SystemSettingKey}).Find(&settings).Error; err != nil {
			return err
		}
		for _, row := range settings {
			value, changed, err := convert(row.Value)
			if err != nil {
				return err
			}
			if changed {
				result := tx.Model(&models.SystemSetting{}).
					Where("key = ? AND value = ?", row.Key, row.Value).UpdateColumn("value", value)
				if result.Error != nil {
					return result.Error
				}
				if result.RowsAffected != 1 {
					return fmt.Errorf("legacy global proxy changed during conversion")
				}
				changedAny = true
			}
		}
		for _, table := range []string{"groups", "credentials"} {
			var rows []struct {
				ID          uint
				ProxyConfig string
			}
			if err := tx.Table(table).Select("id", "proxy_config").Where("proxy_config IS NOT NULL").Order("id ASC").Find(&rows).Error; err != nil {
				return err
			}
			for _, row := range rows {
				value, changed, err := convert(row.ProxyConfig)
				if err != nil {
					return err
				}
				if changed {
					result := tx.Table(table).Where("id = ? AND proxy_config = ?", row.ID, row.ProxyConfig).UpdateColumn("proxy_config", value)
					if result.Error != nil {
						return result.Error
					}
					if result.RowsAffected != 1 {
						return fmt.Errorf("legacy %s proxy changed during conversion", table)
					}
					changedAny = true
				}
			}
		}
		if !changedAny {
			// Roll back the read-only transaction before the revision bump.
			return noChanges
		}
		return nil
	})
	if errors.Is(err, noChanges) {
		return nil
	}
	return err
}
