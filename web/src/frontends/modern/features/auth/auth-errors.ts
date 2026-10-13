import { ApiError, NetworkError } from '@shared/http/errors'

export type AuthFailure = 'invalid' | 'network' | 'invalid-response'

export function authFailure(error: unknown): AuthFailure {
  if (error instanceof ApiError && error.code === 'UNAUTHORIZED') return 'invalid'
  if (error instanceof NetworkError) return 'network'
  return 'invalid-response'
}
