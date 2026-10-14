import type { AppLocale } from '@shared/preferences/locale'

export interface SuccessEnvelope<T> {
  code: 0
  message: string
  data?: T
}

export interface ErrorEnvelope<T = unknown> {
  code: string
  message: string
  data?: T
}

export interface AuthSessionUser {
  id: string
  name: string
  display_name: string
  email: string
}

export interface AuthSessionTeam {
  name: string
  display_name: string
}

export interface AuthSessionPayload {
  user: AuthSessionUser
  teams: string[]
  platform: boolean
  admin_teams: string[]
  // 控制台可切换到的团队：平台管理员为 Casdoor 全部团队，其他人为自己所属的团队。
  selectable_teams: AuthSessionTeam[]
}

export type { AppLocale }
