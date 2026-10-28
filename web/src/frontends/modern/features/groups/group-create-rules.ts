import type { CredentialEntryInput, GroupChannel, ModelDraft } from '@modern/api/group-create'
import type { ApiError } from '@shared/http/errors'

export interface GroupDraftModel extends ModelDraft {
  key: number
  origin: 'manual' | 'discovery' | 'configured'
}
export function groupConnectionParams(
  params: Readonly<Record<string, string>>,
  channel: GroupChannel | undefined,
): Record<string, string> {
  // 可选地址留空时省略字段，让后端使用渠道默认值。
  const optionalBaseURL = channel?.fields.some(
    (field) => field.key === 'base_url' && !field.required,
  )
  return Object.fromEntries(
    Object.entries(params)
      .map(([key, value]) => [key, value.trim()])
      .filter(([key, value]) => key !== 'base_url' || value || !optionalBaseURL),
  )
}
export function modelErrors(models: readonly GroupDraftModel[]): Map<number, 'id' | 'duplicate'> {
  const counts = new Map<string, number>()
  for (const model of models) {
    const name = JSON.stringify([model.id.trim(), model.alias.trim() || model.id.trim()])
    counts.set(name, (counts.get(name) ?? 0) + 1)
  }
  const errors = new Map<number, 'id' | 'duplicate'>()
  for (const model of models) {
    if (!model.id.trim()) errors.set(model.key, 'id')
    else if (
      (counts.get(JSON.stringify([model.id.trim(), model.alias.trim() || model.id.trim()])) ?? 0) >
      1
    )
      errors.set(model.key, 'duplicate')
  }
  return errors
}

export const maxCredentialEntries = 500

// 每行一条 API 密钥凭据；key 只用于列表渲染，不提交。
export interface CredentialEntryDraft {
  key: number
  fields: Record<string, string>
  concurrencyLimit: string
}
// 留空表示不限，与后端的 0 一致。
export function concurrencyLimitInvalid(value: string): boolean {
  return Boolean(value) && (!/^\d+$/u.test(value) || Number(value) > 10000)
}
export function credentialEntryFilled(entry: CredentialEntryDraft): boolean {
  return Object.values(entry.fields).some((value) => value.trim())
}
export function filledCredentialEntries(
  entries: readonly CredentialEntryDraft[],
): CredentialEntryDraft[] {
  return entries.filter(credentialEntryFilled)
}
export function credentialEntriesInvalid(entries: readonly CredentialEntryDraft[]): boolean {
  return filledCredentialEntries(entries).some((entry) =>
    concurrencyLimitInvalid(entry.concurrencyLimit),
  )
}
export function credentialEntriesPayload(
  entries: readonly CredentialEntryDraft[],
): CredentialEntryInput[] {
  return filledCredentialEntries(entries).map((entry) => ({
    fields: { ...entry.fields },
    concurrency_limit: Number(entry.concurrencyLimit || 0),
  }))
}
// 后端只在凭据校验失败时返回条目序号和字段名，其余错误交给调用方的通用文案。
export function credentialFieldError(
  error: ApiError,
): { entry: number; field: string } | undefined {
  const data = error.data
  if (
    error.code === 'VALIDATION_FAILED' &&
    data &&
    typeof data === 'object' &&
    'entry' in data &&
    typeof data.entry === 'number' &&
    Number.isSafeInteger(data.entry) &&
    'field' in data &&
    typeof data.field === 'string' &&
    /^[a-z][a-z0-9_]*$/u.test(data.field)
  )
    return { entry: data.entry, field: data.field }
}
export function validBaseURL(value: string): boolean {
  try {
    const url = new URL(value)
    return (
      ['http:', 'https:'].includes(url.protocol) &&
      Boolean(url.hostname) &&
      !url.username &&
      !url.password &&
      !url.search &&
      !url.hash &&
      !value.endsWith('?')
    )
  } catch {
    return false
  }
}
