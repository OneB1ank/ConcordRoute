import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const {
  createAccountMock,
  importCodexSessionMock,
  createOpenAICodexPATMock,
} = vi.hoisted(() => ({
  createAccountMock: vi.fn(),
  importCodexSessionMock: vi.fn(),
  createOpenAICodexPATMock: vi.fn(),
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn(),
    showWarning: vi.fn(),
  }),
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ isSimpleMode: true }),
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      create: createAccountMock,
      checkMixedChannelRisk: vi.fn().mockResolvedValue({ has_risk: false }),
      importCodexSession: importCodexSessionMock,
      createOpenAICodexPAT: createOpenAICodexPATMock,
    },
    settings: {
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
      getSettings: vi.fn().mockResolvedValue({}),
    },
    tlsFingerprintProfiles: {
      list: vi.fn().mockResolvedValue([]),
    },
  },
}))

vi.mock('@/api/admin/accounts', () => ({
  getAntigravityDefaultModelMapping: vi.fn().mockResolvedValue([]),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

import CreateAccountModal from '../CreateAccountModal.vue'

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>',
})

const ModelWhitelistSelectorStub = defineComponent({
  name: 'ModelWhitelistSelector',
  props: {
    modelValue: {
      type: Array,
      default: () => [],
    },
    platform: {
      type: String,
      default: '',
    },
    syncCredentials: {
      type: Object,
      default: undefined,
    },
  },
  emits: ['update:modelValue'],
  template: `
    <div>
      <button
        type="button"
        data-testid="set-openai-model-whitelist"
        @click="$emit('update:modelValue', ['gpt-5.4'])"
      >
        set whitelist
      </button>
      <button
        type="button"
        data-testid="clear-openai-model-whitelist"
        @click="$emit('update:modelValue', [])"
      >
        clear whitelist
      </button>
    </div>
  `,
})

const OAuthAuthorizationFlowStub = defineComponent({
  name: 'OAuthAuthorizationFlow',
  props: {
    showManualOption: Boolean,
    showCodexSessionImportOption: Boolean,
    showAgentIdentityOption: Boolean,
    showCodexPatOption: Boolean,
    initialInputMethod: String,
  },
  data: () => ({ inputMethod: 'manual' }),
  emits: ['import-codex-session', 'import-codex-pat'],
  template: `
    <div>
      <button data-testid="import-codex-session" @click="$emit('import-codex-session', 'session-json')">session</button>
      <button data-testid="import-codex-pat" @click="$emit('import-codex-pat', 'pat-token')">pat</button>
    </div>
  `,
})

const SelectStub = defineComponent({
  name: 'SelectStub',
  props: {
    modelValue: {
      type: [String, Number, Boolean, null],
      default: '',
    },
    options: {
      type: Array,
      default: () => [],
    },
  },
  emits: ['update:modelValue'],
  template: `
    <select
      v-bind="$attrs"
      :value="modelValue"
      @change="$emit('update:modelValue', $event.target.value)"
    >
      <option v-for="option in options" :key="option.value" :value="option.value">
        {{ option.label }}
      </option>
    </select>
  `,
})

function mountModal() {
  return mount(CreateAccountModal, {
    props: { show: true, proxies: [], groups: [] },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        OAuthAuthorizationFlow: OAuthAuthorizationFlowStub,
        ConfirmDialog: true,
        Select: SelectStub,
        Icon: true,
        PlatformIcon: true,
        ProxySelector: true,
        ProxyAdBanner: true,
        GroupSelector: true,
        ModelWhitelistSelector: ModelWhitelistSelectorStub,
        QuotaLimitCard: true,
      },
    },
  })
}

async function selectButtonByText(wrapper: ReturnType<typeof mountModal>, text: string) {
  const button = wrapper.findAll('button').find((candidate) => candidate.text().includes(text))
  expect(button).toBeDefined()
  await button?.trigger('click')
}

async function submitApiKeyAccount(platform: 'openai' | 'anthropic') {
  const wrapper = mountModal()
  await selectButtonByText(wrapper, platform === 'openai' ? 'OpenAI' : 'admin.accounts.claudeConsole')
  if (platform === 'openai') {
    await selectButtonByText(wrapper, 'API Key')
  }
  await wrapper.get('form#create-account-form input[type="text"]').setValue(`${platform} account`)
  await wrapper.get('form#create-account-form input[type="password"]').setValue('test-api-key')
  await wrapper.get('form#create-account-form').trigger('submit.prevent')
  await flushPromises()
  return wrapper
}

