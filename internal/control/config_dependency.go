package control

import (
	"errors"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"gpt-load/internal/automodel"
	"gpt-load/internal/configdependency"
	"gpt-load/internal/jev"
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/platform/i18n"
	"gpt-load/internal/platform/response"
	"gpt-load/internal/requestaudit"
)

// mapConfigCompileError preserves dependency identifiers without exposing raw
// compiler errors, channel parameters, credentials, or encrypted settings.
func mapConfigCompileError(err error) error {
	var dependency *configdependency.Error
	if errors.As(err, &dependency) {
		return app_errors.NewAPIErrorWithData(app_errors.ErrConfigDependencyConflict, dependency)
	}
	if errors.Is(err, automodel.ErrInvalidConfig) || errors.Is(err, jev.ErrInvalidConfig) || errors.Is(err, requestaudit.ErrInvalidConfig) {
		return app_errors.ErrValidation
	}
	return err
}

func writeConfigDependencyError(c *gin.Context, apiErr *app_errors.APIError) bool {
	if apiErr.Code != app_errors.ErrConfigDependencyConflict.Code {
		return false
	}
	dependency, ok := apiErr.Data.(*configdependency.Error)
	if !ok {
		return false
	}
	features := make([]string, 0, len(dependency.Features))
	for _, feature := range dependency.Features {
		switch feature {
		case "auto_model", "request_audit":
			features = append(features, i18n.Message(c, "config_dependency.feature."+feature))
		}
	}
	group := i18n.Message(c, "config_dependency.any_group")
	if dependency.GroupID != 0 {
		group = strconv.FormatUint(uint64(dependency.GroupID), 10)
	}
	messageID := "config_dependency.conflict"
	switch dependency.Reason {
	case configdependency.JevRouteRequired, configdependency.JevRouteUnavailable,
		configdependency.AutoModelTargetMissing, configdependency.AutoModelNameConflict,
		configdependency.AutoModelFallbackMissing, configdependency.AutoModelRecursiveTarget:
		messageID = "config_dependency." + string(dependency.Reason)
	}
	// Text templates interpolate only public identifiers. The browser renders
	// the resulting message as text, not HTML.
	responseData := map[string]any{
		"Features": strings.Join(features, " / "), "Group": group,
		"Model": dependency.Model, "AutoModel": dependency.AutoModel, "Preset": dependency.Preset,
	}
	// Keep the same envelope and language negotiation as all control errors.
	response.ErrorI18nFromAPIError(c, apiErr, messageID, responseData)
	return true
}
