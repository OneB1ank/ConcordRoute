import { afterEach, describe, expect, it, vi } from 'vitest'
import apiClient from '@/api/client'
import { getConfig, saveConfig } from '@/api/admin/plugins'

vi.mock('@/i18n', () => ({ getLocale: () => 'zh-CN' }))

const originalAdapter = apiClient.defaults.adapter
afterEach(() => {
  apiClient.defaults.adapter = originalAdapter
})

// 使用真实响应拦截器，确保只拆外层信封，不改变插件自己的同名字段。
describe('plugin config response envelope', () => {
  it.each([0, 401, 'custom'])('preserves config.code = %s on read and save', async code => {
    const config = { code, data: { nested: true }, message: 'plugin setting' }
    apiClient.defaults.adapter = async request => ({
      config: request,
      status: 200,
      statusText: 'OK',
      headers: {},
      data: { code: 0, message: 'success', data: config },
    })
    await expect(getConfig(1)).resolves.toEqual(config)
    await expect(saveConfig(1, config)).resolves.toEqual(config)
  })
})
