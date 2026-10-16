import type { ApiClient } from '@shared/http/client'
import { record, text } from './response'

export const currencyKey = ['modern', 'currency'] as const

// 系统货币：新请求按它结算，金额输入框以它为单位。
export interface SystemCurrency {
  currency: string
  exchangeRate: string
}

export async function getCurrency(client: ApiClient, signal: AbortSignal): Promise<SystemCurrency> {
  const row = record(await client.request('/api/currency', { signal }))
  return { currency: text(row.currency), exchangeRate: text(row.exchange_rate) }
}
