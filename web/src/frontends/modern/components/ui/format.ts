import { numberFormatter } from '@modern/components/ui/intl-formatters'
export function formatCompactNumber(value: number, locale: string): string {
  void locale
  return numberFormatter('en-US', { notation: 'compact', maximumFractionDigits: 1 }).format(value)
}

export function formatRemainingDuration(milliseconds: number, locale: string): string {
  void locale
  const minutes = Math.max(0, Math.ceil(milliseconds / 60000))
  const hours = Math.floor(minutes / 60)
  const duration =
    hours >= 24
      ? ([
          [Math.floor(hours / 24), 'day'],
          [hours % 24, 'hour'],
        ] as const)
      : hours > 0
        ? ([
            [hours, 'hour'],
            [minutes % 60, 'minute'],
          ] as const)
        : ([[minutes, 'minute']] as const)
  return duration
    .filter(([value]) => value > 0 || minutes === 0)
    .map(([value, unit]) => `${value}${{ day: 'd', hour: 'h', minute: 'm' }[unit]}`)
    .join(' ')
}

// value 是以 currency（ISO 4217 代码）计的 1e-9 单位整数字符串。
export function formatMoneyNano(
  value: string,
  currency: string,
  locale: string,
  currencyDisplay: 'symbol' | 'narrowSymbol' = 'symbol',
  fractionDigits: 2 | 3 = 3,
): string {
  const amount = BigInt(value)
  const scale = 10n ** BigInt(9 - fractionDigits)
  const unit = 10 ** fractionDigits
  const formatter = numberFormatter(locale, {
    style: 'currency',
    currency,
    currencyDisplay,
    minimumFractionDigits: 2,
    maximumFractionDigits: fractionDigits,
  })
  if (amount > 0n && amount < scale) return `<${formatter.format(1 / unit)}`
  // 先按展示精度完成整数舍入，再转换到展示用 Number，避免原始纳单位直接丢失精度。
  return formatter.format(Number((amount + scale / 2n) / scale) / unit)
}

// 货币的窄符号，例如 USD → $、CNY → ¥，用于输入框单位和精确金额前缀。
export function currencySymbol(currency: string, locale: string): string {
  return (
    numberFormatter(locale, { style: 'currency', currency, currencyDisplay: 'narrowSymbol' })
      .formatToParts(0)
      .find((part) => part.type === 'currency')?.value ?? currency
  )
}

// 精确展示 1e-9 单位整数，不经过浮点。
export function exactMoneyNano(value: string, currency: string, locale: string): string {
  const padded = value.padStart(10, '0')
  const fraction = padded.slice(-9).replace(/0+$/, '')
  return currencySymbol(currency, locale) + padded.slice(0, -9) + (fraction ? '.' + fraction : '')
}

// 换算后的金额最多 9 位小数，往返换算会留下 0.999999997 这类尾数；编辑框回显时舍入到 places 位。
export function roundDecimalString(value: string, places: number): string {
  const match = /^(\d+)(?:\.(\d+))?$/.exec(value)
  if (!match) return value
  const fraction = (match[2] ?? '').padEnd(places + 1, '0')
  const scaled =
    BigInt(match[1]! + fraction.slice(0, places)) + (Number(fraction[places]) >= 5 ? 1n : 0n)
  const digits = scaled.toString().padStart(places + 1, '0')
  const kept = digits.slice(-places).replace(/0+$/, '')
  return digits.slice(0, -places) + (kept ? '.' + kept : '')
}
