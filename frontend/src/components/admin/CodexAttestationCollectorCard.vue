<template>
  <section class="rounded-lg border border-amber-200 bg-amber-50/60 p-4 dark:border-amber-800 dark:bg-amber-950/20">
    <div class="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
      <div>
        <div class="flex items-center gap-2">
          <Icon name="shield" size="sm" class="text-amber-600 dark:text-amber-400" />
          <h4 class="text-sm font-medium text-gray-900 dark:text-white">{{ t('admin.codexAttestationCollector.title') }}</h4>
          <span :class="['rounded-full px-2 py-0.5 text-xs font-medium', status?.running ? 'bg-green-100 text-green-700 dark:bg-green-900/40 dark:text-green-300' : 'bg-gray-200 text-gray-600 dark:bg-dark-600 dark:text-gray-300']">
            {{ status?.running ? t('admin.codexAttestationCollector.running') : t('admin.codexAttestationCollector.stopped') }}
          </span>
        </div>
        <p class="mt-1 text-xs text-gray-600 dark:text-gray-400">{{ t('admin.codexAttestationCollector.description') }}</p>
        <div class="mt-2 flex flex-wrap items-center gap-2 text-[11px] text-gray-600 dark:text-gray-400">
          <span class="rounded-full bg-white px-2 py-0.5 font-medium text-amber-700 ring-1 ring-amber-200 dark:bg-dark-900/60 dark:text-amber-300 dark:ring-amber-800">
            {{ t('admin.codexAttestationCollector.scopeTitle') }}
          </span>
          <span>{{ t('admin.codexAttestationCollector.scopeAttestation') }}</span>
          <span class="text-gray-400">·</span>
          <span>{{ t('admin.codexAttestationCollector.scopeIdentity') }}</span>
          <span class="text-gray-400">·</span>
          <span>{{ t('admin.codexAttestationCollector.scopeTransport') }}</span>
        </div>
      </div>
      <div class="flex flex-wrap gap-2">
        <button type="button" class="btn btn-secondary btn-sm" :disabled="loading" @click="loadStatus">
          <Icon name="refresh" size="sm" :class="['mr-1', loading ? 'animate-spin' : '']" />
          {{ t('common.refresh') }}
        </button>
        <button type="button" :class="['btn btn-sm', status?.running ? 'btn-secondary' : 'btn-primary']" :disabled="actionLoading" @click="toggle">
          <Icon :name="actionLoading ? 'refresh' : (status?.running ? 'x' : 'play')" size="sm" :class="['mr-1', actionLoading ? 'animate-spin' : '']" />
          {{ status?.running ? t('admin.codexAttestationCollector.stop') : t('admin.codexAttestationCollector.start') }}
        </button>
      </div>
    </div>

    <div class="mt-3 grid gap-3 text-xs sm:grid-cols-3">
      <div><div class="text-gray-500 dark:text-gray-400">{{ t('admin.codexAttestationCollector.ttl') }}</div><div class="mt-1 font-mono">{{ status?.session_ttl_seconds ?? '—' }}s</div></div>
      <div><div class="text-gray-500 dark:text-gray-400">{{ t('admin.codexAttestationCollector.maxRecords') }}</div><div class="mt-1 font-mono">{{ status?.max_records_per_session ?? '—' }}</div></div>
      <div><div class="text-gray-500 dark:text-gray-400">{{ t('admin.codexAttestationCollector.activeSessions') }}</div><div class="mt-1 font-mono">{{ status?.active_sessions ?? 0 }}</div></div>
    </div>

    <div v-if="status?.running" class="mt-4 space-y-3">
      <div class="flex flex-wrap gap-2">
        <button type="button" class="btn btn-primary btn-sm" :disabled="sessionLoading" @click="createSession">
          <Icon name="plus" size="sm" class="mr-1" />{{ t('admin.codexAttestationCollector.createSession') }}
        </button>
        <button v-if="session" type="button" class="btn btn-secondary btn-sm" :disabled="capturesLoading" @click="loadCaptures">
          <Icon name="refresh" size="sm" :class="['mr-1', capturesLoading ? 'animate-spin' : '']" />{{ t('admin.codexAttestationCollector.refresh') }}
        </button>
      </div>

      <div v-if="session" class="rounded-md border border-amber-200 bg-white p-3 text-xs dark:border-amber-800 dark:bg-dark-900/60">
        <div class="grid gap-3 sm:grid-cols-2">
          <div><div class="text-gray-500 dark:text-gray-400">{{ t('admin.codexAttestationCollector.token') }}</div><div class="mt-1 break-all font-mono">{{ session.token }}</div></div>
          <div><div class="text-gray-500 dark:text-gray-400">{{ t('admin.codexAttestationCollector.expiresAt') }}</div><div class="mt-1">{{ formatDateTime(session.expires_at) }}</div></div>
        </div>
        <div class="mt-3"><div class="text-gray-500 dark:text-gray-400">{{ t('admin.codexAttestationCollector.bridgeQuery') }}</div><div class="mt-1 break-all rounded bg-gray-900 p-2 font-mono text-gray-100">{{ session.bridge_query }}</div></div>
        <div class="mt-2"><div class="text-gray-500 dark:text-gray-400">{{ t('admin.codexAttestationCollector.bridgeEndpoint') }}</div><div class="mt-1 break-all rounded bg-gray-900 p-2 font-mono text-gray-100">{{ bridgeEndpoint }}</div></div>
        <div class="mt-2"><div class="text-gray-500 dark:text-gray-400">{{ t('admin.codexAttestationCollector.headerHint') }}</div><div class="mt-1 break-all font-mono">{{ session.header_name }}: {{ session.token }}</div></div>
        <div class="mt-3 flex flex-wrap gap-2">
          <button type="button" class="btn btn-secondary btn-xs" @click="copyText(bridgeEndpoint)"><Icon name="copy" size="xs" class="mr-1" />{{ t('admin.codexAttestationCollector.copyEndpoint') }}</button>
          <button type="button" class="btn btn-secondary btn-xs" @click="copyText(`${session.header_name}: ${session.token}`)"><Icon name="copy" size="xs" class="mr-1" />{{ t('common.copy') }}</button>
          <button type="button" class="btn btn-secondary btn-xs" :disabled="sessionDeleteLoading" @click="deleteSession"><Icon :name="sessionDeleteLoading ? 'refresh' : 'trash'" size="xs" :class="['mr-1', sessionDeleteLoading ? 'animate-spin' : '']" />{{ t('admin.codexAttestationCollector.deleteSession') }}</button>
        </div>
      </div>

      <div v-if="session" class="space-y-2">
        <div class="flex items-center justify-between"><h5 class="text-xs font-medium uppercase text-gray-500 dark:text-gray-400">{{ t('admin.codexAttestationCollector.captures') }}</h5><span class="text-xs text-gray-500 dark:text-gray-400">{{ captures.length }}</span></div>
        <div v-if="captures.length === 0" class="rounded-md border border-dashed border-gray-300 px-3 py-4 text-center text-xs text-gray-500 dark:border-dark-600">{{ t('admin.codexAttestationCollector.noCaptures') }}</div>
        <div v-for="record in captures" :key="record.id" class="rounded-md border border-gray-200 bg-white p-3 text-xs dark:border-dark-700 dark:bg-dark-900/60">
          <div class="flex flex-wrap items-center gap-2"><span class="font-medium">{{ record.event }}</span><span class="rounded bg-gray-100 px-1.5 py-0.5 font-mono dark:bg-dark-700">{{ record.status }}</span><span class="text-gray-500 dark:text-gray-400">{{ formatDateTime(record.captured_at) }}</span></div>
          <div class="mt-1 grid gap-1 text-gray-600 dark:text-gray-400 sm:grid-cols-2"><span>{{ t('admin.codexAttestationCollector.client') }}: {{ record.client_name || '—' }} {{ record.client_version || '' }}</span><span>{{ t('admin.codexAttestationCollector.connection') }}: {{ record.connection_id }}</span><span>{{ t('admin.codexAttestationCollector.session') }}: {{ record.session_id || '—' }}</span><span>JSON-RPC: {{ record.jsonrpc_version || '—' }} · {{ record.generate_response_id || record.initialize_id || '—' }}</span><span>Capabilities: {{ record.capability_keys?.join(', ') || '—' }}</span><span>Handshake: {{ record.handshake_protocol || '—' }} / {{ record.handshake_transport || '—' }}</span><span class="break-all">UA: {{ record.handshake_user_agent || '—' }}</span><span class="break-all">Originator: {{ record.handshake_originator || '—' }}</span><span>{{ t('admin.codexAttestationCollector.proof') }}: {{ record.proof_length ?? 0 }} bytes / {{ record.proof_sha256 || '—' }}</span><span class="break-all">Frame SHA-256: {{ record.frame_sha256 || '—' }}</span></div>
        </div>
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { adminAPI } from '@/api/admin'
import { buildGatewayUrl } from '@/api/client'
import type { CodexAttestationCaptureRecord, CodexAttestationCollectorSession, CodexAttestationCollectorStatus } from '@/api/admin/codexAttestationCollector'
import Icon from '@/components/icons/Icon.vue'

