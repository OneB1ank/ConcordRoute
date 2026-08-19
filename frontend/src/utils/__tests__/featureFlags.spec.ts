import { beforeEach, describe, expect, it, vi } from 'vitest'

const { appStore } = vi.hoisted(() => ({
  appStore: {
    cachedPublicSettings: undefined as
      | { channel_monitor_hide_throughput?: boolean }
      | undefined,
  },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => appStore,
}))

import { isChannelMonitorThroughputHidden } from '@/utils/featureFlags'

describe('channel monitor throughput privacy flag', () => {
  beforeEach(() => {
    appStore.cachedPublicSettings = undefined
  })

  it('设置尚未加载时默认隐藏吞吐', () => {
    expect(isChannelMonitorThroughputHidden()).toBe(true)
  })

  it('字段缺失时默认隐藏吞吐', () => {
    appStore.cachedPublicSettings = {}
    expect(isChannelMonitorThroughputHidden()).toBe(true)
  })

  it('仅在后端显式关闭隐藏时展示吞吐', () => {
    appStore.cachedPublicSettings = { channel_monitor_hide_throughput: false }
    expect(isChannelMonitorThroughputHidden()).toBe(false)

    appStore.cachedPublicSettings = { channel_monitor_hide_throughput: true }
    expect(isChannelMonitorThroughputHidden()).toBe(true)
  })
})
