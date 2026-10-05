package control

import (
	"github.com/gin-gonic/gin"

	"gpt-load/internal/platform/config"
	"gpt-load/internal/platform/response"
	"gpt-load/internal/platform/version"
)

const (
	systemInstanceModeSingle       = "single"
	systemDatabaseSQLite           = "sqlite"
	systemDatabaseMySQL            = "mysql"
	systemDatabasePostgreSQL       = "postgres"
	systemDistributionSingleBinary = "single_binary"
	systemSecretSourceEnvironment  = "environment"
)

type systemDeploymentResponse struct {
	InstanceMode string `json:"instance_mode"`
	Database     string `json:"database"`
	Distribution string `json:"distribution"`
}

// Secrets come only from environment variables, so no source has a file path.
type systemSecretResponse struct {
	Source string  `json:"source"`
	Path   *string `json:"path"`
}

type systemEncryptionResponse struct {
	Enabled bool    `json:"enabled"`
	Source  string  `json:"source"`
	Path    *string `json:"path"`
}

type systemInfoResponse struct {
	Version    string                   `json:"version"`
	Deployment systemDeploymentResponse `json:"deployment"`
	AuthKey    systemSecretResponse     `json:"auth_key"`
	Encryption systemEncryptionResponse `json:"encryption"`
}

func newSystemInfoResponse(cfg *config.Config) systemInfoResponse {
	database := systemDatabaseSQLite
	switch cfg.DatabaseMetadata.Driver {
	case config.DatabaseDriverMySQL:
		database = systemDatabaseMySQL
	case config.DatabaseDriverPostgreSQL:
		database = systemDatabasePostgreSQL
	case config.DatabaseDriverSQLite:
		database = systemDatabaseSQLite
	}
	return systemInfoResponse{
		Version: version.Version,
		Deployment: systemDeploymentResponse{
			InstanceMode: systemInstanceModeSingle,
			Database:     database,
			Distribution: systemDistributionSingleBinary,
		},
		AuthKey: systemSecretResponse{Source: systemSecretSourceEnvironment},
		Encryption: systemEncryptionResponse{
			Enabled: true,
			Source:  systemSecretSourceEnvironment,
		},
	}
}

func (s *Server) handleSystemInfo(c *gin.Context) {
	response.SuccessI18n(c, "common.success", s.systemInfo)
}
