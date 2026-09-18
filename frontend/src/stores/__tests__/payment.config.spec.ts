import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createPinia, setActivePinia } from 'pinia'
import { usePaymentStore } from '../payment'
import type { PaymentConfig } from '@/types/payment'

const getConfig = vi.hoisted(() => vi.fn())
vi.mock('@/api/payment', () => ({ paymentAPI: { getConfig } }))

const config = { stripe_publishable_key: 'pk_test_example', payment_enabled: true } as PaymentConfig

// 由测试显式结束请求，验证并发调用确实等待而非提前返回旧值。
function deferred() {
  let resolve!: (value: { data: PaymentConfig }) => void
  let reject!: (error: Error) => void
  const promise = new Promise<{ data: PaymentConfig }>((yes, no) => { resolve = yes; reject = no })
  return { promise, resolve, reject }
}

beforeEach(() => {
  setActivePinia(createPinia())
  getConfig.mockReset()
  vi.spyOn(console, 'error').mockImplementation(() => {})
})
afterEach(() => vi.restoreAllMocks())

describe('支付配置并发请求', () => {
  it('首次并发调用等待同一个响应，不提前返回空配置', async () => {
    const pending = deferred()
    getConfig.mockReturnValue(pending.promise)
    const store = usePaymentStore()
    const first = store.fetchConfig()
    let finished = false
    const second = store.fetchConfig().then(value => { finished = true; return value })
    await Promise.resolve()
    await Promise.resolve()
    expect(finished).toBe(false)
    expect(store.configLoading).toBe(true)
    expect(getConfig).toHaveBeenCalledTimes(1)
    pending.resolve({ data: config })
    expect(await Promise.all([first, second])).toEqual([config, config])
    expect(store.configLoading).toBe(false)
    expect(store.configLoaded).toBe(true)
  })

  it('并发强制刷新共享请求并返回新配置，普通调用保留缓存语义', async () => {
    getConfig.mockResolvedValueOnce({ data: config })
    const store = usePaymentStore()
    await store.fetchConfig()
    const pending = deferred()
    getConfig.mockReturnValueOnce(pending.promise)
    const first = store.fetchConfig(true)
    const second = store.fetchConfig(true)
    expect(await store.fetchConfig()).toEqual(config)
    const updated = { ...config, stripe_publishable_key: 'pk_test_updated' }
    pending.resolve({ data: updated })
    expect(await Promise.all([first, second])).toEqual([updated, updated])
    expect(getConfig).toHaveBeenCalledTimes(2)
    expect(store.configLoading).toBe(false)
  })

  it('首次请求失败后释放共享请求，后续调用可重试', async () => {
    const pending = deferred()
    getConfig.mockReturnValueOnce(pending.promise)
    const store = usePaymentStore()
    const first = store.fetchConfig()
    const second = store.fetchConfig()
    pending.reject(new Error('offline'))
    expect(await Promise.all([first, second])).toEqual([null, null])
    expect(store.configLoading).toBe(false)
    expect(store.configLoaded).toBe(false)
    getConfig.mockResolvedValueOnce({ data: config })
    expect(await store.fetchConfig()).toEqual(config)
    expect(getConfig).toHaveBeenCalledTimes(2)
  })

  it('强制刷新失败保留旧缓存，后续强制刷新仍可成功', async () => {
    getConfig.mockResolvedValueOnce({ data: config })
    const store = usePaymentStore()
    await store.fetchConfig()
    const pending = deferred()
    getConfig.mockReturnValueOnce(pending.promise)
    const first = store.fetchConfig(true)
    const second = store.fetchConfig(true)
    pending.reject(new Error('offline'))
    expect(await Promise.all([first, second])).toEqual([null, null])
    expect(store.configLoading).toBe(false)
    expect(store.configLoaded).toBe(true)
    expect(await store.fetchConfig()).toEqual(config)
    const updated = { ...config, payment_enabled: false }
    getConfig.mockResolvedValueOnce({ data: updated })
    expect(await store.fetchConfig(true)).toEqual(updated)
    expect(getConfig).toHaveBeenCalledTimes(3)
  })

  it('正常缓存命中不重复请求', async () => {
    getConfig.mockResolvedValue({ data: config })
    const store = usePaymentStore()
    await store.fetchConfig()
    expect(await store.fetchConfig()).toEqual(config)
    expect(getConfig).toHaveBeenCalledTimes(1)
  })
})
