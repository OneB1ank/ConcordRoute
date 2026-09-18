<template>
  <BaseDialog :show="plugin !== null" :title="t('admin.plugins.routing') + (plugin ? ' · ' + plugin.name : '')" width="wide" :show-close-button="!busy" :close-on-escape="!busy" @close="emit('close')">
    <form class="space-y-5 p-5" @submit.prevent="submit">
      <p class="text-sm text-gray-500">{{ t('admin.plugins.routingHint') }}</p>
      <fieldset v-for="draft in drafts" :key="draft.capability" class="space-y-3 border-t border-gray-200 pt-4 dark:border-dark-700" :disabled="busy">
        <legend class="break-all font-mono text-xs text-gray-700 dark:text-gray-300">{{ draft.capability }}</legend>
        <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
          <label class="block text-sm">{{ t('admin.plugins.priority') }}
            <input v-model.number="draft.priority" class="input mt-1 w-full" type="number" min="-1000" max="1000" step="1" required>
          </label>
          <label class="block text-sm">{{ t('admin.plugins.rollout') }}
            <input v-model.number="draft.rollout_percent" class="input mt-1 w-full" type="number" min="0" max="100" step="1" required>
          </label>
          <label class="block text-sm">{{ t('admin.plugins.maxConcurrency') }}
            <input v-model.number="draft.max_concurrency" class="input mt-1 w-full" type="number" min="1" max="256" step="1" required>
          </label>
          <label class="block text-sm">{{ t('admin.plugins.timeoutOverride') }}
            <input v-model.number="draft.timeout_ms" class="input mt-1 w-full" type="number" min="0" :max="draft.declaredTimeout" step="1" required>
          </label>
        </div>
        <label class="block text-sm">{{ t('admin.plugins.accountScope') }}
          <input v-model="draft.accounts" class="input mt-1 w-full" type="text" :placeholder="t('admin.plugins.allIDs')" autocomplete="off">
        </label>
        <label class="block text-sm">{{ t('admin.plugins.userScope') }}
          <input v-model="draft.users" class="input mt-1 w-full" type="text" :placeholder="t('admin.plugins.allIDs')" autocomplete="off">
        </label>
        <label class="block text-sm">{{ t('admin.plugins.groupScope') }}
          <input v-model="draft.groups" class="input mt-1 w-full" type="text" :placeholder="t('admin.plugins.allIDs')" autocomplete="off">
        </label>
      </fieldset>
      <p v-if="error" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ error }}</p>
      <div class="flex justify-end gap-2">
        <button type="button" class="btn btn-secondary" :disabled="busy" @click="emit('close')">{{ t('common.cancel') }}</button>
        <button type="submit" class="btn btn-primary" :disabled="busy || drafts.length === 0">{{ t('common.save') }}</button>
      </div>
    </form>
  </BaseDialog>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import type { PluginInstallation, PluginRoutingPolicy } from '@/api/admin'

const props = defineProps<{ plugin: PluginInstallation | null; busy: boolean }>()
const emit = defineEmits<{ close: []; save: [policies: PluginRoutingPolicy[]] }>()
const { t } = useI18n()
interface Draft {
  capability: string
  priority: number
  rollout_percent: number
  max_concurrency: number
  timeout_ms: number
  declaredTimeout: number
  accounts: string
  users: string
  groups: string
}
const drafts = ref<Draft[]>([])
const error = ref('')
watch(() => props.plugin, plugin => {
  error.value = ''
  drafts.value = (plugin?.bindings || []).map(binding => ({
    capability: binding.capability,
    priority: binding.priority ?? 0,
    rollout_percent: binding.rollout_percent,
    max_concurrency: binding.max_concurrency || 32,
    timeout_ms: binding.timeout_ms || 0,
    // 覆盖值还受宿主路由与数据库的 5000ms 上限约束。
    declaredTimeout: Math.min(plugin?.manifest.capabilities.find(cap => cap.id === binding.capability)?.timeout_ms || 5000, 5000),
    accounts: binding.account_ids?.join(', ') || '',
    users: binding.user_ids?.join(', ') || '',
    groups: binding.group_ids?.join(', ') || ''
  }))
}, { immediate: true })
function parseIDs(value: string): number[] {
  if (!value.trim()) return []
  const ids = value.trim().split(/[\s,，]+/u).map(Number)
  if (ids.length > 1000 || ids.some(id => !Number.isSafeInteger(id) || id <= 0) || new Set(ids).size !== ids.length) {
    throw new Error(t('admin.plugins.invalidIDs'))
  }
  return ids.sort((a, b) => a - b)
}
function submit(): void {
  error.value = ''
  try {
    const policies = drafts.value.map(draft => {
      if (!Number.isInteger(draft.priority) || draft.priority < -1000 || draft.priority > 1000 ||
        !Number.isInteger(draft.rollout_percent) || draft.rollout_percent < 0 || draft.rollout_percent > 100 ||
        !Number.isInteger(draft.max_concurrency) || draft.max_concurrency < 1 || draft.max_concurrency > 256 ||
        !Number.isInteger(draft.timeout_ms) || draft.timeout_ms < 0 || draft.timeout_ms > draft.declaredTimeout) {
        throw new Error(t('admin.plugins.invalidRouting'))
      }
      return {
        capability: draft.capability, priority: draft.priority, rollout_percent: draft.rollout_percent,
        max_concurrency: draft.max_concurrency, timeout_ms: draft.timeout_ms,
        account_ids: parseIDs(draft.accounts), user_ids: parseIDs(draft.users), group_ids: parseIDs(draft.groups)
      }
    })
    emit('save', policies)
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : t('admin.plugins.invalidRouting')
  }
}
</script>
