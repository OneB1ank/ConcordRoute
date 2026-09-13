import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const {
  statusMock,
  startMock,
  stopMock,
  createSessionMock,
  listCapturesMock,
  deleteSessionMock,
  showSuccessMock,
  showErrorMock
} = vi.hoisted(() => ({
  statusMock: vi.fn(),
  startMock: vi.fn(),
  stopMock: vi.fn(),
  createSessionMock: vi.fn(),
  listCapturesMock: vi.fn(),
  deleteSessionMock: vi.fn(),
  showSuccessMock: vi.fn(),
  showErrorMock: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    codexAttestationCollector: {
      status: statusMock,
      start: startMock,
      stop: stopMock,
      createSession: createSessionMock,
      listCaptures: listCapturesMock,
      deleteSession: deleteSessionMock
    }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showSuccess: showSuccessMock, showError: showErrorMock })
}))

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

import CodexAttestationCollectorCard from '../CodexAttestationCollectorCard.vue'

const runningStatus = {
  running: true,
  session_ttl_seconds: 1800,
  max_records_per_session: 100,
  active_sessions: 0
}

const session = {
  token: 'collector-token',
  expires_at: '2026-09-13T10:00:00Z',
  bridge_query: 'collector_token=collector-token',
  header_name: 'X-Codex-Attestation-Collector-Token'
}

function findButton(wrapper: ReturnType<typeof mount>, suffix: string) {
  return wrapper.findAll('button').find((button) => button.text().includes(suffix))!
}

describe('CodexAttestationCollectorCard', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    statusMock.mockResolvedValue(runningStatus)
    listCapturesMock.mockResolvedValue([])
    deleteSessionMock.mockResolvedValue({ deleted: true })
  })

  it('shows scope boundaries and the complete bridge endpoint', async () => {
    createSessionMock.mockResolvedValue(session)
    const wrapper = mount(CodexAttestationCollectorCard, { props: { show: true } })
    await flushPromises()

    expect(wrapper.text()).toContain('admin.codexAttestationCollector.scopeIdentity')
    await findButton(wrapper, 'admin.codexAttestationCollector.createSession').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('/backend-api/codex/app-server?collector_token=collector-token')
    wrapper.unmount()
  })

  it('deletes the active capture session and stops polling', async () => {
    createSessionMock.mockResolvedValue(session)
    const wrapper = mount(CodexAttestationCollectorCard, { props: { show: true } })
    await flushPromises()
    await findButton(wrapper, 'admin.codexAttestationCollector.createSession').trigger('click')
    await flushPromises()

    await findButton(wrapper, 'admin.codexAttestationCollector.deleteSession').trigger('click')
    await flushPromises()

    expect(deleteSessionMock).toHaveBeenCalledWith('collector-token')
    expect(showSuccessMock).toHaveBeenCalledWith('admin.codexAttestationCollector.sessionDeleted')
    expect(wrapper.text()).not.toContain('/backend-api/codex/app-server?collector_token=collector-token')
    wrapper.unmount()
  })
})
