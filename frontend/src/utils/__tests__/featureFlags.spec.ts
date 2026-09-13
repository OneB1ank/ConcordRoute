import { beforeEach, describe, expect, it, vi } from 'vitest'

const { appStore } = vi.hoisted(() => ({
  appStore: {
    cachedPublicSettings: undefined as
      | { channel_monitor_hide_throughput?: boolean; channel_monitor_mode?: 'v1' | 'v2' | string }
      | undefined,
  },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => appStore,
}))

import { getChannelMonitorMode, isChannelMonitorThroughputHidden } from '@/utils/featureFlags'

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

describe('channel monitor mode default', () => {
  beforeEach(() => {
    appStore.cachedPublicSettings = undefined
  })

  it('设置尚未加载或字段缺失时默认使用 V2', () => {
    expect(getChannelMonitorMode()).toBe('v2')

    appStore.cachedPublicSettings = {}
    expect(getChannelMonitorMode()).toBe('v2')
  })

  it('仅后端显式返回 v1 时保留主动探测模式', () => {
    appStore.cachedPublicSettings = { channel_monitor_mode: 'v1' }
    expect(getChannelMonitorMode()).toBe('v1')

    appStore.cachedPublicSettings = { channel_monitor_mode: 'invalid' }
    expect(getChannelMonitorMode()).toBe('v2')
  })
})
