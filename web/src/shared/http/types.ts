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

export interface AuthSessionPayload {
  user: AuthSessionUser
  teams: string[]
  platform: boolean
  admin_teams: string[]
}

export type { AppLocale }
