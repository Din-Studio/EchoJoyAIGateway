import { useQuery } from '@tanstack/vue-query'
import { computed, type ComputedRef } from 'vue'
import { currencyKey, getCurrency, type SystemCurrency } from '@modern/api/currency'
import { useApiClient } from '@shared/http/client-context'

// 金额展示使用各接口响应自带的币种；这里只提供输入框单位与默认值换算所需的系统货币。
export function useSystemCurrency(): ComputedRef<SystemCurrency | undefined> {
  const client = useApiClient()
  const query = useQuery({
    queryKey: currencyKey,
    queryFn: ({ signal }) => getCurrency(client, signal),
  })
  return computed(() => query.data.value)
}