const props = defineProps<{ show: boolean }>()
const { t } = useI18n()
const appStore = useAppStore()
const status = ref<CodexAttestationCollectorStatus | null>(null)
const session = ref<CodexAttestationCollectorSession | null>(null)
const captures = ref<CodexAttestationCaptureRecord[]>([])
const loading = ref(false)
const actionLoading = ref(false)
const sessionLoading = ref(false)
const capturesLoading = ref(false)
const sessionDeleteLoading = ref(false)
let pollTimer: number | null = null

const bridgeEndpoint = computed(() => {
  if (!session.value) return ''
  const separator = session.value.bridge_query ? '?' : ''
  return `${buildGatewayUrl('/backend-api/codex/app-server')}${separator}${session.value.bridge_query}`
})

watch(() => props.show, (visible) => {
  if (visible) loadStatus()
  else stopPolling()
}, { immediate: true })

onUnmounted(stopPolling)

async function loadStatus() {
  loading.value = true
  try { status.value = await adminAPI.codexAttestationCollector.status() }
  catch (error) { appStore.showError(t('admin.codexAttestationCollector.statusFailed')); console.error(error) }
  finally { loading.value = false }
}

async function toggle() {
  actionLoading.value = true
  try {
    if (status.value?.running) { status.value = await adminAPI.codexAttestationCollector.stop(); session.value = null; captures.value = []; stopPolling() }
    else status.value = await adminAPI.codexAttestationCollector.start()
  } catch (error: any) { appStore.showError(error?.message || t('admin.codexAttestationCollector.actionFailed')); console.error(error) }
  finally { actionLoading.value = false }
}

