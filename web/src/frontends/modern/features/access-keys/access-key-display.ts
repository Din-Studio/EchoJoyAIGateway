import { dateFormatter } from '@modern/components/ui/intl-formatters'
import type { AccessKey } from '@modern/api/access-keys'
import { formatMoneyNano } from '@modern/components/ui/format'
import type { SemanticTone } from '@modern/components/ui'
export function accessState(row: AccessKey & { expired?: boolean }): {
  key: string
  tone: SemanticTone
} {
  if (row.status === 'disabled') return { key: 'disabled', tone: 'neutral' }
  if (row.expired ?? (row.expires_at_ms !== null && row.expires_at_ms <= Date.now()))
    return { key: 'expired', tone: 'warning' }
  if (row.cost_limit_status?.allowed === false) return { key: 'exhausted', tone: 'danger' }
  return { key: 'active', tone: 'success' }
}
export function accessTime(value: number | null | undefined, locale: string, time = true): string {
  if (!value) return '—'
  return dateFormatter(locale, {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    ...(time ? { hour: '2-digit', minute: '2-digit' } : {}),
  }).format(value)
}
// 十进制金额字符串转换为 1e-9 单位整数。
export function accessNano(value: string): bigint {
  const [whole = '0', fraction = ''] = value.split('.')
  return BigInt(whole) * 1000000000n + BigInt(fraction.padEnd(9, '0'))
}
export function accessMoney(value: string, currency: string, locale: string): string {
  return formatMoneyNano(String(accessNano(value)), currency, locale, 'narrowSymbol')
}
