import type { CredentialEntryInput } from '@modern/api/group-create'
import { maxCredentialEntries } from './group-create-rules'

const maxImportBytes = 32 * 1024 * 1024

export class APIKeyFileImportError extends Error {
  constructor(readonly code: 'empty' | 'too_large' | 'too_many' | 'read_failed') {
    super(code)
  }
}

function credentialFields(content: string): Record<string, string> {
  let parsed: unknown
  try {
    parsed = JSON.parse(content)
  } catch {
    // 普通 Key 可以含 JSON 前缀；保持与现有 TXT 导出和逐行输入一致。
  }
  if (parsed !== null && typeof parsed === 'object') {
    if (Array.isArray(parsed) || Object.values(parsed).some((value) => typeof value !== 'string'))
      throw new APIKeyFileImportError('read_failed')
    return parsed as Record<string, string>
  }
  return { api_key: content }
}

export async function readAPIKeyCredentialFiles(
  files: readonly File[],
): Promise<CredentialEntryInput[]> {
  if (files.reduce((size, file) => size + file.size, 0) > maxImportBytes)
    throw new APIKeyFileImportError('too_large')

  const credentials: CredentialEntryInput[] = []
  for (const file of files) {
    let content: string
    try {
      content = new TextDecoder('utf-8', { fatal: true }).decode(await file.arrayBuffer()).trim()
    } catch {
      throw new APIKeyFileImportError('read_failed')
    }
    if (!content) continue
    let parsed: unknown
    try {
      parsed = JSON.parse(content)
    } catch {
      // TXT 和 JSONL 按非空行读取；格式化的 JSON 对象作为一条凭据。
    }
    const lines =
      parsed !== null && typeof parsed === 'object'
        ? [content]
        : content
            .split(/\r?\n/u)
            .map((line) => line.trim())
            .filter(Boolean)
    if (credentials.length + lines.length > maxCredentialEntries)
      throw new APIKeyFileImportError('too_many')
    for (const line of lines)
      credentials.push({ fields: credentialFields(line), concurrency_limit: 0 })
  }

  if (!credentials.length) throw new APIKeyFileImportError('empty')
  if (new TextEncoder().encode(JSON.stringify({ credentials })).length > maxImportBytes)
    throw new APIKeyFileImportError('too_large')
  return credentials
}
