package importfile

import "gpt-load/internal/channel"

// 导入字段包含认证所需的临时字段，持久化仍由各渠道生成固定凭据结构。
func cpaImportFields(channelID channel.ID) []string {
	switch channelID {
	case channel.Codex:
		return []string{
			"type", "access_token", "refresh_token", "id_token", "account_id", "email",
			"plan_type", "expired", "last_refresh",
		}
	case channel.Claude:
		return []string{
			"type", "access_token", "refresh_token", "id_token", "account_uuid", "email",
			"organization_uuid", "organization_name", "claude_device_ids", "expired", "last_refresh",
		}
	case channel.Antigravity:
		return []string{
			"type", "access_token", "refresh_token", "account_id", "email", "project_id",
			"expires_in", "timestamp", "expired", "last_refresh",
		}
	case channel.Grok:
		return []string{
			"type", "access_token", "refresh_token", "id_token", "account_id", "sub", "email",
			"token_type", "expires_in", "expired", "last_refresh", "token_endpoint", "auth_kind",
		}
	default:
		return nil
	}
}

func mapCPA(object map[string]any, channelID channel.ID) (map[string]any, string) {
	// 先检查原始条目的大小，不能通过丢弃元数据绕过限制。
	if tooLarge(object) {
		return nil, "credential_too_large"
	}
	m := newMapper(stringValue(object["type"]))
	// OAuth 客户端标识影响认证兼容性，必须先验证，再从导入结果中丢弃。
	if _, present := object["client_id"]; present {
		if client, ok := optionalString(object, "client_id"); !ok || client == "" {
			return nil, "invalid_credential"
		}
		m.validateClient(object, channelID)
	}
	for _, field := range cpaImportFields(channelID) {
		if value, present := object[field]; present {
			m.out[field] = value
		}
	}
	// 保留白名单字段的原始类型和值，由渠道执行既有的完整性和身份校验。
	// 其他字段既不校验业务结构，也不进入暂存或 CPA 执行器。
	m.requireRefresh()
	return m.result()
}