async function createSession() {
  sessionLoading.value = true
  try { session.value = await adminAPI.codexAttestationCollector.createSession(); captures.value = []; startPolling() }
  catch (error: any) { appStore.showError(error?.message || t('admin.codexAttestationCollector.sessionFailed')); console.error(error) }
  finally { sessionLoading.value = false }
}

async function deleteSession() {
  if (!session.value || sessionDeleteLoading.value) return
  sessionDeleteLoading.value = true
  try {
    await adminAPI.codexAttestationCollector.deleteSession(session.value.token)
    session.value = null
    captures.value = []
    stopPolling()
    appStore.showSuccess(t('admin.codexAttestationCollector.sessionDeleted'))
  } catch (error) {
    appStore.showError(t('admin.codexAttestationCollector.deleteSessionFailed'))
    console.error(error)
  } finally {
    sessionDeleteLoading.value = false
  }
}

async function loadCaptures() {
  if (!session.value) return
  capturesLoading.value = true
  try { captures.value = await adminAPI.codexAttestationCollector.listCaptures(session.value.token) }
  catch (error) { console.error(error) }
  finally { capturesLoading.value = false }
}

function startPolling() { stopPolling(); pollTimer = window.setInterval(loadCaptures, 2500); loadCaptures() }
function stopPolling() { if (pollTimer !== null) { window.clearInterval(pollTimer); pollTimer = null } }
function formatDateTime(value: string) { return new Date(value).toLocaleString() }
async function copyText(value: string) { try { await navigator.clipboard.writeText(value); appStore.showSuccess(t('admin.codexAttestationCollector.copied')) } catch (error) { appStore.showError(t('admin.codexAttestationCollector.copyFailed')); console.error(error) } }
</script>
