package control

import (
	"context"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/platform/response"
	"gpt-load/internal/state"
	stateloader "gpt-load/internal/state/loader"
	"gpt-load/internal/storage/models"
)

// resolveAccessKeyRecordRequestBody reads the committed default inside the
// creation transaction, never from a possibly stale replica snapshot.
func resolveAccessKeyRecordRequestBody(tx *gorm.DB, explicit *bool) (bool, error) {
	if explicit != nil {
		return *explicit, nil
	}
	var rows []models.SystemSetting
	if err := tx.Where("key = ?", state.SettingDefaultAccessKeyRecordRequestBody).Find(&rows).Error; err != nil {
		return false, app_errors.ParseDBError(err)
	}
	settings, err := stateloader.MapSystemSettings(rows)
	if err != nil {
		return false, app_errors.ErrValidation
	}
	resolved, err := state.ResolveRuntimeSettings(settings)
	if err != nil {
		return false, app_errors.ErrValidation
	}
	return resolved.DefaultAccessKeyRecordRequestBody, nil
}

type AccessKeyRecordRequestBodyResult struct {
	UpdatedCount int64 `json:"updated_count"`
}

// SetAccessKeyRecordRequestBody updates all existing keys, including disabled
// and expired keys. It does not change the default for future keys.
func (s *Service) SetAccessKeyRecordRequestBody(ctx context.Context, enabled bool) (AccessKeyRecordRequestBodyResult, error) {
	var result AccessKeyRecordRequestBodyResult
	_, err := s.writeConfig(ctx, func(tx *gorm.DB) error {
		nowMS, err := safeEpochMilliseconds(s.now())
		if err != nil {
			return err
		}
		updated := tx.Model(&models.AccessKey{}).Where("1 = 1").Updates(map[string]any{
			"record_request_body": enabled,
			"updated_at_ms":       nowMS,
		})
		if updated.Error != nil {
			return app_errors.ParseDBError(updated.Error)
		}
		result.UpdatedCount = updated.RowsAffected
		return nil
	}, nil)
	if err != nil {
		return AccessKeyRecordRequestBodyResult{}, err
	}
	return result, nil
}

func (s *Server) handleSetAccessKeyRecordRequestBody(c *gin.Context) {
	var request struct {
		RecordRequestBody *bool `json:"record_request_body"`
	}
	if err := bindStrictJSON(c, &request); err != nil {
		writeServiceError(c, "access_keys_record_request_body", mapControlJSONError(err))
		return
	}
	if request.RecordRequestBody == nil {
		writeServiceError(c, "access_keys_record_request_body", app_errors.ErrBadRequest)
		return
	}
	result, err := s.service.SetAccessKeyRecordRequestBody(c.Request.Context(), *request.RecordRequestBody)
	if err != nil {
		writeServiceError(c, "access_keys_record_request_body", err)
		return
	}
	response.SuccessI18n(c, "common.success", result)
}
