package control

import (
	"github.com/gin-gonic/gin"

	"gpt-load/internal/platform/response"
	"gpt-load/internal/platform/version"
)

const (
	systemDatabasePostgreSQL      = "postgres"
	systemSecretSourceEnvironment = "environment"
)

type systemDeploymentResponse struct {
	Database string `json:"database"`
}

// Secrets come only from environment variables; their values are never exposed.
type systemSecretResponse struct {
	Source string `json:"source"`
}

type systemInfoResponse struct {
	Version    string                   `json:"version"`
	Deployment systemDeploymentResponse `json:"deployment"`
	AuthKey    systemSecretResponse     `json:"auth_key"`
	Encryption systemSecretResponse     `json:"encryption"`
}

func newSystemInfoResponse() systemInfoResponse {
	return systemInfoResponse{
		Version:    version.Version,
		Deployment: systemDeploymentResponse{Database: systemDatabasePostgreSQL},
		AuthKey:    systemSecretResponse{Source: systemSecretSourceEnvironment},
		Encryption: systemSecretResponse{Source: systemSecretSourceEnvironment},
	}
}

func (s *Server) handleSystemInfo(c *gin.Context) {
	response.SuccessI18n(c, "common.success", s.systemInfo)
}