async function openCodexImportStep() {
  const wrapper = mountModal()
  await selectButtonByText(wrapper, 'OpenAI')
  await wrapper.get('form#create-account-form input[type="text"]').setValue('Codex import')
  await wrapper.get('form#create-account-form').trigger('submit.prevent')
  return wrapper
}

describe('CreateAccountModal OpenAI account options', () => {
  beforeEach(() => {
    createAccountMock.mockReset().mockResolvedValue({ id: 42, platform: 'openai', type: 'apikey' })
    importCodexSessionMock.mockReset().mockResolvedValue({
      created: 1,
      updated: 0,
      skipped: 0,
      failed: 0,
      errors: [],
      warnings: [],
    })
    createOpenAICodexPATMock.mockReset().mockResolvedValue({})
  })

  it('does not render or submit the removed account-level long-context setting', async () => {
    const wrapper = await submitApiKeyAccount('openai')

    expect(wrapper.find('[data-testid="openai-long-context-billing-toggle"]').exists()).toBe(false)
    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBeUndefined()
  })

  it('does not render or submit the removed upstream billing probe setting', async () => {
    const wrapper = await submitApiKeyAccount('openai')

    expect(wrapper.find('[data-testid="upstream-billing-auto-probe"]').exists()).toBe(false)
    expect(createAccountMock.mock.calls[0]?.[0]?.upstream_billing_probe_enabled).toBeUndefined()
  })

  // namespace 摊平是 OAuth 专属兼容开关，API Key 由自己的协议桥处理。
  it('shows the Codex namespace flatten toggle only for OpenAI OAuth accounts', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')

    expect(wrapper.find('[data-testid="create-openai-flatten-namespaces-toggle"]').exists()).toBe(true)

    await selectButtonByText(wrapper, 'API Key')
    expect(wrapper.find('[data-testid="create-openai-flatten-namespaces-toggle"]').exists()).toBe(false)
  })

  it('exposes Agent Identity in the OpenAI authorization methods', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('OpenAI account')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')

    const flow = wrapper.getComponent(OAuthAuthorizationFlowStub)
    expect(flow.props('showManualOption')).toBe(true)
    expect(flow.props('showCodexSessionImportOption')).toBe(true)
    expect(flow.props('showAgentIdentityOption')).toBe(true)
    expect(flow.props('showCodexPatOption')).toBe(true)
    expect(flow.props('initialInputMethod')).toBe('manual')
  })

  it.each([
    ['camelCase', { authMode: 'agentIdentity', agentIdentity: { agentRuntimeId: 'runtime' } }],
    ['nested identity without auth_mode', { agent_identity: { agent_runtime_id: 'runtime' } }],
  ])('accepts backend-compatible %s Agent Identity imports', async (_name, content) => {
    const wrapper = await openCodexImportStep()
    const flow = wrapper.getComponent(OAuthAuthorizationFlowStub)
    flow.vm.inputMethod = 'agent_identity'

    flow.vm.$emit('import-codex-session', JSON.stringify(content))
    await flushPromises()

    expect(importCodexSessionMock).toHaveBeenCalledTimes(1)
  })

  it('omits the OpenAI setting for non-OpenAI account creation', async () => {
    await submitApiKeyAccount('anthropic')

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBeUndefined()
  })

  it('leaves Codex session import billing ownership to the backend', async () => {
    const wrapper = await openCodexImportStep()
    await wrapper.get('[data-testid="import-codex-session"]').trigger('click')
    await flushPromises()

    expect(importCodexSessionMock).toHaveBeenCalledTimes(1)
    expect(importCodexSessionMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBeUndefined()
  })

  it('leaves Codex PAT import billing ownership to the backend', async () => {
    const wrapper = await openCodexImportStep()
    await wrapper.get('[data-testid="import-codex-pat"]').trigger('click')
    await flushPromises()

    expect(createOpenAICodexPATMock).toHaveBeenCalledTimes(1)
    expect(createOpenAICodexPATMock.mock.calls[0]?.[0]?.extra?.openai_long_context_billing_enabled).toBeUndefined()
  })

  it.each([
    ['Session', 'import-codex-session', importCodexSessionMock],
    ['PAT', 'import-codex-pat', createOpenAICodexPATMock],
  ])('为 Codex %s 导入独立保存最终模型白名单', async (_name, triggerTestId, apiMock) => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    await wrapper.get('[data-testid="set-openai-model-whitelist"]').trigger('click')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('Codex import')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await wrapper.get(`[data-testid="${triggerTestId}"]`).trigger('click')
    await flushPromises()

    expect(apiMock).toHaveBeenCalledTimes(1)
    expect(apiMock.mock.calls[0]?.[0]?.credential_extras).toMatchObject({
      model_whitelist: ['gpt-5.4'],
    })
    expect(apiMock.mock.calls[0]?.[0]?.credential_extras).not.toHaveProperty('model_mapping')
  })

  it('为 Codex Session 导入显式保存空模型白名单', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')
    await wrapper.get('[data-testid="clear-openai-model-whitelist"]').trigger('click')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('Codex import')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await wrapper.get('[data-testid="import-codex-session"]').trigger('click')
    await flushPromises()

    expect(importCodexSessionMock.mock.calls[0]?.[0]?.credential_extras).toMatchObject({
      model_whitelist: [],
    })
    expect(importCodexSessionMock.mock.calls[0]?.[0]?.credential_extras).not.toHaveProperty('model_mapping')
  })

  it('defaults Codex fingerprint convergence to cockpit for OAuth imports', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')

    const modeSelect = wrapper.get<HTMLSelectElement>(
      '[data-testid="create-codex-fingerprint-mode-select"]'
    )
    expect(modeSelect.element.value).toBe('cockpit')

    await wrapper.get('form#create-account-form input[type="text"]').setValue('Codex import')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await wrapper.get('[data-testid="import-codex-session"]').trigger('click')
    await flushPromises()

    expect(importCodexSessionMock.mock.calls[0]?.[0]?.extra).not.toHaveProperty('codex_fingerprint_mode')
  })

  it('persists an explicit Codex fingerprint convergence mode for OAuth imports', async () => {
    const wrapper = mountModal()
    await selectButtonByText(wrapper, 'OpenAI')

    await wrapper.get<HTMLSelectElement>('[data-testid="create-codex-fingerprint-mode-select"]').setValue('session')
    await wrapper.get('form#create-account-form input[type="text"]').setValue('Codex import')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await wrapper.get('[data-testid="import-codex-session"]').trigger('click')
    await flushPromises()

    expect(importCodexSessionMock.mock.calls[0]?.[0]?.extra?.codex_fingerprint_mode).toBe('session')
  })

})

