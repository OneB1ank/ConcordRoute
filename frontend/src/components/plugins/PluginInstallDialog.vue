<template>
  <BaseDialog :show="inspection !== null" :title="t('admin.plugins.publisherReviewTitle')" @close="!busy && emit('close')">
    <div v-if="inspection" class="space-y-5 p-5">
      <div>
        <h3 class="break-words text-lg font-semibold text-gray-900 dark:text-white">{{ inspection.manifest.name }}</h3>
        <p class="mt-1 text-sm text-gray-500">v{{ inspection.manifest.version }} · {{ fileName }}</p>
        <p v-if="inspection.manifest.description" class="mt-2 line-clamp-3 break-words text-sm text-gray-600 dark:text-gray-300">{{ inspection.manifest.description }}</p>
      </div>
      <div class="rounded-xl border border-amber-200 bg-amber-50 p-4 dark:border-amber-900 dark:bg-amber-950/40">
        <p class="font-medium text-amber-900 dark:text-amber-200">{{ t('admin.plugins.firstPublisher') }}</p>
        <p class="mt-2 text-sm text-amber-800 dark:text-amber-300">{{ t('admin.plugins.publisherReviewHelp') }}</p>
        <p class="mt-3 break-all text-sm"><span class="text-gray-500">{{ t('admin.plugins.publisherID') }}：</span>{{ inspection.publisher?.key_id }}</p>
      </div>
      <div>
        <h4 class="text-sm font-medium">{{ t('admin.plugins.requestedAccess') }}</h4>
        <ul class="mt-2 space-y-1 text-sm text-gray-600 dark:text-gray-300">
          <li v-for="permission in permissions" :key="permission">{{ permissionLabel(permission) }}</li>
        </ul>
        <p v-if="isLegacyTransport" class="mt-2 text-sm text-gray-600 dark:text-gray-300">{{ t('admin.plugins.legacyTransportAccess') }}</p>
        <p v-if="inspection.runtime_isolation !== 'container'" class="mt-3 text-sm text-gray-600 dark:text-gray-300">{{ t('admin.plugins.processAccessNotice') }}</p>
      </div>
      <details class="text-xs text-gray-500">
        <summary class="cursor-pointer">{{ t('admin.plugins.signatureDetails') }}</summary>
        <dl class="mt-3 space-y-2">
          <dt>{{ t('admin.plugins.publisherFingerprint') }}</dt><dd class="break-all font-mono">{{ inspection.publisher?.fingerprint }}</dd>
          <dt>{{ t('admin.plugins.packageDigest') }}</dt><dd class="break-all font-mono">{{ inspection.package_sha256 }}</dd>
        </dl>
      </details>
      <p v-if="!inspection.compatibility.compatible" role="alert" class="text-sm text-red-600">{{ inspection.compatibility.message }}</p>
      <label class="flex cursor-pointer items-start gap-3 rounded-lg border border-gray-200 p-3 text-sm dark:border-dark-700">
        <input v-model="consent" type="checkbox" class="mt-0.5" :disabled="busy" data-testid="publisher-consent">
        <span>{{ t('admin.plugins.trustPublisherConsent') }}</span>
      </label>
      <p class="text-xs text-gray-500">{{ t('admin.plugins.installCompiledHelp') }}</p>
      <div class="flex justify-end gap-3">
        <button type="button" class="btn btn-secondary" :disabled="busy" @click="emit('close')">{{ t('common.cancel') }}</button>
        <button type="button" class="btn btn-primary" :disabled="!consent || busy || !inspection.compatibility.compatible || !inspection.publisher" data-testid="confirm-publisher-install" @click="emit('confirm')">
          {{ busy ? t('common.loading') : t(upgrade ? 'admin.plugins.trustAndUpgrade' : 'admin.plugins.trustAndInstall') }}
        </button>
      </div>
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import type { PluginPackageInspection } from '@/api/admin/plugins'

const props = defineProps<{ inspection: PluginPackageInspection | null; fileName: string; busy: boolean; upgrade: boolean }>()
const emit = defineEmits<{ close: []; confirm: [] }>()
const { t, te } = useI18n()
const consent = ref(false)
watch(() => props.inspection, () => { consent.value = false })
const permissions = computed(() => [...new Set(props.inspection?.manifest.capabilities.flatMap(capability => capability.permissions ?? []) ?? [])])
const isLegacyTransport = computed(() => props.inspection?.manifest.schema_version === 1)
function permissionLabel(permission: string): string {
  const key = `admin.plugins.permissionLabels.${permission.replace(/\./g, '_')}`
  return te(key) ? t(key) : permission
}
</script>
