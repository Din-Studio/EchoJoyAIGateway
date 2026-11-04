import type { ApiClient } from '@shared/http/client'
import { InvalidResponseError } from '@shared/http/errors'
import { boolean, integer, list, oneOf, record, text } from './response'

export interface ProxyItem {
  id: number
  name: string
  display_url: string
  scheme: 'http' | 'socks5'
  has_auth: boolean
  enabled: boolean
  group_count: number
  credential_count: number
  global: boolean
  last_test_url: string
  last_test_at_ms: number | null
  last_test_duration_ms: number | null
  last_test_status_code: number | null
  last_test_error: string
}

export interface ProxyFilters {
  q: string
  state: string
  scheme: string
  used: string
  test: string
  sort: string
  page: number
  page_size: number
}

export interface ProxyList {
  items: ProxyItem[]
  total: number
  page: number
  page_size: number
  test_url: string
}

export interface ProxyReference {
  group_id: number
  group_name: string
  credential_id?: number
  label?: string
  connection_type?: string
}

export interface ProxyImpact {
  groups: ProxyReference[]
  credentials: ProxyReference[]
  global: boolean
}

export const proxyListKey = ['modern', 'proxies'] as const

function readProxy(value: unknown): ProxyItem {
  const row = record(value)
  return {
    id: integer(row.id, 1),
    name: text(row.name),
    display_url: text(row.display_url),
    scheme: oneOf(row.scheme, ['http', 'socks5']),
    has_auth: boolean(row.has_auth),
    enabled: boolean(row.enabled),
    group_count: integer(row.group_count),
    credential_count: integer(row.credential_count),
    global: boolean(row.global),
    last_test_url: text(row.last_test_url),
    last_test_at_ms: row.last_test_at_ms === null ? null : integer(row.last_test_at_ms),
    last_test_duration_ms:
      row.last_test_duration_ms === null ? null : integer(row.last_test_duration_ms),
    last_test_status_code:
      row.last_test_status_code === null ? null : integer(row.last_test_status_code),
    last_test_error: text(row.last_test_error),
  }
}

export async function listProxies(
  client: ApiClient,
  filters: Partial<ProxyFilters> = {},
  signal?: AbortSignal,
  all = false,
): Promise<ProxyList> {
  const query = new URLSearchParams()
  for (const [key, value] of Object.entries(filters)) {
    if (value !== '') query.set(key, String(value))
  }
  if (all) query.set('all', 'true')
  const data = record(await client.request(`/api/proxies?${query}`, { signal }))
  return {
    items: list(data.items).map(readProxy),
    total: integer(data.total),
    page: integer(data.page, 1),
    page_size: integer(data.page_size, 1),
    test_url: text(data.test_url),
  }
}

export function proxyOptionLabel(proxy: Pick<ProxyItem, 'name' | 'display_url'>) {
  return proxy.name ? `${proxy.name} · ${proxy.display_url}` : proxy.display_url
}

export async function revealProxy(client: ApiClient, id: number, signal?: AbortSignal) {
  const data = record(
    await client.request(`/api/proxies/${id}/reveal`, {
      method: 'POST',
      cache: 'no-store',
      signal,
    }),
  )
  if (integer(data.id, 1) !== id) throw new InvalidResponseError()
  return { id, name: text(data.name), url: text(data.url) }
}

export async function saveProxy(
  client: ApiClient,
  value: { name?: string; url: string },
  key: string,
  id?: number,
) {
  const proxy = readProxy(
    await client.request(id ? `/api/proxies/${id}` : '/api/proxies', {
      method: id ? 'PUT' : 'POST',
      json: value,
      headers: { 'Idempotency-Key': key },
    }),
  )
  if (id !== undefined && proxy.id !== id) throw new InvalidResponseError()
  return proxy
}

export async function importProxies(client: ApiClient, entries: string, key: string) {
  const data = record(
    await client.request('/api/proxies/import', {
      method: 'POST',
      json: { entries },
      headers: { 'Idempotency-Key': key },
    }),
  )
  return {
    imported: integer(data.imported),
    duplicates: integer(data.duplicates),
    invalid_lines: list(data.invalid_lines).map((line) => integer(line, 1)),
  }
}

function readReference(value: unknown): ProxyReference {
  const row = record(value)
  return {
    group_id: integer(row.group_id, 1),
    group_name: text(row.group_name),
    credential_id: row.credential_id === undefined ? undefined : integer(row.credential_id, 1),
    label: row.label === undefined ? undefined : text(row.label),
    connection_type: row.connection_type === undefined ? undefined : text(row.connection_type),
  }
}

export async function proxyImpact(client: ApiClient, ids: number[]): Promise<ProxyImpact> {
  const data = record(
    await client.request('/api/proxies/impact', { method: 'POST', json: { ids } }),
  )
  return {
    groups: list(data.groups).map(readReference),
    credentials: list(data.credentials).map(readReference),
    global: boolean(data.global),
  }
}

export function batchProxies(
  client: ApiClient,
  ids: number[],
  action: 'enable' | 'disable' | 'delete',
) {
  return client.request('/api/proxies/batch', { method: 'POST', json: { ids, action } })
}

export function saveProxyTestURL(client: ApiClient, url: string, signal?: AbortSignal) {
  return client.request('/api/proxies/test-url', { method: 'PUT', json: { url }, signal })
}

export async function testProxy(client: ApiClient, id: number, url: string, signal?: AbortSignal) {
  const proxy = readProxy(
    await client.request(`/api/proxies/${id}/test`, {
      method: 'POST',
      json: { url },
      signal,
    }),
  )
  if (proxy.id !== id) throw new InvalidResponseError()
  return proxy
}

export function proxyTestSucceeded(proxy: ProxyItem) {
  return (
    proxy.last_test_at_ms !== null &&
    !proxy.last_test_error &&
    proxy.last_test_status_code !== null &&
    proxy.last_test_status_code >= 200 &&
    proxy.last_test_status_code < 300
  )
}

export function validProxySelection(value: string) {
  return /^[1-9]\d*$/.test(value) && Number.isSafeInteger(Number(value))
}

export function validTestURL(value: string) {
  try {
    const url = new URL(value)
    return (
      ['http:', 'https:'].includes(url.protocol) &&
      !url.username &&
      !url.password &&
      !url.hash &&
      value.length <= 2048
    )
  } catch {
    return false
  }
}

/** 固定本次目标集合与 URL；取消只停止本次队列，不触发代理状态变更。 */
export async function runProxyTests(
  client: ApiClient,
  ids: number[],
  url: string,
  signal: AbortSignal,
  completed: (id: number, result?: ProxyItem, error?: unknown) => void,
) {
  let cursor = 0
  const targets = [...new Set(ids)]
  await Promise.all(
    Array.from({ length: Math.min(4, targets.length) }, async () => {
      while (!signal.aborted && cursor < targets.length) {
        const id = targets[cursor++]!
        try {
          const result = await testProxy(client, id, url, signal)
          if (!signal.aborted) completed(id, result)
        } catch (error) {
          if (!signal.aborted) completed(id, undefined, error)
        }
      }
    }),
  )
}
