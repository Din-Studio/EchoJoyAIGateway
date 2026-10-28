package requestlog

import "gorm.io/gorm"

const (
	auditStatusColumn = "request_logs.request_audit::jsonb->>'status'"
	// 取样审查不缺规则结果，最终处置按放行展示；其余未完成都是审查失败。
	auditTruncatedCondition = "COALESCE(request_logs.request_audit::jsonb->>'reason', '') = 'content_truncated'"
)

// 直接筛选日志中冻结的审查结果，规则删除或改名后仍可查询历史记录。
// allowed/failed 与管理 API 的最终处置一致；incomplete 保留按内部状态查询的语义。
func applyRequestAuditFilters(query *gorm.DB, input ListQuery) *gorm.DB {
	switch input.AuditStatus {
	case "":
	case "allowed":
		query = query.Where(auditStatusColumn + " = 'passed' OR (" + auditStatusColumn + " = 'incomplete' AND " + auditTruncatedCondition + ")")
	case "failed":
		query = query.Where(auditStatusColumn + " = 'incomplete' AND NOT " + auditTruncatedCondition)
	default:
		query = query.Where(auditStatusColumn+" = ?", input.AuditStatus)
	}
	if input.AuditRule != "" {
		// 使用字面子串匹配，百分号、下划线等字符不会被当成通配符。
		query = query.Where(`EXISTS (SELECT 1 FROM jsonb_array_elements(COALESCE(NULLIF(request_logs.request_audit::jsonb->'findings', 'null'::jsonb), '[]'::jsonb)) AS audit_finding(value)
			WHERE strpos(LOWER(audit_finding.value->>'name'), LOWER(?)) > 0)`, input.AuditRule)
	}
	return query
}
