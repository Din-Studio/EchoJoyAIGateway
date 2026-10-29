import { InvalidResponseError } from './http/errors'

export interface ConcurrencyView {
  current: number
  limit: number
}
export function readConcurrency(value: unknown): ConcurrencyView {
  if (!value || typeof value !== 'object') throw new InvalidResponseError()
  const row = value as Record<string, unknown>
  for (const key of ['current', 'limit'])
    if (typeof row[key] !== 'number' || !Number.isSafeInteger(row[key]) || row[key] < 0)
      throw new InvalidResponseError()
  return { current: row.current as number, limit: row.limit as number }
}
// Unlimited scopes are not counted, so only a limited scope shows its current value.
export function formatConcurrency(
  value: ConcurrencyView,
  t: (key: string, values?: Record<string, unknown>) => string,
): string {
  if (value.limit === 0) return t('concurrency.valueUnlimited')
  return t('concurrency.value', { current: value.current, limit: value.limit })
}
