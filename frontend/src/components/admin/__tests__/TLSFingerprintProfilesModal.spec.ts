import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import zhSettings from '@/i18n/locales/zh/admin/settings'
import enSettings from '@/i18n/locales/en/admin/settings'

const clipboardWriteTextMock = vi.fn()

const {
  listProfilesMock,
  createProfileMock,
  updateProfileMock,
  deleteProfileMock,
  collectorStatusMock,
  startCollectorMock,
  stopCollectorMock,
  createCollectorSessionMock,
  listCollectorCapturesMock,
  showSuccessMock,
  showErrorMock
} = vi.hoisted(() => ({
  listProfilesMock: vi.fn(),
  createProfileMock: vi.fn(),
  updateProfileMock: vi.fn(),
  deleteProfileMock: vi.fn(),
  collectorStatusMock: vi.fn(),
  startCollectorMock: vi.fn(),
  stopCollectorMock: vi.fn(),
  createCollectorSessionMock: vi.fn(),
  listCollectorCapturesMock: vi.fn(),
  showSuccessMock: vi.fn(),
  showErrorMock: vi.fn()
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: showErrorMock,
    showSuccess: showSuccessMock,
    showInfo: vi.fn()
  })
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    tlsFingerprintProfiles: {
      list: listProfilesMock,
      create: createProfileMock,
      update: updateProfileMock,
      delete: deleteProfileMock,
      collectorStatus: collectorStatusMock,
      startCollector: startCollectorMock,
      stopCollector: stopCollectorMock,
      createCollectorSession: createCollectorSessionMock,
      listCollectorCaptures: listCollectorCapturesMock
    }
  }
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) => {
        if (params && typeof params.count !== 'undefined') {
          return `${key}:${params.count}`
        }
        return key
      }
    })
  }
})

import TLSFingerprintProfilesModal from '../TLSFingerprintProfilesModal.vue'

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

const captureRecord = {
  id: 'cap-1',
  captured_at: '2026-05-28T10:00:00Z',
  client_kind: 'codex',
  request_path: '/capture/token/v1/responses',
  method: 'POST',
  user_agent: 'codex-cli/0.1.0',
  ja3_raw: '771,4865-4866,0-10-16,29,0',
  ja3_hash: '1234567890abcdef1234567890abcdef',
  negotiated_alpn: 'http/1.1',
  http_proto: 'HTTP/1.1',
  yaml: 'captured_profile:\n  name: "Codex CLI 2026"\n  enable_grease: true\n',
  headers_summary: {},
  stainless_summary: {},
  profile: {
    id: 0,
    name: 'Codex CLI 2026',
    description: '由内置 TLS 指纹收集器采集',
    enable_grease: true,
    cipher_suites: [4865, 4866],
    curves: [29, 23],
    point_formats: [0],
    signature_algorithms: [1027, 2052],
    alpn_protocols: ['h2', 'http/1.1'],
    supported_versions: [772, 771],
    key_share_groups: [29],
    psk_modes: [1],
    extensions: [0, 10, 16, 43, 51],
    created_at: '',
    updated_at: ''
  }
}

function mountModal() {
  return mount(TLSFingerprintProfilesModal, {
    props: {
      show: true
    },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        ConfirmDialog: true,
        Icon: true
      }
    }
  })
}

async function openCreateForm(wrapper: ReturnType<typeof mountModal>) {
  const createProfileButton = wrapper.findAll('button').find(button =>
    button.text().includes('admin.tlsFingerprintProfiles.createProfile')
  )
  expect(createProfileButton).toBeTruthy()
  await createProfileButton!.trigger('click')
  await flushPromises()
  await wrapper.find('input[required]').setValue('macOS Codex')
}

