package requestlog

import "gorm.io/gorm"

// 直接筛选日志中冻结的审查结果，规则删除或改名后仍可查询历史记录。
func applyRequestAuditFilters(query *gorm.DB, input ListQuery) *gorm.DB {
	if input.AuditStatus != "" {
		query = query.Where("request_logs.request_audit::jsonb->>'status' = ?", input.AuditStatus)
	}
	if input.AuditRule != "" {
		// 使用字面子串匹配，百分号、下划线等字符不会被当成通配符。
		query = query.Where(`EXISTS (SELECT 1 FROM jsonb_array_elements(COALESCE(NULLIF(request_logs.request_audit::jsonb->'findings', 'null'::jsonb), '[]'::jsonb)) AS audit_finding(value)
			WHERE strpos(LOWER(audit_finding.value->>'name'), LOWER(?)) > 0)`, input.AuditRule)
	}
	return query
}
