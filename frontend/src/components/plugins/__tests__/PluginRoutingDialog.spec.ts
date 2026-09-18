import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import type { PluginInstallation } from '@/api/admin/plugins'
import PluginRoutingDialog from '../PluginRoutingDialog.vue'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

function mountRouting(timeout: number) {
  const plugin = {
    id: 1,
    name: 'routing test',
    manifest: { capabilities: [{ id: 'test.capability', timeout_ms: timeout }] },
    bindings: [{ capability: 'test.capability', priority: 0, rollout_percent: 100, max_concurrency: 32, timeout_ms: 0 }],
  } as PluginInstallation
  return mount(PluginRoutingDialog, {
    props: { plugin, busy: false },
    global: { stubs: { BaseDialog: { template: '<div><slot /></div>' } } },
  })
}

describe('plugin routing timeout bounds', () => {
  // 覆盖值同时受清单声明和后端的 5000ms 上限约束，0 表示沿用默认值。
  it.each([[120000, 5000], [200, 200]])('bounds manifest %i to %i', async (declared, limit) => {
    const wrapper = mountRouting(declared)
    try {
      const timeout = wrapper.findAll('input[type="number"]')[3]!
      expect(timeout.attributes('max')).toBe(String(limit))
      await timeout.setValue(limit + 1)
      await wrapper.find('form').trigger('submit')
      expect(wrapper.emitted('save')).toBeUndefined()
      expect(wrapper.find('[role="alert"]').exists()).toBe(true)
      await timeout.setValue(limit)
      await wrapper.find('form').trigger('submit')
      expect(wrapper.emitted('save')).toHaveLength(1)
    } finally {
      wrapper.unmount()
    }
  })
})
