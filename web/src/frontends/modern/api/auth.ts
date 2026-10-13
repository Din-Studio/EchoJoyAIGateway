import type { ApiClient } from '@shared/http/client'
import { ApiError, InvalidResponseError, NetworkError } from '@shared/http/errors'
import type { AuthSessionPayload } from '@shared/http/types'
import { boolean, list, record, text } from './response'

export async function getAuthSession(
  client: ApiClient,
  signal: AbortSignal,
): Promise<AuthSessionPayload> {
  const data = record(
    await client.request<unknown>('/api/auth/session', { cache: 'no-store', signal }),
  )
  const user = record(data.user)
  return {
    user: {
      id: text(user.id),
      name: text(user.name),
      display_name: text(user.display_name),
      email: text(user.email),
    },
    teams: list(data.teams).map(text),
    platform: boolean(data.platform),
    admin_teams: list(data.admin_teams).map(text),
  }
}

/** 浏览器登录入口；Casdoor 完成认证后回到 redirect。 */
export function loginURL(redirect: string): string {
  return '/auth/login?redirect=' + encodeURIComponent(redirect)
}

/** 删除网关会话，返回结束 Casdoor 会话的登出地址。 */
export async function logout(): Promise<string> {
  let response: Response
  try {
    response = await fetch('/auth/logout', { method: 'POST', credentials: 'same-origin' })
  } catch {
    throw new NetworkError()
  }
  let body: unknown
  try {
    body = await response.json()
  } catch {
    throw new InvalidResponseError()
  }
  const envelope = record(body)
  if (!response.ok || envelope.code !== 0) {
    throw new ApiError(response.status, text(envelope.code), text(envelope.message), envelope.data)
  }
  return text(record(envelope.data).redirect_url)
}
