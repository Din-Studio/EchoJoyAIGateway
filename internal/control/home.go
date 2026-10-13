package control

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"gpt-load/internal/platform/epochms"
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/platform/response"
	"gpt-load/internal/platform/version"
	"gpt-load/internal/protocol"
	"gpt-load/internal/state"
	"gpt-load/internal/storage/models"
)

type HomeInventory struct {
	GroupCount               int64 `json:"group_count"`
	CredentialCount          int64 `json:"credential_count"`
	AvailableCredentialCount int64 `json:"available_credential_count"`
	ModelCount               int64 `json:"model_count"`
}

type HomeAccessKey struct {
	ID        uint                `json:"id"`
	Name      string              `json:"name"`
	MaskedKey string              `json:"masked_key"`
	Protocols []protocol.Protocol `json:"protocols"`
	Models    []string            `json:"models"`
}

type HomeBase struct {
	Inventory  HomeInventory   `json:"inventory"`
	AccessKeys []HomeAccessKey `json:"access_keys"`
}

type homeResponse struct {
	ServerNowMS int64  `json:"server_now_ms"`
	StartedAtMS int64  `json:"started_at_ms"`
	Version     string `json:"version"`
	HomeBase
}

type homeCredentialRow struct {
	ID                  uint
	GroupID             uint
	ChannelID           string
	ConnectionType      models.ConnectionType
	Params              models.JSON
	Fingerprint         string
	IdentityFingerprint string
	SecretVersion       uint64
	Status              models.CredentialStatus
}

type homeAccessKeyRow struct {
	KeyPrefix             string
	PriceMultiplierMicros *int64
	ID                    uint
	Name                  string
	KeySuffix             string
	Status                string
	Filters               models.JSON
	RPMLimit              int64
	ExpiresAtMS           *int64
	CreatedAtMS           int64
	UpdatedAtMS           int64
	LastRequestAtMS       *int64
}

type homeReadRows struct {
	groupCount  int64
	credentials []homeCredentialRow
	accessKeys  []homeAccessKeyRow
}

func (s *Service) ReadHomeBase(
	ctx context.Context,
	nowMS int64,
) (HomeBase, error) {
	return s.ReadScopedHomeBase(ctx, nowMS, nil)
}

