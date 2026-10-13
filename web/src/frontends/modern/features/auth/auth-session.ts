import type { QueryClient } from '@tanstack/vue-query'
import { inject, reactive, readonly, type InjectionKey } from 'vue'

import { getAuthSession, logout as requestLogout } from '@modern/api/auth'
import type { ApiClient } from '@shared/http/client'
import { RequestCancelledError } from '@shared/http/errors'
import type { AuthSessionUser } from '@shared/http/types'
import { authFailure } from './auth-errors'

// 会话由 httpOnly cookie 承载，前端只能通过 /api/auth/session 得知是否已登录。
type AuthPhase =
  'anonymous' | 'unvalidated' | 'validating' | 'validated' | 'network-error' | 'invalid-response'

export function createAuthSession(deps: { client: ApiClient; queryClient: QueryClient }) {
  let revision = 0
  let validation: { controller: AbortController; promise: Promise<void> } | undefined
  const state = reactive({
    phase: 'unvalidated' as AuthPhase,
    user: null as AuthSessionUser | null,
    teams: [] as string[],
    platform: false,
    adminTeams: [] as string[],
  })

  function cancelValidation(): void {
    validation?.controller.abort()
    validation = undefined
  }

  function resetIdentity(): void {
    state.user = null
    state.teams = []
    state.platform = false
    state.adminTeams = []
  }

  function clear(): void {
    revision += 1
    cancelValidation()
    state.phase = 'anonymous'
    resetIdentity()
    // 取消查询，并清空当前身份的查询和 mutation 缓存。
    deps.queryClient.clear()
  }

  function ensureValidated(): Promise<void> {
    if (state.phase === 'validated' || state.phase === 'anonymous') return Promise.resolve()
    if (validation) return validation.promise
    const currentRevision = revision
    const controller = new AbortController()
    state.phase = 'validating'

    const promise = getAuthSession(deps.client, controller.signal)
      .then((session) => {
        if (currentRevision !== revision || controller.signal.aborted) return
        state.user = session.user
        state.teams = session.teams
        state.platform = session.platform
        state.adminTeams = session.admin_teams
        state.phase = 'validated'
      })
      .catch((error: unknown) => {
        if (currentRevision !== revision || controller.signal.aborted) return
        if (error instanceof RequestCancelledError) {
          state.phase = 'unvalidated'
          return
        }
        const failure = authFailure(error)
        if (failure === 'invalid') clear()
        else state.phase = failure === 'network' ? 'network-error' : 'invalid-response'
      })
      .finally(() => {
        if (validation?.controller === controller) validation = undefined
      })
    validation = { controller, promise }
    return promise
  }

  /** 结束网关会话，返回 Casdoor 登出地址，由调用方跳转以结束单点登录会话。 */
  async function logout(): Promise<string> {
    const redirectURL = await requestLogout()
    clear()
    return redirectURL
  }

  return {
    state: readonly(state),
    getRevision: () => revision,
    isAnonymous: () => state.phase === 'anonymous',
    isValidated: () => state.phase === 'validated',
    isPlatformAdmin: () => state.platform,
    ensureValidated,
    logout,
    clear,
  }
}

export type AuthSession = ReturnType<typeof createAuthSession>
export const authSessionKey: InjectionKey<AuthSession> = Symbol('modern-auth-session')

export function useAuthSession(): AuthSession {
  const session = inject(authSessionKey)
  if (!session) throw new Error('MODERN_AUTH_SESSION_NOT_PROVIDED')
  return session
}
