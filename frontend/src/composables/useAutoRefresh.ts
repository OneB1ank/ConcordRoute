import { onBeforeUnmount, ref, type Ref } from 'vue'

export interface UseAutoRefreshOptions {
  storageKey: string
  intervals?: readonly number[]
  defaultInterval?: number
  onRefresh: () => Promise<void> | void
  // 弹窗打开或页面隐藏时可暂停本轮刷新。
  shouldPause?: () => boolean
}

export function useAutoRefresh(options: UseAutoRefreshOptions) {
  const intervals = options.intervals ?? ([5, 10, 15, 30] as const)
  const enabled = ref(false)
  const intervalSeconds = ref(options.defaultInterval ?? intervals[intervals.length - 1])
  const countdown = ref(0)
  const fetching = ref(false)
  let timerID: number | undefined

  function loadFromStorage() {
    try {
      const saved = localStorage.getItem(options.storageKey)
      if (!saved) return
      const parsed = JSON.parse(saved) as { enabled?: boolean; interval_seconds?: number }
      enabled.value = parsed.enabled === true
      const value = Number(parsed.interval_seconds)
      if (intervals.includes(value)) intervalSeconds.value = value
    } catch {
      // 本地缓存损坏时沿用默认值。
    }
  }

  function saveToStorage() {
    try {
      localStorage.setItem(options.storageKey, JSON.stringify({
        enabled: enabled.value,
        interval_seconds: intervalSeconds.value,
      }))
    } catch {
      // 浏览器禁用本地存储时仍允许当前页面刷新。
    }
  }

  async function tick() {
    if (!enabled.value || options.shouldPause?.() || fetching.value) return
    if (countdown.value > 0) {
      countdown.value -= 1
      return
    }
    countdown.value = intervalSeconds.value
    fetching.value = true
    try {
      await options.onRefresh()
    } finally {
      fetching.value = false
    }
  }

  function start() {
    if (timerID === undefined) timerID = window.setInterval(tick, 1000)
  }

  function stop() {
    if (timerID === undefined) return
    window.clearInterval(timerID)
    timerID = undefined
  }

  function setEnabled(value: boolean) {
    enabled.value = value
    saveToStorage()
    if (value) {
      countdown.value = intervalSeconds.value
      start()
    } else {
      stop()
      countdown.value = 0
    }
  }

  function setInterval(seconds: number) {
    intervalSeconds.value = seconds
    saveToStorage()
    if (enabled.value) countdown.value = seconds
  }

  function resetCountdown() {
    countdown.value = intervalSeconds.value
  }

  loadFromStorage()
  onBeforeUnmount(stop)

  return {
    enabled: enabled as Ref<boolean>,
    intervalSeconds: intervalSeconds as Ref<number>,
    countdown: countdown as Ref<number>,
    fetching: fetching as Ref<boolean>,
    intervals,
    setEnabled,
    setInterval,
    resetCountdown,
    start,
    stop,
  }
}