// ReadScopedHomeBase narrows the overview to what the scoped keys can route;
// nil scope reads the whole project. Only active keys count, and a scope
// without one yields an empty overview rather than an error.
func (s *Service) ReadScopedHomeBase(
	ctx context.Context,
	nowMS int64,
	scope []uint,
) (HomeBase, error) {
	if s == nil || s.db == nil || s.manager == nil || s.registrySnapshot == nil {
		return HomeBase{}, fmt.Errorf(
			"read home base: dependencies unavailable: %w",
			app_errors.ErrInternalServer,
		)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateSafeMilliseconds(nowMS); err != nil {
		return HomeBase{}, fmt.Errorf(
			"read home base: invalid observation time: %w",
			app_errors.ErrInternalServer,
		)
	}
	now, err := epochms.ToTime(nowMS)
	if err != nil {
		return HomeBase{}, fmt.Errorf(
			"read home base: invalid observation time: %w",
			app_errors.ErrInternalServer,
		)
	}

	s.writeMu.RLock()
	defer s.writeMu.RUnlock()

	snapshot := s.manager.Current()
	if snapshot == nil {
		return HomeBase{}, fmt.Errorf(
			"read home base: runtime snapshot unavailable: %w",
			app_errors.ErrInternalServer,
		)
	}
	runtimeKeys := s.registrySnapshot()
	rows, err := s.readHomeRows(ctx)
	if err != nil {
		return HomeBase{}, app_errors.ParseDBError(err)
	}

	var allowedGroups map[uint]struct{}
	var scopedKeys []state.AccessKeyView
	if scope != nil {
		scopedKeys = activeScopedKeys(snapshot, scope)
		allowedGroups = make(map[uint]struct{})
		for _, accessKey := range scopedKeys {
			for groupID := range accessibleHomeGroups(snapshot, accessKey) {
				allowedGroups[groupID] = struct{}{}
			}
		}
	}

	inventory := HomeInventory{}
	if scope == nil {
		inventory.GroupCount = rows.groupCount
		inventory.CredentialCount = int64(len(rows.credentials))
		inventory.ModelCount = countHomeModels(snapshot)
	} else {
		inventory.GroupCount = int64(len(allowedGroups))
		inventory.CredentialCount = countHomeCredentials(rows.credentials, allowedGroups)
		inventory.ModelCount = countScopedHomeModels(snapshot, scopedKeys)
	}
	inventory.AvailableCredentialCount, err = countAvailableHomeCredentialsInGroups(
		snapshot,
		rows.credentials,
		runtimeKeys,
		now,
		allowedGroups,
	)
	if err != nil {
		return HomeBase{}, err
	}
	if err := validateHomeInventory(inventory); err != nil {
		return HomeBase{}, err
	}
	accessKeyRows := rows.accessKeys
	if scope != nil {
		accessKeyRows = make([]homeAccessKeyRow, 0, len(scope))
		for _, row := range rows.accessKeys {
			if slices.Contains(scope, row.ID) {
				accessKeyRows = append(accessKeyRows, row)
			}
		}
	}
	accessKeys, err := mapHomeAccessKeys(accessKeyRows, snapshot)
	if err != nil {
		return HomeBase{}, err
	}
	return HomeBase{
		Inventory:  inventory,
		AccessKeys: accessKeys,
	}, nil
}

func (s *Server) handleHome(c *gin.Context) {
	if c.Request.URL.RawQuery != "" || c.Request.URL.ForceQuery {
		writeServiceError(c, "home", app_errors.ErrBadRequest)
		return
	}
	serverNowMS, err := safeEpochMilliseconds(s.now())
	if err != nil {
		writeServiceError(c, "home", err)
		return
	}
	startedAtMS, err := safeEpochMilliseconds(s.startedAt)
	if err != nil {
		writeServiceError(c, "home", err)
		return
	}
	principal, _ := currentPrincipal(c)
	scope, err := s.service.scopedAccessKeyIDs(c.Request.Context(), principal)
	if err != nil {
		writeServiceError(c, "home", err)
		return
	}
	base, err := s.service.ReadScopedHomeBase(c.Request.Context(), serverNowMS, scope)
	if err != nil {
		writeServiceError(c, "home", err)
		return
	}
	response.SuccessI18n(c, "common.success", homeResponse{
		ServerNowMS: serverNowMS,
		StartedAtMS: startedAtMS,
		Version:     version.Version,
		HomeBase:    base,
	})
}

func (s *Service) readHomeRows(
	ctx context.Context,
) (homeReadRows, error) {
	var result homeReadRows
	err := s.withReadSnapshot(ctx, func(tx *gorm.DB) error {
		if err := tx.Model(&models.Group{}).Count(&result.groupCount).Error; err != nil {
			return fmt.Errorf("count home groups: %w", err)
		}
		if err := homeCredentialRowsScope(tx).Find(&result.credentials).Error; err != nil {
			return fmt.Errorf("query home credentials: %w", err)
		}
		if err := tx.Model(&models.AccessKey{}).
			Select(
				"id", "name", "key_prefix", "key_suffix", "status", "filters", "rpm_limit", "price_multiplier_micros",
				"expires_at_ms",
				"created_at_ms", "updated_at_ms",
				"(SELECT MAX(request_logs.completed_at_ms) FROM request_logs WHERE request_logs.access_key_id = access_keys.id) AS last_request_at_ms",
			).
			Where("status = ?", state.AccessKeyStatusActive).
			Order("id ASC").
			Find(&result.accessKeys).Error; err != nil {
			return fmt.Errorf("query home access keys: %w", err)
		}
		return nil
	})
	if err != nil {
		return homeReadRows{}, err
	}
	return result, nil
}

func homeCredentialRowsScope(db *gorm.DB) *gorm.DB {
	homeGroups := db.Session(&gorm.Session{NewDB: true}).
		Model(&models.Group{}).
		Select("id", "channel_id", "connection_type", "params")
	return db.Model(&models.Credential{}).
		Select(
			"credentials.id", "credentials.group_id", "home_groups.channel_id", "home_groups.connection_type", "home_groups.params",
			"credentials.fingerprint", "credentials.identity_fingerprint", "credentials.secret_version", "credentials.status",
		).
		Joins(
			"JOIN (?) AS home_groups ON home_groups.id = credentials.group_id",
			homeGroups,
		).
		Order("credentials.id ASC")
}

func countHomeModels(snapshot *state.ConfigSnapshot) int64 {
	names := make(map[string]struct{})
	for _, group := range snapshot.Groups {
		for _, model := range group.Models {
			name := strings.TrimSpace(model.Alias)
			if name == "" {
				name = strings.TrimSpace(model.ID)
			}
			if name != "" {
				names[name] = struct{}{}
			}
		}
	}
	return int64(len(names))
}

func accessibleHomeGroups(
	snapshot *state.ConfigSnapshot,
	accessKey state.AccessKeyView,
) map[uint]struct{} {
	result := make(map[uint]struct{})
	for groupID, group := range snapshot.Groups {
		if len(accessKey.Filters.Groups) > 0 {
			if _, allowed := accessKey.Filters.Groups[groupID]; !allowed {
				continue
			}
		}
		protocolAllowed := len(accessKey.Filters.Protocols) == 0
		for _, value := range group.ClientProtocols {
			if _, allowed := accessKey.Filters.Protocols[value]; allowed {
				protocolAllowed = true
				break
			}
		}
		if !protocolAllowed {
			continue
		}
		if len(accessKey.Filters.Models) > 0 {
			modelAllowed := false
			for _, model := range group.Models {
				if _, allowed := accessKey.Filters.Models[homeExternalModelName(model)]; allowed {
					modelAllowed = true
					break
				}
			}
			if !modelAllowed {
				continue
			}
		}
		result[groupID] = struct{}{}
	}
	return result
}

// countScopedHomeModels counts the client model names any of keys can route.
func countScopedHomeModels(snapshot *state.ConfigSnapshot, keys []state.AccessKeyView) int64 {
	names := make(map[string]struct{})
	for _, accessKey := range keys {
		for _, name := range scopedHomeModelNames(snapshot, accessibleHomeGroups(snapshot, accessKey), accessKey) {
			names[name] = struct{}{}
		}
	}
	return int64(len(names))
}

func scopedHomeModelNames(
	snapshot *state.ConfigSnapshot,
	allowedGroups map[uint]struct{},
	accessKey state.AccessKeyView,
) []string {
	names := make(map[string]struct{})
	for groupID := range allowedGroups {
		group, exists := snapshot.Groups[groupID]
		if !exists {
			continue
		}
		for _, model := range group.Models {
			name := homeExternalModelName(model)
			if name == "" {
				continue
			}
			if len(accessKey.Filters.Models) > 0 {
				if _, allowed := accessKey.Filters.Models[name]; !allowed {
					continue
				}
			}
			names[name] = struct{}{}
		}
	}
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func homeExternalModelName(model state.ModelConfig) string {
	if alias := strings.TrimSpace(model.Alias); alias != "" {
		return alias
	}
	return strings.TrimSpace(model.ID)
}

func countHomeCredentials(
	rows []homeCredentialRow,
	allowedGroups map[uint]struct{},
) int64 {
	var count int64
	for _, row := range rows {
		if _, allowed := allowedGroups[row.GroupID]; allowed {
			count++
		}
	}
	return count
}

func countAvailableHomeCredentials(
	snapshot *state.ConfigSnapshot,
	persisted []homeCredentialRow,
	runtime []state.CredentialRuntimeView,
	now time.Time,
) (int64, error) {
	return countAvailableHomeCredentialsInGroups(snapshot, persisted, runtime, now, nil)
}

func countAvailableHomeCredentialsInGroups(
	snapshot *state.ConfigSnapshot,
	persisted []homeCredentialRow,
	runtime []state.CredentialRuntimeView,
	now time.Time,
	allowedGroups map[uint]struct{},
) (int64, error) {
	runtimeByID := make(map[uint]state.CredentialRuntimeView, len(runtime))
	for _, credential := range runtime {
		if credential.ID == 0 {
			return 0, fmt.Errorf(
				"count available home credentials: runtime credential has zero id: %w",
				app_errors.ErrInternalServer,
			)
		}
		if _, duplicate := runtimeByID[credential.ID]; duplicate {
			return 0, fmt.Errorf(
				"count available home credentials: duplicate runtime credential %d: %w",
				credential.ID,
				app_errors.ErrInternalServer,
			)
		}
		runtimeByID[credential.ID] = credential
	}

	seen := make(map[uint]struct{}, len(persisted))
	var available int64
	for _, row := range persisted {
		if row.ID == 0 {
			return 0, fmt.Errorf(
				"count available home credentials: persisted credential has zero id: %w",
				app_errors.ErrInternalServer,
			)
		}
		if _, duplicate := seen[row.ID]; duplicate {
			return 0, fmt.Errorf(
				"count available home credentials: duplicate persisted credential %d: %w",
				row.ID,
				app_errors.ErrInternalServer,
			)
		}
		seen[row.ID] = struct{}{}
		group, groupExists := snapshot.GroupCatalog[row.GroupID]
		credential, credentialExists := runtimeByID[row.ID]
		if !groupExists || !credentialExists || credential.GroupID != row.GroupID {
			return 0, fmt.Errorf(
				"count available home credential %d: runtime configuration mismatch: %w",
				row.ID,
				app_errors.ErrInternalServer,
			)
		}

		var status state.CredentialStatus
		switch row.Status {
		case models.CredentialStatusActive:
			status = state.CredentialStatusActive
		case models.CredentialStatusDisabled:
			status = state.CredentialStatusDisabled
		default:
			return 0, fmt.Errorf(
				"count available home credential %d: invalid persisted status: %w",
				row.ID,
				app_errors.ErrInternalServer,
			)
		}
		if credential.Status != status ||
			credential.Version != groupCollectionCredentialVersion(row.SecretVersion) ||
			credential.IdentityGeneration != groupCollectionCredentialIdentity(
				row.IdentityFingerprint,
				models.Group{
					ID: row.GroupID, ChannelID: row.ChannelID,
					ConnectionType: row.ConnectionType, Params: row.Params,
				},
			) {
			return 0, fmt.Errorf(
				"count available home credential %d: runtime identity mismatch: %w",
				row.ID,
				app_errors.ErrInternalServer,
			)
		}
		_, groupAllowed := allowedGroups[row.GroupID]
		// 与健康页共用 classifyHealthKey：只看 status/拉黑/冷却会把「待重新授权」
		// 和「权重手动置 0」的凭据算成可用，而调度器根本不会选中它们。
		if (allowedGroups == nil || groupAllowed) &&
			classifyHealthKey(group, credential, now) == healthBucketAvailable {
			available++
		}
	}
	if len(runtimeByID) != len(seen) {
		return 0, fmt.Errorf(
			"count available home credentials: runtime credential set mismatch: %w",
			app_errors.ErrInternalServer,
		)
	}
	return available, nil
}

func mapHomeAccessKeys(
	rows []homeAccessKeyRow,
	snapshot *state.ConfigSnapshot,
) ([]HomeAccessKey, error) {
	result := make([]HomeAccessKey, 0, len(rows))
	for _, row := range rows {
		if uint64(row.ID) > uint64(maxSafeInteger) {
			return nil, fmt.Errorf(
				"map home access key %d: unsafe id: %w",
				row.ID,
				app_errors.ErrInternalServer,
			)
		}
		if !validAccessKeyPrefix(row.KeyPrefix) || !validAccessKeySuffix(row.KeySuffix) {
			return nil, fmt.Errorf(
				"map home access key %d: invalid persisted suffix: %w",
				row.ID,
				app_errors.ErrInternalServer,
			)
		}
		filters, err := decodeStoredAccessKeyFilters(row.Filters)
		if err != nil {
			return nil, fmt.Errorf(
				"map home access key %d filters: %w",
				row.ID,
				app_errors.ErrInternalServer,
			)
		}
		protocols := filters.Protocols
		if len(protocols) == 0 {
			protocols = protocol.DataPlaneProtocols()
		} else {
			protocols = append([]protocol.Protocol(nil), protocols...)
		}
		accessKey, exists := snapshot.AccessKeysByID[row.ID]
		if !exists {
			return nil, fmt.Errorf(
				"map home access key %d: runtime configuration mismatch: %w",
				row.ID,
				app_errors.ErrInternalServer,
			)
		}
		// 行是 active 而快照不是：团队授权为空的 key 编译为 disabled，不能用于连接。
		if accessKey.Status != state.AccessKeyStatusActive {
			continue
		}
		models := scopedHomeModelNames(
			snapshot,
			accessibleHomeGroups(snapshot, accessKey),
			accessKey,
		)
		result = append(result, HomeAccessKey{
			ID: row.ID, Name: row.Name,
			MaskedKey: maskedAccessKey(row.KeyPrefix, row.KeySuffix),
			Protocols: protocols,
			Models:    models,
		})
	}
	return result, nil
}

func validateHomeInventory(value HomeInventory) error {
	for _, field := range []struct {
		name  string
		value int64
	}{
		{name: "group count", value: value.GroupCount},
		{name: "credential count", value: value.CredentialCount},
		{
			name:  "available credential count",
			value: value.AvailableCredentialCount,
		},
		{name: "model count", value: value.ModelCount},
	} {
		if field.value < 0 || field.value > maxSafeInteger {
			return fmt.Errorf(
				"map home inventory %s: unsafe integer: %w",
				field.name,
				app_errors.ErrInternalServer,
			)
		}
	}
	return nil
}
