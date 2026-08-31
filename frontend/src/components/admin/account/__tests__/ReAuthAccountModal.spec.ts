import { describe, expect, it, beforeEach, vi } from 'vitest'
import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import type { Account } from '@/types'
import ReAuthAccountModal from '../ReAuthAccountModal.vue'

const {
  showSuccessMock,
  showErrorMock,
  updateAccountMock,
  clearErrorMock,
  applyOAuthCredentialsMock,
  generateOpenAIAuthUrlMock,
  exchangeAuthCodeMock,
  buildCredentialsMock,
  buildExtraInfoMock
} = vi.hoisted(() => ({
  showSuccessMock: vi.fn(),
  showErrorMock: vi.fn(),
  updateAccountMock: vi.fn(),
  clearErrorMock: vi.fn(),
  applyOAuthCredentialsMock: vi.fn(),
  generateOpenAIAuthUrlMock: vi.fn(),
  exchangeAuthCodeMock: vi.fn(),
  buildCredentialsMock: vi.fn(),
  buildExtraInfoMock: vi.fn()
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showSuccess: showSuccessMock,
    showError: showErrorMock
  })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      update: updateAccountMock,
      clearError: clearErrorMock,
      applyOAuthCredentials: applyOAuthCredentialsMock
    }
  }
}))

vi.mock('@/composables/useAccountOAuth', () => ({
  useAccountOAuth: () => ({
    authUrl: { value: '' },
    sessionId: { value: '' },
    loading: { value: false },
    error: { value: '' },
    resetState: vi.fn(),
    generateAuthUrl: vi.fn(),
    buildExtraInfo: vi.fn()
  })
}))

vi.mock('@/composables/useOpenAIOAuth', () => ({
  useOpenAIOAuth: () => ({
    authUrl: { value: 'https://auth.example.test' },
    sessionId: { value: 'session-1' },
    oauthState: { value: 'state-1' },
    loading: { value: false },
    error: { value: '' },
    resetState: vi.fn(),
    generateAuthUrl: generateOpenAIAuthUrlMock,
    exchangeAuthCode: exchangeAuthCodeMock,
    buildCredentials: buildCredentialsMock,
    buildExtraInfo: buildExtraInfoMock
  })
}))

vi.mock('@/composables/useGeminiOAuth', () => ({
  useGeminiOAuth: () => ({
    authUrl: { value: '' },
    sessionId: { value: '' },
    state: { value: '' },
    loading: { value: false },
    error: { value: '' },
    resetState: vi.fn(),
    generateAuthUrl: vi.fn(),
    exchangeAuthCode: vi.fn(),
    buildCredentials: vi.fn()
  })
}))

vi.mock('@/composables/useAntigravityOAuth', () => ({
  useAntigravityOAuth: () => ({
    authUrl: { value: '' },
    sessionId: { value: '' },
    state: { value: '' },
    loading: { value: false },
    error: { value: '' },
    resetState: vi.fn(),
    generateAuthUrl: vi.fn(),
    exchangeAuthCode: vi.fn(),
    buildCredentials: vi.fn()
  })
}))

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: {
    show: {
      type: Boolean,
      default: false
    }
  },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})

const OAuthAuthorizationFlowStub = defineComponent({
  name: 'OAuthAuthorizationFlow',
  emits: ['generate-url'],
  setup(_, { expose }) {
    expose({
      authCode: 'auth-code-1',
      oauthState: 'state-1',
      projectId: '',
      sessionKey: '',
      inputMethod: 'manual',
      reset: vi.fn()
    })
    return {}
  },
  template: '<button data-testid="oauth-generate" @click="$emit(\'generate-url\')">generate</button>'
})

function openAIAccount(): Account {
  return {
    id: 101,
    name: 'OpenAI OAuth',
    platform: 'openai',
    type: 'oauth',
    credentials: {},
    extra: {
      tls_fingerprint_router_id: 9
    },
    proxy_id: 12,
    concurrency: 1,
    priority: 0,
    status: 'error',
    error_message: 'old error',
    last_used_at: null,
    expires_at: null,
    auto_pause_on_expired: false,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    schedulable: true,
    rate_limited_at: null,
    rate_limit_reset_at: null,
    overload_until: null,
    temp_unschedulable_until: null,
    temp_unschedulable_reason: null,
    session_window_start: null,
    session_window_end: null,
    session_window_status: null,
    tls_fingerprint_router_id: 9
  }
}

describe('admin/account/ReAuthAccountModal', () => {
  beforeEach(() => {
    showSuccessMock.mockReset()
    showErrorMock.mockReset()
    updateAccountMock.mockReset()
    clearErrorMock.mockReset()
    applyOAuthCredentialsMock.mockReset()
    generateOpenAIAuthUrlMock.mockReset()
    exchangeAuthCodeMock.mockReset()
    buildCredentialsMock.mockReset()
    buildExtraInfoMock.mockReset()

    exchangeAuthCodeMock.mockResolvedValue({
      access_token: 'new-access-token',
      refresh_token: 'new-refresh-token',
      email: 'user@example.test'
    })
    buildCredentialsMock.mockReturnValue({
      access_token: 'new-access-token',
      refresh_token: 'new-refresh-token'
    })
    buildExtraInfoMock.mockReturnValue({
      email: 'user@example.test'
    })
    applyOAuthCredentialsMock.mockResolvedValue({
      ...openAIAccount(),
      status: 'active',
      error_message: null
    })
  })

  it('生成重新授权链接时绑定账号 TLS Router', async () => {
    const wrapper = mount(ReAuthAccountModal, {
      props: {
        show: true,
        account: openAIAccount()
      },
      global: {
        stubs: {
          BaseDialog: BaseDialogStub,
          OAuthAuthorizationFlow: OAuthAuthorizationFlowStub,
          Icon: true
        }
      }
    })
    await flushPromises()

    await wrapper.find('[data-testid="oauth-generate"]').trigger('click')
    await flushPromises()

    expect(generateOpenAIAuthUrlMock).toHaveBeenCalledWith(12, undefined, 9)
  })

  it('OpenAI 重新授权使用增量合并接口保留 TLS Router 绑定', async () => {
    const wrapper = mount(ReAuthAccountModal, {
      props: {
        show: true,
        account: openAIAccount()
      },
      global: {
        stubs: {
          BaseDialog: BaseDialogStub,
          OAuthAuthorizationFlow: OAuthAuthorizationFlowStub,
          Icon: true
        }
      }
    })
    await flushPromises()

    await wrapper.find('button.btn-primary').trigger('click')
    await flushPromises()

    expect(exchangeAuthCodeMock).toHaveBeenCalledWith(
      'auth-code-1',
      'session-1',
      'state-1',
      12,
      9
    )
    expect(applyOAuthCredentialsMock).toHaveBeenCalledWith(101, {
      type: 'oauth',
      credentials: {
        access_token: 'new-access-token',
        refresh_token: 'new-refresh-token'
      },
      extra: {
        email: 'user@example.test'
      }
    })
    expect(updateAccountMock).not.toHaveBeenCalled()
    expect(clearErrorMock).not.toHaveBeenCalled()
    expect(wrapper.emitted('reauthorized')?.[0]?.[0]).toMatchObject({
      id: 101,
      status: 'active',
      error_message: null
    })
  })
})
