import { ApiError } from '@shared/http/errors'

// The API localizes these public dependency messages with Accept-Language.
// Keep unknown errors generic; do not display raw compiler or transport errors.
export function configDependencyMessage(cause: unknown, fallback: string): string {
  return cause instanceof ApiError && cause.code === 'CONFIG_DEPENDENCY_CONFLICT'
    ? cause.message || fallback
    : fallback
}
