package gateway

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"

	"gpt-load/internal/accessquota"
	"gpt-load/internal/platform/utils"
	"gpt-load/internal/pricing"
)

// AccessKeyUsageReader 查询长期保留的 AccessKey 累计预估成本（指定币种的纳单位）。
type AccessKeyUsageReader interface {
	QueryAccessKeyTotalCost(context.Context, uint, pricing.CurrencyCode) (int64, error)
}

// accessKeyUsageResponse 对齐 CC Switch 通用模板；金额是系统币种的预估费用。
type accessKeyUsageResponse struct {
	IsActive bool    `json:"is_active"`
	Balance  float64 `json:"balance"`
	Used     float64 `json:"used"`
	Total    float64 `json:"total"`
}

func (handler *Handler) handleUsage(c *gin.Context, request *dataPlaneRequestContext) {
	c.Header("Cache-Control", "no-store")
	result := accessKeyUsageResponse{IsActive: true}
	// 没有额度规则时 View 不访问 Redis，直接返回空视图。
	view, err := handler.accessQuota.View(
		c.Request.Context(), request.snapshot, request.accessKey.ID, handler.quotaNow(),
	)
	if err != nil {
		handler.completeLimitStateFailure(c, nil, "access_quota.view", err)
		return
	}
	var totalRule *accessquota.RuleView
	for index := range view.Rules {
		if view.Rules[index].Kind == accessquota.KindTotal {
			totalRule = &view.Rules[index]
			break
		}
	}
	if totalRule != nil {
		result.Total = float64(totalRule.LimitNano) / 1e9
		result.Used = float64(totalRule.UsedNano) / 1e9
		result.Balance = float64(totalRule.RemainingNano) / 1e9
	} else {
		used, err := handler.usageReader.QueryAccessKeyTotalCost(
			c.Request.Context(), request.accessKey.ID, request.snapshot.Settings.Currency.Code,
		)
		if err != nil {
			utils.LogPlaneBestEffort(handler.logger, logrus.WarnLevel, utils.LogPlaneData,
				logrus.Fields{"event": "access_key_usage_query_failed", "access_key_id": request.accessKey.ID, "error": err.Error()},
				"Access key usage query failed")
			handler.writeUsageUnavailable(c)
			return
		}
		result.Used = float64(used) / 1e9
	}
	c.JSON(http.StatusOK, result)
}

func (handler *Handler) writeUsageUnavailable(c *gin.Context) {
	_ = handler.writeReason(c, reason{Status: http.StatusServiceUnavailable,
		Code: "usage_unavailable", Message: "Usage is temporarily unavailable."})
}