describe('CreateAccountModal Gemini API Key provider source', () => {
  beforeEach(() => {
    createAccountMock.mockReset().mockResolvedValue({ id: 43, platform: 'gemini', type: 'apikey' })
  })

  it('creates a third-party Gemini API Key without an official tier', async () => {
    const wrapper = mountModal()
    await wrapper.get('[data-testid="create-account-platform-gemini"]').trigger('click')
    await wrapper.get('[data-testid="create-gemini-apikey-type"]').trigger('click')

    expect(wrapper.findAll('[data-testid="create-gemini-tier"]').length).toBe(1)

    await wrapper.get<HTMLSelectElement>('[data-testid="create-gemini-provider-type"]').setValue('third_party')
    expect(wrapper.find('[data-testid="create-gemini-tier"]').exists()).toBe(false)

    await wrapper.get('form#create-account-form input[type="text"]').setValue('Third-party Gemini')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('provider-key')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).not.toHaveBeenCalled()

    await wrapper.get<HTMLInputElement>('[data-testid="create-account-base-url"]').setValue('https://provider.example.test')
    await wrapper.get('form#create-account-form').trigger('submit.prevent')
    await flushPromises()

    expect(createAccountMock).toHaveBeenCalledTimes(1)
    expect(createAccountMock.mock.calls[0]?.[0]?.credentials).toMatchObject({
      provider_type: 'third_party',
      base_url: 'https://provider.example.test',
      api_key: 'provider-key'
    })
    expect(createAccountMock.mock.calls[0]?.[0]?.credentials).not.toHaveProperty('tier_id')
  })
})
