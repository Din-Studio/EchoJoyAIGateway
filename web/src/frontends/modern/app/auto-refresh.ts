import { onScopeDispose, toValue, watch, type MaybeRefOrGetter } from 'vue'

// 调度只属于当前浏览器。刷新动作的身份变化时，旧页面不得继续计时。
export function useAutoRefresh({
  interval,
  run,
  busy,
}: {
  interval: MaybeRefOrGetter<number>
  run: () => (() => Promise<boolean>) | undefined
  busy: () => boolean
}): void {
  let timer: ReturnType<typeof setTimeout> | undefined
  let generation = 0
  let failures = 0
  let running = false

  function stop(): void {
    clearTimeout(timer)
    timer = undefined
    generation++
  }

  async function tick(): Promise<void> {
    stop()
    const period = toValue(interval)
    const refresh = run()
    if (period <= 0 || document.hidden || !refresh) return
    // 可见性或档位变化也不得重入尚未完成的请求。
    if (!running && !busy()) {
      const current = generation
      running = true
      let ok = false
      try {
        ok = await refresh()
      } catch {
        // 与返回失败一样退避；错误展示仍由页面查询负责。
      } finally {
        running = false
      }
      if (current !== generation) return
      failures = ok ? 0 : failures + 1
    }
    // 忙碌时保留退避进度；请求结束后再计时，不累积并行轮询。
    timer = setTimeout(tick, Math.min(period * 2 ** failures, 5 * 60_000))
  }

  function syncVisibility(): void {
    if (document.hidden) stop()
    else void tick()
  }

  watch(
    [() => toValue(interval), run],
    () => {
      failures = 0
      void tick()
    },
    { immediate: true },
  )
  document.addEventListener('visibilitychange', syncVisibility)
  onScopeDispose(() => {
    stop()
    document.removeEventListener('visibilitychange', syncVisibility)
  })
}
