import { queryOptions } from '@tanstack/vue-query'

import type { ApiClient } from '@shared/http/client'
import { InvalidResponseError } from '@shared/http/errors'
import { controlQueryKeys } from '@/app/query-keys'

import { assertNoSecretLikeFields, projectEnum, projectRecord, projectString } from './projector'

export type SecretSource = 'environment'
export type DatabaseDriver = 'postgres'

export interface SecretSourceInfo {
  source: SecretSource
}

export interface SystemInfoDto {
  version: string
  deployment: {
    database: DatabaseDriver
  }
  auth_key: SecretSourceInfo
  encryption: SecretSourceInfo
}

function invalidResponse(): never {
  throw new InvalidResponseError()
}

function assertExactFields(record: Record<string, unknown>, fields: readonly string[]): void {
  assertNoSecretLikeFields(record, fields)
  if (
    Object.keys(record).length !== fields.length ||
    fields.some((field) => !Object.prototype.hasOwnProperty.call(record, field))
  ) {
    invalidResponse()
  }
}

function projectNonBlankTrimmedString(value: unknown): string {
  const result = projectString(value)
  if (result.trim().length === 0 || result !== result.trim()) invalidResponse()
  return result
}

function projectSecretSource(value: unknown): SecretSourceInfo {
  const record = projectRecord(value)
  assertExactFields(record, ['source'])
  return { source: projectEnum(record.source, ['environment'] as const) }
}

export function projectSystemInfo(value: unknown): SystemInfoDto {
  const record = projectRecord(value)
  assertExactFields(record, ['version', 'deployment', 'auth_key', 'encryption'])
  const deployment = projectRecord(record.deployment)
  assertExactFields(deployment, ['database'])
  return {
    version: projectNonBlankTrimmedString(record.version),
    deployment: {
      database: projectEnum(deployment.database, ['postgres'] as const),
    },
    auth_key: projectSecretSource(record.auth_key),
    encryption: projectSecretSource(record.encryption),
  }
}

export async function getSystemInfo(
  client: ApiClient,
  signal?: AbortSignal,
): Promise<SystemInfoDto> {
  return projectSystemInfo(await client.request('/api/system/info', { signal }))
}

export function systemInfoQueryOptions(client: ApiClient) {
  return queryOptions({
    queryKey: controlQueryKeys.systemInfo(),
    queryFn: ({ signal }) => getSystemInfo(client, signal),
  })
}