describe('TLSFingerprintProfilesModal', () => {
  it.each([zhSettings, enSettings])('ALPN 修复提示在实际模板语言包中完整定义', (locale) => {
    for (const key of ['yamlAlpnInvalid', 'alpnExtensionConflict', 'addAlpnExtension', 'clearAlpnProtocols']) {
      expect(locale.tlsFingerprintProfiles.form).toHaveProperty(key, expect.any(String))
    }
  })

  it('原生排序默认关闭，可显式启用并随保存请求提交', async () => {
    const wrapper = mountModal()
    await flushPromises()
    await openCreateForm(wrapper)
    const toggle = wrapper.find('button[role="switch"][aria-label="admin.tlsFingerprintProfiles.form.rustlsNativeOrder"]')
    expect(toggle.attributes('aria-checked')).toBe('false')
    await toggle.trigger('click')
    expect(toggle.attributes('aria-checked')).toBe('true')
    const submit = wrapper.findAll('button').find(button => button.text().includes('common.create'))
    await submit!.trigger('click')
    await flushPromises()
    expect(createProfileMock).toHaveBeenCalledWith(expect.objectContaining({
      rustls_native_order: true,
      enable_grease: false
    }))
    wrapper.unmount()
  })

  it('导入原生排序 YAML 后再导入旧 YAML，不继承上次的开关', async () => {
    const wrapper = mountModal()
    await flushPromises()
    await openCreateForm(wrapper)
    const yaml = wrapper.find('textarea')
    const parse = wrapper.findAll('button').find(button =>
      button.text().includes('admin.tlsFingerprintProfiles.form.parseYaml')
    )!
    await yaml.setValue('name: native\nrustls_native_order: true')
    await parse.trigger('click')
    const toggle = wrapper.find('button[role="switch"]')
    expect(toggle.attributes('aria-checked')).toBe('true')
    await yaml.setValue('name: legacy')
    await parse.trigger('click')
    expect(toggle.attributes('aria-checked')).toBe('false')
    wrapper.unmount()
  })

  it('原生排序与 GREASE 冲突时在保存前提示，不静默改变选项', async () => {
    const wrapper = mountModal()
    await flushPromises()
    await openCreateForm(wrapper)
    await wrapper.find('textarea').setValue('name: mixed\nrustls_native_order: true\nenable_grease: true')
    await wrapper.findAll('button').find(button =>
      button.text().includes('admin.tlsFingerprintProfiles.form.parseYaml')
    )!.trigger('click')
    await wrapper.findAll('button').find(button => button.text().includes('common.create'))!.trigger('click')
    await flushPromises()
    expect(createProfileMock).not.toHaveBeenCalled()
    expect(showErrorMock).toHaveBeenCalledWith('admin.tlsFingerprintProfiles.form.rustlsNativeOrderConflict')
    expect(wrapper.find('button[role="switch"]').attributes('aria-checked')).toBe('true')
    wrapper.unmount()
  })

  beforeEach(() => {
    vi.useFakeTimers()
    listProfilesMock.mockReset()
    createProfileMock.mockReset()
    updateProfileMock.mockReset()
    deleteProfileMock.mockReset()
    collectorStatusMock.mockReset()
    startCollectorMock.mockReset()
    stopCollectorMock.mockReset()
    createCollectorSessionMock.mockReset()
    listCollectorCapturesMock.mockReset()
    showSuccessMock.mockReset()
    showErrorMock.mockReset()
    clipboardWriteTextMock.mockReset()

    Object.defineProperty(navigator, 'clipboard', {
      value: {
        writeText: clipboardWriteTextMock
      },
      configurable: true
    })

    listProfilesMock.mockResolvedValue([])
    createProfileMock.mockResolvedValue({})
    collectorStatusMock.mockResolvedValue({
      running: false,
      listen_address: '127.0.0.1:8443',
      public_base_url: 'https://collector.example:8443',
      using_generated_cert: false,
      session_ttl_seconds: 1800,
      max_records_per_session: 20
    })
    startCollectorMock.mockResolvedValue({
      running: true,
      listen_address: '127.0.0.1:8443',
      public_base_url: 'https://collector.example:8443',
      using_generated_cert: true,
      ca_pem: '-----BEGIN CERTIFICATE-----\\nCA\\n-----END CERTIFICATE-----',
      session_ttl_seconds: 1800,
      max_records_per_session: 20
    })
    createCollectorSessionMock.mockResolvedValue({
      token: 'token-1',
      expires_at: '2026-05-28T10:30:00Z',
      capture_url: 'https://collector.example:8443/capture/token-1',
      ca_pem: '-----BEGIN CERTIFICATE-----\\nCA\\n-----END CERTIFICATE-----'
    })
    listCollectorCapturesMock.mockResolvedValue([captureRecord])
  })

  it('可启动收集器、创建会话并将采集结果填入表单', async () => {
    const wrapper = mountModal()
    await flushPromises()

    expect(wrapper.text()).toContain('admin.tlsFingerprintProfiles.collector.stopped')

    const startButton = wrapper.findAll('button').find(button =>
      button.text().includes('admin.tlsFingerprintProfiles.collector.start')
    )
    expect(startButton).toBeTruthy()
    await startButton!.trigger('click')
    await flushPromises()

    expect(startCollectorMock).toHaveBeenCalledTimes(1)
    expect(collectorStatusMock).toHaveBeenCalledTimes(2)

    collectorStatusMock.mockResolvedValue({
      running: true,
      listen_address: '127.0.0.1:8443',
      public_base_url: 'https://collector.example:8443',
      using_generated_cert: true,
      ca_pem: '-----BEGIN CERTIFICATE-----\\nCA\\n-----END CERTIFICATE-----',
      session_ttl_seconds: 1800,
      max_records_per_session: 20
    })
    await wrapper.findAll('button').find(button =>
      button.text().includes('common.refresh')
    )!.trigger('click')
    await flushPromises()

    const sessionButton = wrapper.findAll('button').find(button =>
      button.text().includes('admin.tlsFingerprintProfiles.collector.createSession')
    )
    expect(sessionButton).toBeTruthy()
    await sessionButton!.trigger('click')
    await flushPromises()

    expect(createCollectorSessionMock).toHaveBeenCalledTimes(1)
    expect(listCollectorCapturesMock).toHaveBeenCalledWith('token-1')
    expect(wrapper.text()).toContain('codex-cli/0.1.0')
    expect(wrapper.text()).toContain('claude --settings')
    expect(wrapper.text()).toContain('"ANTHROPIC_BASE_URL":"https://collector.example:8443/capture/token-1"')
    expect(wrapper.text()).toContain('"ANTHROPIC_AUTH_TOKEN":"token-1"')
    expect(wrapper.text()).toContain('CODEX_CA_CERTIFICATE=/path/to/concordroute-tls-collector-ca.pem codex -c')
    expect(wrapper.text()).toContain('openai_base_url = "https://collector.example:8443/capture/token-1"')

    const applyButton = wrapper.findAll('button').find(button =>
      button.text().includes('admin.tlsFingerprintProfiles.collector.applyCapture')
    )
    expect(applyButton).toBeTruthy()
    await applyButton!.trigger('click')
    await flushPromises()

    expect((wrapper.find('input[required]').element as HTMLInputElement).value).toBe('Codex CLI 2026')
    expect(wrapper.find('textarea').element.value).toContain('captured_profile:')
  })

  it('可在模板列表操作列复制模板 YAML', async () => {
    listProfilesMock.mockResolvedValue([
      {
        id: 12,
        name: 'Mac Codex',
        description: 'export me',
        enable_grease: true,
        cipher_suites: [4865, 4866],
        curves: [29, 23],
        point_formats: [0],
        signature_algorithms: [2052],
        alpn_protocols: ['h2', 'http/1.1'],
        supported_versions: [772, 771],
        key_share_groups: [29],
        psk_modes: [1],
        extensions: [0, 43],
        created_at: '2026-06-01T00:00:00Z',
        updated_at: '2026-06-01T00:00:00Z'
      }
    ])

    const wrapper = mountModal()
    await flushPromises()

    const copyButton = wrapper.find('button[aria-label="admin.tlsFingerprintProfiles.copyYaml"]')
    expect(copyButton.exists()).toBe(true)

    await copyButton.trigger('click')
    await flushPromises()

    expect(clipboardWriteTextMock).toHaveBeenCalledWith([
      'tls_fingerprint_profile:',
      '  name: "Mac Codex"',
      '  description: "export me"',
      '  enable_grease: true',
      '  rustls_native_order: false',
      '  cipher_suites: [0x1301, 0x1302]',
      '  curves: [29, 23]',
      '  point_formats: [0]',
      '  signature_algorithms: [0x0804]',
      '  alpn_protocols: ["h2", "http/1.1"]',
      '  supported_versions: [0x0304, 0x0303]',
      '  key_share_groups: [29]',
      '  psk_modes: [1]',
      '  extensions: [0x0000, 0x002b]'
    ].join('\n'))
    expect(showSuccessMock).toHaveBeenCalledWith('admin.tlsFingerprintProfiles.collector.copied')
  })

  it('在保存前拒绝会被截断的 point_formats 数值', async () => {
    const wrapper = mountModal()
    await flushPromises()
    await openCreateForm(wrapper)

    await wrapper.find('textarea[placeholder="0"]').setValue('256')
    const submitButton = wrapper.findAll('button').find(button => button.text().includes('common.create'))
    await submitButton!.trigger('click')
    await flushPromises()

    expect(createProfileMock).not.toHaveBeenCalled()
    expect(showErrorMock).toHaveBeenCalledWith('admin.tlsFingerprintProfiles.form.numberOutOfRange')
  })

  it('在保存前拒绝重复 ALPN 和缺失的 ALPN 扩展', async () => {
    const wrapper = mountModal()
    await flushPromises()
    await openCreateForm(wrapper)

    const alpnInput = wrapper.find('textarea[placeholder="h2, http/1.1"]')
    const extensionsInput = wrapper.find('textarea[placeholder="0x0000, 0x0005, 0x000a"]')
    const submitButton = wrapper.findAll('button').find(button => button.text().includes('common.create'))

    await alpnInput.setValue('h2, h2')
    await submitButton!.trigger('click')
    await flushPromises()
    expect(showErrorMock).toHaveBeenLastCalledWith('admin.tlsFingerprintProfiles.form.duplicateAlpn')

    showErrorMock.mockClear()
    await alpnInput.setValue('h2, http/1.1')
    await extensionsInput.setValue('0, 10, 11, 13, 43, 45, 51')
    await submitButton!.trigger('click')
    await flushPromises()
    expect(createProfileMock).not.toHaveBeenCalled()
    expect(showErrorMock).toHaveBeenCalledWith('admin.tlsFingerprintProfiles.form.requiredExtension')
  })

  it('保存包含 h2 与 HTTP/1.1 回退的合法 ALPN 模板', async () => {
    const wrapper = mountModal()
    await flushPromises()
    await openCreateForm(wrapper)

    await wrapper.find('textarea[placeholder="h2, http/1.1"]').setValue('h2, http/1.1')
    await wrapper.find('textarea[placeholder="0x0000, 0x0005, 0x000a"]').setValue('16')
    const submitButton = wrapper.findAll('button').find(button => button.text().includes('common.create'))
    await submitButton!.trigger('click')
    await flushPromises()

    expect(showErrorMock).not.toHaveBeenCalled()
    expect(createProfileMock).toHaveBeenCalledWith(expect.objectContaining({
      name: 'macOS Codex',
      alpn_protocols: ['h2', 'http/1.1'],
      extensions: [16]
    }))
  })

  it.each(['', 'alpn_protocols: null', 'alpn_protocols:', 'alpn_protocols: []'])(
    '重新导入无 ALPN 的 YAML 不残留上一模板的协议：%s',
    async (alpnLine) => {
      const wrapper = mountModal()
      await flushPromises()
      await openCreateForm(wrapper)
      const yaml = wrapper.find('textarea')
      const parse = wrapper.findAll('button').find(button =>
        button.text().includes('admin.tlsFingerprintProfiles.form.parseYaml')
      )!
      await yaml.setValue('name: previous\nalpn_protocols: ["h2", "http/1.1"]\nextensions: [0, 16]')
      await parse.trigger('click')
      await yaml.setValue(`name: captured-no-alpn\n${alpnLine}\nextensions: [0, 10, 11, 13, 43, 45, 51]`)
      await parse.trigger('click')
      await wrapper.findAll('button').find(button => button.text().includes('common.create'))!.trigger('click')
      await flushPromises()

      expect(showErrorMock).not.toHaveBeenCalled()
      expect(createProfileMock).toHaveBeenCalledWith(expect.objectContaining({
        name: 'captured-no-alpn',
        alpn_protocols: [],
        extensions: [0, 10, 11, 13, 43, 45, 51]
      }))
      wrapper.unmount()
    }
  )

  it('ALPN 缺少扩展时显式补齐 16，保留原扩展顺序且不自动保存', async () => {
    const wrapper = mountModal()
    await flushPromises()
    await openCreateForm(wrapper)
    const extensions = wrapper.find('textarea[placeholder="0x0000, 0x0005, 0x000a"]')
    await wrapper.find('textarea[placeholder="h2, http/1.1"]').setValue('h2, http/1.1')
    await extensions.setValue('0, 43, 13, 35, 10, 11, 51, 49, 23, 65281, 45')

    const repair = wrapper.findAll('button').find(button =>
      button.text().includes('admin.tlsFingerprintProfiles.form.addAlpnExtension')
    )
    expect(repair).toBeTruthy()
    expect(createProfileMock).not.toHaveBeenCalled()
    expect(extensions.element.value).toBe('0, 43, 13, 35, 10, 11, 51, 49, 23, 65281, 45')
    await repair!.trigger('click')
    expect(createProfileMock).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid="alpn-extension-conflict"]').exists()).toBe(false)
    await wrapper.findAll('button').find(button => button.text().includes('common.create'))!.trigger('click')
    await flushPromises()
    expect(createProfileMock).toHaveBeenCalledWith(expect.objectContaining({
      alpn_protocols: ['h2', 'http/1.1'],
      extensions: [0, 43, 13, 35, 10, 11, 51, 49, 23, 65281, 45, 16]
    }))
    wrapper.unmount()
  })

  it('ALPN 冲突时可明确清空协议，保存无 ALPN 实采而不增添扩展', async () => {
    const wrapper = mountModal()
    await flushPromises()
    await openCreateForm(wrapper)
    const alpn = wrapper.find('textarea[placeholder="h2, http/1.1"]')
    const extensions = wrapper.find('textarea[placeholder="0x0000, 0x0005, 0x000a"]')
    await alpn.setValue('http/1.1')
    await extensions.setValue('0, 43, 13, 35, 10, 11, 51, 49, 23, 65281, 45')
    const clear = wrapper.findAll('button').find(button =>
      button.text().includes('admin.tlsFingerprintProfiles.form.clearAlpnProtocols')
    )
    expect(clear).toBeTruthy()
    await clear!.trigger('click')
    expect(alpn.element.value).toBe('')
    expect(extensions.element.value).toBe('0, 43, 13, 35, 10, 11, 51, 49, 23, 65281, 45')
    expect(createProfileMock).not.toHaveBeenCalled()
    await wrapper.findAll('button').find(button => button.text().includes('common.create'))!.trigger('click')
    await flushPromises()
    expect(createProfileMock).toHaveBeenCalledWith(expect.objectContaining({
      alpn_protocols: [],
      extensions: [0, 43, 13, 35, 10, 11, 51, 49, 23, 65281, 45]
    }))
    wrapper.unmount()
  })

  it('ALPN 导入格式错误时报告错误，不清空已有协议', async () => {
    const wrapper = mountModal()
    await flushPromises()
    await openCreateForm(wrapper)
    const alpn = wrapper.find('textarea[placeholder="h2, http/1.1"]')
    await alpn.setValue('h2')
    await wrapper.find('textarea').setValue('name: malformed\nalpn_protocols: [h2')
    await wrapper.findAll('button').find(button =>
      button.text().includes('admin.tlsFingerprintProfiles.form.parseYaml')
    )!.trigger('click')
    expect(showErrorMock).toHaveBeenCalledWith('admin.tlsFingerprintProfiles.form.yamlAlpnInvalid')
    expect(alpn.element.value).toBe('h2')
    expect(createProfileMock).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it.each([
    ['h2', ''],
    ['h2', '0, 16'],
    ['', '0, 13'],
    ['h2', '0, invalid'],
    ['h2, h2', '0, 13']
  ])('默认、合法或格式错误的字段不显示 ALPN 缺扩展修复：%s / %s', async (alpn, extensions) => {
    const wrapper = mountModal()
    await flushPromises()
    await openCreateForm(wrapper)
    await wrapper.find('textarea[placeholder="h2, http/1.1"]').setValue(alpn)
    await wrapper.find('textarea[placeholder="0x0000, 0x0005, 0x000a"]').setValue(extensions)
    expect(wrapper.find('[data-testid="alpn-extension-conflict"]').exists()).toBe(false)
    expect(createProfileMock).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('补齐 ALPN 扩展时保留末尾 PSK 的位置约束', async () => {
    const wrapper = mountModal()
    await flushPromises()
    await openCreateForm(wrapper)
    await wrapper.find('textarea[placeholder="h2, http/1.1"]').setValue('http/1.1')
    await wrapper.find('textarea[placeholder="0x0000, 0x0005, 0x000a"]').setValue('0, 13, 43, 51, 41')
    await wrapper.findAll('button').find(button =>
      button.text().includes('admin.tlsFingerprintProfiles.form.addAlpnExtension')
    )!.trigger('click')
    await wrapper.findAll('button').find(button => button.text().includes('common.create'))!.trigger('click')
    await flushPromises()
    expect(createProfileMock).toHaveBeenCalledWith(expect.objectContaining({
      alpn_protocols: ['http/1.1'],
      extensions: [0, 13, 43, 51, 16, 41]
    }))
    wrapper.unmount()
  })

  it('编辑旧模板时选择清空 ALPN 仅更新原模板，不创建或应用新模板', async () => {
    listProfilesMock.mockResolvedValue([{
      ...captureRecord.profile,
      id: 19,
      name: '本机代理',
      enable_grease: false,
      extensions: [0, 43, 13, 35, 10, 11, 51, 49, 23, 65281, 45]
    }])
    const wrapper = mountModal()
    await flushPromises()
    await wrapper.find('button[title="common.edit"]').trigger('click')
    await wrapper.findAll('button').find(button =>
      button.text().includes('admin.tlsFingerprintProfiles.form.clearAlpnProtocols')
    )!.trigger('click')
    expect(updateProfileMock).not.toHaveBeenCalled()
    await wrapper.findAll('button').find(button => button.text().includes('common.update'))!.trigger('click')
    await flushPromises()
    expect(updateProfileMock).toHaveBeenCalledWith(19, expect.objectContaining({
      alpn_protocols: [],
      extensions: [0, 43, 13, 35, 10, 11, 51, 49, 23, 65281, 45]
    }))
    expect(createProfileMock).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('没有模板名称的失败导入不清空正在编辑的 ALPN', async () => {
    const wrapper = mountModal()
    await flushPromises()
    await openCreateForm(wrapper)
    const alpn = wrapper.find('textarea[placeholder="h2, http/1.1"]')
    await alpn.setValue('h2')
    await wrapper.find('textarea').setValue('description: incomplete')
    await wrapper.findAll('button').find(button =>
      button.text().includes('admin.tlsFingerprintProfiles.form.parseYaml')
    )!.trigger('click')
    expect(showErrorMock).toHaveBeenCalledWith('admin.tlsFingerprintProfiles.form.yamlParseFailed')
    expect(alpn.element.value).toBe('h2')
    wrapper.unmount()
  })

  it('使用默认 TLS 1.3 版本时拒绝缺少 key_share 的自定义扩展', async () => {
    const wrapper = mountModal()
    await flushPromises()
    await openCreateForm(wrapper)

    await wrapper.find('textarea[placeholder="0x0000, 0x0005, 0x000a"]').setValue('43, 13')
    const submitButton = wrapper.findAll('button').find(button => button.text().includes('common.create'))
    await submitButton!.trigger('click')
    await flushPromises()

    expect(createProfileMock).not.toHaveBeenCalled()
    expect(showErrorMock).toHaveBeenCalledWith('admin.tlsFingerprintProfiles.form.tls13RequiredExtension')
  })
})
