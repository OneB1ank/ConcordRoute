import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import UsageProgressBar from '../UsageProgressBar.vue'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

describe('UsageProgressBar', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-03-17T00:00:00Z'))
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('累计行不重复显示五小时标签，刷新不会再次累加', async () => {
    const wrapper = mount(UsageProgressBar, {
      props: {
        label: '5h',
        utilization: 0,
        color: 'indigo',
        showNowWhenIdle: true,
        statsHint: '按账号累计最近五小时，与上游额度独立',
        windowStats: { requests: 7, tokens: 481500, cost: 1.42, user_cost: 1.42 }
      }
    })
    const row = wrapper.get('[data-testid="window-stats"]')
    expect(row.text()).not.toContain('5h')
    expect(wrapper.text()).toContain('5h')
    expect(row.text()).toContain('7 req')
    expect(row.attributes('title')).toContain('与上游额度独立')
    expect(wrapper.text()).toContain('0%')
    await wrapper.setProps({
      windowStats: { requests: 8, tokens: 491500, cost: 1.5, user_cost: 1.5 }
    })
    expect(row.text()).toContain('8 req')
    expect(row.text()).not.toContain('15 req')
    expect(wrapper.text()).toContain('0%')
  })

  it('showNowWhenIdle=true 且利用率为 0 时显示“现在”', () => {
    const wrapper = mount(UsageProgressBar, {
      props: {
        label: '5h',
        utilization: 0,
        resetsAt: '2026-03-17T02:30:00Z',
        showNowWhenIdle: true,
        color: 'indigo'
      }
    })

    expect(wrapper.text()).toContain('usage.resetNow')
    expect(wrapper.text()).not.toContain('2h 30m')
  })

  it('showNowWhenIdle=true 但利用率大于 0 时显示倒计时', () => {
    const wrapper = mount(UsageProgressBar, {
      props: {
        label: '7d',
        utilization: 12,
        resetsAt: '2026-03-17T02:30:00Z',
        showNowWhenIdle: true,
        color: 'emerald'
      }
    })

    expect(wrapper.text()).toContain('2h 30m')
    expect(wrapper.text()).not.toContain('usage.resetNow')
    expect(wrapper.text()).not.toContain('usage.resetPending')
  })

  it('showNowWhenIdle=false 时保持原有倒计时行为', () => {
    const wrapper = mount(UsageProgressBar, {
      props: {
        label: '1d',
        utilization: 0,
        resetsAt: '2026-03-17T02:30:00Z',
        showNowWhenIdle: false,
        color: 'indigo'
      }
    })

    expect(wrapper.text()).toContain('2h 30m')
    expect(wrapper.text()).not.toContain('usage.resetNow')
  })

  it('resetsAt 已过期且利用率大于 0 时显示「待刷新」', () => {
    const wrapper = mount(UsageProgressBar, {
      props: {
        label: '5h',
        utilization: 53,
        // 早于 fake system time 2026-03-17T00:00:00Z
        resetsAt: '2026-03-16T22:00:00Z',
        color: 'indigo'
      }
    })

    expect(wrapper.text()).toContain('usage.resetPending')
    expect(wrapper.text()).not.toContain('usage.resetNow')
  })

  it('resetsAt 已过期且利用率为 0 时仍显示「现在」', () => {
    const wrapper = mount(UsageProgressBar, {
      props: {
        label: '5h',
        utilization: 0,
        resetsAt: '2026-03-16T22:00:00Z',
        color: 'indigo'
      }
    })

    expect(wrapper.text()).toContain('usage.resetNow')
    expect(wrapper.text()).not.toContain('usage.resetPending')
  })

  it('剩余容量模式在 100% 时显示满格绿色', () => {
    const wrapper = mount(UsageProgressBar, {
      props: {
        label: 'Req',
        utilization: 100,
        remainingCapacity: true,
        color: 'indigo'
      }
    })

    expect(wrapper.text()).toContain('100%')
    expect(wrapper.get('.h-1\\.5 > div').attributes('style')).toContain('width: 100%')
    expect(wrapper.get('.h-1\\.5 > div').classes()).toContain('bg-green-500')
  })

  it('剩余容量模式在低量和耗尽时缩短并变红', async () => {
    const wrapper = mount(UsageProgressBar, {
      props: {
        label: 'Req',
        utilization: 15,
        remainingCapacity: true,
        color: 'indigo'
      }
    })

    expect(wrapper.text()).toContain('15%')
    expect(wrapper.get('.h-1\\.5 > div').attributes('style')).toContain('width: 15%')
    expect(wrapper.get('.h-1\\.5 > div').classes()).toContain('bg-red-500')

    await wrapper.setProps({ utilization: 0 })

    expect(wrapper.text()).toContain('0%')
    expect(wrapper.get('.h-1\\.5 > div').attributes('style')).toContain('width: 0%')
    expect(wrapper.get('.h-1\\.5 > div').classes()).toContain('bg-red-500')
  })

  it('默认利用率模式仍把超限显示为满格红色', () => {
    const wrapper = mount(UsageProgressBar, {
      props: {
        label: '5h',
        utilization: 120,
        color: 'indigo'
      }
    })

    expect(wrapper.text()).toContain('120%')
    expect(wrapper.get('.h-1\\.5 > div').attributes('style')).toContain('width: 100%')
    expect(wrapper.get('.h-1\\.5 > div').classes()).toContain('bg-red-500')
  })

  it('显示透支期间的独立请求、Token 与费用统计', () => {
    const wrapper = mount(UsageProgressBar, {
      props: {
        label: '7d',
        utilization: 100,
        color: 'emerald',
        overdraftStats: {
          requests: 7,
          tokens: 1234,
          cost: 2.5,
          user_cost: 3
        }
      }
    })

    expect(wrapper.get('[data-testid="overdraft-stats"]').text()).toContain('admin.accounts.usageWindow.overdraftStats')
    expect(wrapper.text()).toContain('7 req')
    expect(wrapper.text()).toContain('1.2K')
  })

  it('仅有透支费用时仍显示透支金额，避免额外用量被隐藏', () => {
    const wrapper = mount(UsageProgressBar, {
      props: {
        label: '7d',
        utilization: 100,
        color: 'emerald',
        overdraftStats: {
          requests: 0,
          tokens: 0,
          cost: 12.34,
          user_cost: 12.34
        }
      }
    })

    expect(wrapper.get('[data-testid="overdraft-stats"]').text()).toContain('A $12.34')
    expect(wrapper.get('[data-testid="overdraft-stats"]').text()).toContain('U $12.34')
  })

  it('statsOnly 仅显示本地统计，不伪造配额百分比和重置时间', () => {
    const wrapper = mount(UsageProgressBar, {
      props: {
        label: '5h',
        utilization: 0,
        resetsAt: null,
        statsOnly: true,
        color: 'indigo',
        windowStats: { requests: 3, tokens: 336800, cost: 0.32, user_cost: 0.32 }
      }
    })

    expect(wrapper.text()).toContain('5h')
    expect(wrapper.text()).toContain('3 req')
    expect(wrapper.text()).toContain('336.8K')
    expect(wrapper.text()).not.toContain('0%')
    expect(wrapper.find('.h-1\\.5').exists()).toBe(false)
  })

  it('宽标签模式为 Credits 预留固定空间，避免与进度条重叠', () => {
    const wrapper = mount(UsageProgressBar, {
      props: {
        label: 'Credits',
        utilization: 0,
        wideLabel: true,
        color: 'indigo'
      }
    })

    const label = wrapper.get('span')
    expect(label.classes()).toContain('w-[48px]')
    expect(label.classes()).toContain('whitespace-nowrap')
    expect(label.classes()).not.toContain('w-[32px]')
  })

})
