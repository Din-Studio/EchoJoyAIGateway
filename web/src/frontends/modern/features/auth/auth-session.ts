import type { QueryClient } from '@tanstack/vue-query'
import { inject, reactive, readonly, type InjectionKey } from 'vue'

import { getAuthSession, logout as requestLogout } from '@modern/api/auth'
import type { ApiClient } from '@shared/http/client'
import { RequestCancelledError } from '@shared/http/errors'
import type { AuthSessionTeam, AuthSessionUser } from '@shared/http/types'
import { authFailure } from './auth-errors'

// 会话由 httpOnly cookie 承载，前端只能通过 /api/auth/session 得知是否已登录。
type AuthPhase =
  'anonymous' | 'unvalidated' | 'validating' | 'validated' | 'network-error' | 'invalid-response'

// 控制台选中的团队只存在浏览器里，每个请求以 X-Team-Scope 头带给后端。
const teamStorageKey = 'gpt-load.modern.team'

function readStoredTeam(): string {
  try {
    return window.localStorage.getItem(teamStorageKey) ?? ''
  } catch {
    return ''
  }
}

function storeTeam(team: string): void {
  try {
    if (team) window.localStorage.setItem(teamStorageKey, team)
    else window.localStorage.removeItem(teamStorageKey)
  } catch {
    // 存储被禁用时，本次访问仍可使用所选团队。
  }
}

export function createAuthSession(deps: { client: ApiClient; queryClient: QueryClient }) {
  let revision = 0
  let validation: { controller: AbortController; promise: Promise<void> } | undefined
  const state = reactive({
    phase: 'unvalidated' as AuthPhase,
    user: null as AuthSessionUser | null,
    teams: [] as string[],
    platform: false,
    adminTeams: [] as string[],
    // 控制台可切换到的团队，由 /api/auth/session 按身份给出。
    selectableTeams: [] as AuthSessionTeam[],
    // 控制台当前的团队，校验成功后总是 selectableTeams 中的一个；
    // 只有可选团队为空（平台管理员且 Casdoor 没有团队）时为空，后端视为不收窄。
    team: readStoredTeam(),
  })

  /**
   * 切换控制台的团队范围。resetQueries 会把所有查询重置为初始状态并重新拉取
   * 当前页面正在用的那些；clear() 只清缓存，已挂载的查询不会重新请求。
   */
  function selectTeam(team: string): void {
    if (team === state.team) return
    state.team = team
    storeTeam(team)
    void deps.queryClient.resetQueries()
  }

  function cancelValidation(): void {
    validation?.controller.abort()
    validation = undefined
  }

  function resetIdentity(): void {
    state.user = null
    state.teams = []
    state.platform = false
    state.adminTeams = []
    state.selectableTeams = []
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
        state.selectableTeams = session.selectable_teams
        // 上次选的团队不在本账号可选范围内（换了账号或被移出团队）时，默认第一个。
        if (!session.selectable_teams.some((team) => team.name === state.team)) {
          state.team = session.selectable_teams[0]?.name ?? ''
          storeTeam(state.team)
        }
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
    selectTeam,
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
