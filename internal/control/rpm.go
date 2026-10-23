package control

import (
	"cmp"
	"context"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"

	"gpt-load/internal/cluster"
	app_errors "gpt-load/internal/platform/errors"
	"gpt-load/internal/platform/response"
	"gpt-load/internal/storage/models"
)

// rpmPeaks decorates a listing; without the shared statistics the listing is
// still served, just without peaks.
func (s *Service) rpmPeaks(ctx context.Context, kind cluster.RPMKind, ids []uint) map[uint]int64 {
	peaks, err := s.rpmStats.Peaks(ctx, kind, ids)
	if err != nil {
		logrus.WithError(err).WithField("event", "rpm_stats.peaks_failed").Warn("RPM peaks are unavailable")
		return nil
	}
	return peaks
}

// 已观测到的零值排在无数据之前；相同峰值交给后续排序条件。
func compareRPMPeaks(left, right *int64) int {
	if left == nil && right == nil {
		return 0
	}
	if left == nil {
		return -1
	}
	if right == nil {
		return 1
	}
	return cmp.Compare(*left, *right)
}

func (s *Server) handleAccessKeyRPM(c *gin.Context) {
	id, ok := accessKeyID(c)
	if !ok {
		return
	}
	var key models.AccessKey
	if err := s.service.db.WithContext(c.Request.Context()).Select("id").Take(&key, id).Error; err != nil {
		writeServiceError(c, "get_access_key", app_errors.ParseDBError(err))
		return
	}
	s.writeRPM(c, cluster.RPMAccessKey, id)
}

func (s *Server) handleCredentialRPM(c *gin.Context) {
	group, ok := groupID(c, "get_group_credential")
	if !ok {
		return
	}
	id, ok := credentialID(c, "get_group_credential")
	if !ok {
		return
	}
	var credential models.Credential
	if err := s.service.db.WithContext(c.Request.Context()).Select("id").Where("group_id = ? AND id = ?", group, id).Take(&credential).Error; err != nil {
		writeServiceError(c, "get_group_credential", app_errors.ParseDBError(err))
		return
	}
	s.writeRPM(c, cluster.RPMCredential, id)
}

func (s *Server) writeRPM(c *gin.Context, kind cluster.RPMKind, id uint) {
	if c.Request.URL.RawQuery != "" || c.Request.URL.ForceQuery {
		writeServiceError(c, "query_rpm", app_errors.ErrBadRequest)
		return
	}
	result, err := s.service.rpmStats.Report(c.Request.Context(), kind, id)
	if err != nil {
		writeServiceError(c, "query_rpm", err)
		return
	}
	response.SuccessI18n(c, "common.success", result)
}
