<template>
  <BaseDialog :show="plugin !== null" :title="t('admin.plugins.hostServices') + (plugin ? ' · ' + plugin.name : '')" width="wide" :show-close-button="!busy" :close-on-escape="!busy" @close="emit('close')">
    <div class="space-y-5 p-5">
      <p v-if="loading" class="text-sm text-gray-500">{{ t('common.loading') }}</p>
      <p v-if="error" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ error }}</p>
      <template v-if="stats">
        <dl class="grid grid-cols-2 gap-2 text-sm">
          <dt>{{ t('admin.plugins.hostLogs') }}</dt><dd>{{ stats.logs }}</dd>
          <dt>{{ t('admin.plugins.hostEvents') }}</dt><dd>{{ stats.events_accepted }}</dd>
          <dt>{{ t('admin.plugins.hostDropped') }}</dt><dd>{{ stats.events_dropped }}</dd>
          <template v-for="(value, name) in stats.metrics" :key="name">
            <dt class="font-mono text-xs">{{ name }}</dt><dd>{{ value }}</dd>
          </template>
        </dl>
        <details v-if="stats.recent_events?.length" class="text-sm">
          <summary class="cursor-pointer">{{ t('admin.plugins.recentHostEvents') }}</summary>
          <ul class="mt-2 space-y-1 font-mono text-xs">
            <li v-for="event in stats.recent_events" :key="event.sequence">{{ event.name }} · {{ event.value }} · {{ new Date(event.time).toLocaleTimeString() }}</li>
          </ul>
        </details>
      </template>
      <template v-if="secretCapabilities.length">
        <p class="text-sm text-gray-500">{{ t('admin.plugins.secretNotice') }}</p>
        <ul class="space-y-2">
          <li v-for="grant in grants" :key="grant.capability + grant.alias" class="flex flex-wrap items-center justify-between gap-2 border-t border-gray-200 pt-2 text-sm dark:border-dark-700">
            <div>
              <p class="font-mono">{{ grant.alias }}</p>
              <p class="text-xs text-gray-500">{{ grant.capability }} · {{ new Date(grant.expires_at).toLocaleString() }}</p>
            </div>
            <button type="button" class="btn btn-secondary btn-sm" :disabled="busy" @click="emit('revoke', grant)">{{ t('admin.plugins.revokeSecret') }}</button>
          </li>
        </ul>
        <form class="space-y-3 border-t border-gray-200 pt-4 dark:border-dark-700" @submit.prevent="submit">
          <fieldset class="block text-sm" :disabled="busy">
            <legend>{{ t('admin.plugins.capabilities') }}</legend>
            <div class="mt-1 flex flex-wrap gap-2">
              <label v-for="cap in secretCapabilities" :key="cap.id" class="inline-flex cursor-pointer items-center gap-2 rounded border border-gray-200 px-2 py-1 font-mono text-xs dark:border-dark-700">
                <input v-model="capability" type="radio" name="plugin-secret-capability" :value="cap.id">
                <span>{{ cap.id }}</span>
              </label>
            </div>
          </fieldset>
          <label class="block text-sm">{{ t('admin.plugins.secretAlias') }}
            <input v-model="alias" class="input mt-1 w-full" type="text" pattern="[a-z][a-z0-9_-]{0,63}" maxlength="64" autocomplete="off" required :disabled="busy">
          </label>
          <label class="block text-sm">{{ t('admin.plugins.secretValue') }}
            <input v-model="secret" class="input mt-1 w-full" type="password" maxlength="16384" autocomplete="new-password" required :disabled="busy">
          </label>
          <label class="block text-sm">{{ t('admin.plugins.secretMinutes') }}
            <input v-model.number="minutes" class="input mt-1 w-full" type="number" min="1" max="60" step="1" required :disabled="busy">
          </label>
          <p v-if="validationError" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ validationError }}</p>
          <button type="submit" class="btn btn-primary" :disabled="busy">{{ t('admin.plugins.grantSecret') }}</button>
        </form>
      </template>
      <div class="flex justify-end">
        <button type="button" class="btn btn-secondary" :disabled="loading || busy" @click="emit('refresh')">{{ t('common.refresh') }}</button>
      </div>
    </div>
  </BaseDialog>
</template>
<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import type { PluginHostSnapshot, PluginInstallation, PluginSecretGrant } from '@/api/admin'

const props = defineProps<{
  plugin: PluginInstallation | null; stats: PluginHostSnapshot | null; grants: PluginSecretGrant[]
  loading: boolean; busy: boolean; error: string
}>()
const emit = defineEmits<{
  close: []; refresh: []; revoke: [grant: PluginSecretGrant]
  grant: [capability: string, alias: string, value: string, ttlSeconds: number]
}>()
const { t } = useI18n()
const secretCapabilities = computed(() => props.plugin?.manifest.capabilities.filter(cap => cap.permissions?.includes('secrets.broker')) || [])
const capability = ref('')
const alias = ref('')
const secret = ref('')
const minutes = ref(15)
const validationError = ref('')
watch(() => props.plugin, () => {
  capability.value = secretCapabilities.value[0]?.id || ''
  alias.value = ''
  secret.value = ''
  validationError.value = ''
}, { immediate: true })
function submit(): void {
  if (!capability.value || !/^[a-z][a-z0-9_-]{0,63}$/.test(alias.value) || !secret.value ||
    !Number.isInteger(minutes.value) || minutes.value < 1 || minutes.value > 60) {
    validationError.value = t('admin.plugins.invalidSecret')
    return
  }
  validationError.value = ''
  emit('grant', capability.value, alias.value, secret.value, minutes.value * 60)
  secret.value = ''
}
</script>
