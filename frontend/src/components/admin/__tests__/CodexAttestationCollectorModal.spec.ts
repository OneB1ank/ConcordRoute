import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import CodexAttestationCollectorModal from '../CodexAttestationCollectorModal.vue'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key === 'admin.codexAttestationCollector.title'
        ? 'Codex app-server Attestation Collector'
        : key
    })
  }
})

describe('CodexAttestationCollectorModal', () => {
  it('renders the collector card as a dedicated tools modal', () => {
    const wrapper = mount(CodexAttestationCollectorModal, {
      props: { show: true },
      global: {
        stubs: {
          BaseDialog: {
            props: ['show', 'title'],
            template: '<div data-test="dialog" :data-show="show" @click="$emit(\'close\')"><h1>{{ title }}</h1><slot /></div>'
          },
          CodexAttestationCollectorCard: {
            props: ['show'],
            template: '<div data-test="collector-card" :data-show="show" />'
          }
        }
      }
    })

    expect(wrapper.get('[data-test="dialog"]').attributes('data-show')).toBe('true')
    expect(wrapper.get('[data-test="collector-card"]').attributes('data-show')).toBe('true')
    expect(wrapper.text()).toContain('Codex app-server Attestation Collector')
  })

  it('forwards the dialog close event', async () => {
    const wrapper = mount(CodexAttestationCollectorModal, {
      props: { show: true },
      global: {
        stubs: {
          BaseDialog: { template: '<div data-test="dialog" @click="$emit(\'close\')"><slot /></div>' },
          CodexAttestationCollectorCard: true
        }
      }
    })

    await wrapper.get('[data-test="dialog"]').trigger('click')
    expect(wrapper.emitted('close')).toHaveLength(1)
  })
})
