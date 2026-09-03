import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import ModelWhitelistSelector from '../ModelWhitelistSelector.vue'

const {
  syncUpstreamModels,
  syncUpstreamModelsPreview,
  showError,
  showInfo,
  showSuccess,
  copyToClipboard
} = vi.hoisted(() => ({
  syncUpstreamModels: vi.fn(),
  syncUpstreamModelsPreview: vi.fn(),
  showError: vi.fn(),
  showInfo: vi.fn(),
  showSuccess: vi.fn(),
  copyToClipboard: vi.fn().mockResolvedValue(true)
}))

vi.mock('@/api/admin/accounts', () => ({
  accountsAPI: {
    syncUpstreamModels,
    syncUpstreamModelsPreview
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError,
    showInfo,
    showSuccess
  })
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({ copyToClipboard })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) => params ? `${key}:${JSON.stringify(params)}` : key
    })
  }
})

function mountSelector(props: Record<string, unknown> = {}) {
  return mount(ModelWhitelistSelector, {
    props: {
      modelValue: [],
      platform: 'openai',
      ...props
    },
    global: {
      stubs: {
        ModelIcon: true,
        Icon: true
      }
    }
  })
}

describe('ModelWhitelistSelector', () => {
  beforeEach(() => {
    syncUpstreamModels.mockReset()
    syncUpstreamModelsPreview.mockReset()
    showError.mockReset()
    showInfo.mockReset()
    showSuccess.mockReset()
    copyToClipboard.mockReset()
    copyToClipboard.mockResolvedValue(true)
  })

  it('复制模型 ID 时不会选中模型', async () => {
    const wrapper = mountSelector()
    await wrapper.get('div.cursor-pointer').trigger('click')

    const row = wrapper
      .findAll('[data-testid="model-option"]')
      .find(candidate => candidate.text().includes('gpt-5.6-sol'))
    expect(row).toBeTruthy()

    const copyButton = row!.get('[data-testid="copy-model-id"]')
    expect(copyButton.attributes('aria-label')).toBe('common.copy gpt-5.6-sol')
    await copyButton.trigger('click')
    await flushPromises()

    expect(copyToClipboard).toHaveBeenCalledWith('gpt-5.6-sol')
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
  })

  it('模型选择行为保持不变', async () => {
    const wrapper = mountSelector()
    await wrapper.get('div.cursor-pointer').trigger('click')

    const row = wrapper
      .findAll('[data-testid="model-option"]')
      .find(candidate => candidate.text().includes('gpt-5.6-sol'))
    expect(row).toBeTruthy()
    await row!.get('[data-testid="select-model"]').trigger('click')

    expect(wrapper.emitted('update:modelValue')).toEqual([[['gpt-5.6-sol']]])
    expect(copyToClipboard).not.toHaveBeenCalled()
  })

  it('创建账号时使用临时凭证同步上游模型', async () => {
    syncUpstreamModelsPreview.mockResolvedValue({ models: ['gpt-5.1', 'o3', 'gpt-5.1'] })
    const syncCredentials = {
      platform: 'openai',
      type: 'apikey',
      base_url: 'https://openai.example.com/v1',
      api_key: 'openai-key'
    }
    const wrapper = mountSelector({ syncCredentials })

    const button = wrapper.findAll('button').find((item) => item.text().includes('admin.accounts.syncUpstreamModels'))
    expect(button).toBeTruthy()
    await button!.trigger('click')

    expect(syncUpstreamModelsPreview).toHaveBeenCalledWith(syncCredentials)
    expect(syncUpstreamModels).not.toHaveBeenCalled()
    expect(wrapper.emitted('update:modelValue')?.[0]?.[0]).toEqual(['gpt-5.1', 'o3'])
    expect(showSuccess).toHaveBeenCalled()
  })

  it('编辑账号时仍使用账号 ID 同步上游模型', async () => {
    syncUpstreamModels.mockResolvedValue({ models: ['claude-sonnet-4-5'] })
    const wrapper = mountSelector({
      platform: 'anthropic',
      accountId: 7,
      syncCredentials: {
        platform: 'anthropic',
        type: 'apikey',
        api_key: 'should-not-use'
      }
    })

    const button = wrapper.findAll('button').find((item) => item.text().includes('admin.accounts.syncUpstreamModels'))
    expect(button).toBeTruthy()
    await button!.trigger('click')

    expect(syncUpstreamModels).toHaveBeenCalledWith(7)
    expect(syncUpstreamModelsPreview).not.toHaveBeenCalled()
    expect(wrapper.emitted('update:modelValue')?.[0]?.[0]).toEqual(['claude-sonnet-4-5'])
  })

  it('OpenAI OAuth 账号不显示不受支持的实时同步按钮', () => {
    const wrapper = mountSelector({
      platform: 'openai',
      accountId: 2,
      accountType: 'oauth'
    })

    const button = wrapper.findAll('button').find((item) => item.text().includes('admin.accounts.syncUpstreamModels'))
    expect(button).toBeUndefined()
  })

  it('OpenAI API Key 账号继续显示实时同步按钮', () => {
    const wrapper = mountSelector({
      platform: 'openai',
      accountId: 3,
      accountType: 'apikey'
    })

    const button = wrapper.findAll('button').find((item) => item.text().includes('admin.accounts.syncUpstreamModels'))
    expect(button).toBeTruthy()
  })

  it.each([
    ['anthropic', 'service_account'],
    ['gemini', 'service_account'],
    ['antigravity', 'upstream']
  ])('%s 的 %s 账号不显示不受支持的实时同步按钮', (platform, accountType) => {
    const wrapper = mountSelector({ platform, accountId: 4, accountType })

    const button = wrapper.findAll('button').find((item) => item.text().includes('admin.accounts.syncUpstreamModels'))
    expect(button).toBeUndefined()
  })

  it('同步失败时展示 API 普通对象中的具体原因', async () => {
    syncUpstreamModels.mockRejectedValue({
      status: 400,
      message: 'Unsupported OpenAI account type for upstream model sync: oauth'
    })
    const wrapper = mountSelector({ accountId: 2 })

    const button = wrapper.findAll('button').find((item) => item.text().includes('admin.accounts.syncUpstreamModels'))
    expect(button).toBeTruthy()
    await button!.trigger('click')
    await flushPromises()

    expect(showError).toHaveBeenCalledWith(
      'admin.accounts.syncUpstreamModelsError:{"message":"Unsupported OpenAI account type for upstream model sync: oauth"}'
    )
  })

  it('同步失败且 API 未返回原因时不重复 fallback 文本', async () => {
    syncUpstreamModels.mockRejectedValue({ status: 400 })
    const wrapper = mountSelector({ accountId: 2 })

    const button = wrapper.findAll('button').find((item) => item.text().includes('admin.accounts.syncUpstreamModels'))
    expect(button).toBeTruthy()
    await button!.trigger('click')
    await flushPromises()

    expect(showError).toHaveBeenCalledWith('admin.accounts.syncUpstreamModelsFailed')
  })
})
