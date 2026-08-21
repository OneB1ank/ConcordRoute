<template>
  <AppLayout>
    <div class="space-y-5">
      <div class="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 class="text-xl font-semibold text-gray-900 dark:text-white">
            {{ t('admin.clashProxy.title') }}
          </h1>
          <p class="mt-1 text-sm text-gray-500 dark:text-dark-400">
            {{ t('admin.clashProxy.description') }}
          </p>
        </div>
        <div class="flex gap-2">
          <RouterLink class="btn btn-secondary" to="/admin/proxies">
            {{ t('admin.clashProxy.standardProxy') }}
          </RouterLink>
          <button class="btn btn-secondary" :disabled="loading" @click="loadAll">
            <Icon name="refresh" size="sm" :class="{ 'animate-spin': loading }" />
            {{ t('common.refresh') }}
          </button>
        </div>
      </div>

      <div
        v-if="error"
        class="rounded-lg border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700 dark:border-red-800/50 dark:bg-red-900/20 dark:text-red-200"
      >
        {{ error }}
      </div>

      <div class="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
        <div
          v-for="card in runtimeCards"
          :key="card.label"
          class="rounded-lg border border-gray-200 bg-white px-4 py-3 dark:border-dark-700 dark:bg-dark-800"
        >
          <div class="text-xs uppercase tracking-wide text-gray-500 dark:text-dark-400">
            {{ card.label }}
          </div>
          <div class="mt-1 text-2xl font-semibold text-gray-900 dark:text-white">
            {{ card.value }}
          </div>
        </div>
      </div>

      <div class="flex flex-wrap gap-2 border-b border-gray-200 dark:border-dark-700">
        <button
          v-for="tab in tabs"
          :key="tab.key"
          class="px-3 py-2 text-sm font-medium"
          :class="activeTab === tab.key
            ? 'border-b-2 border-primary-500 text-primary-600 dark:text-primary-300'
            : 'text-gray-500 hover:text-gray-900 dark:text-dark-300 dark:hover:text-white'"
          @click="activeTab = tab.key"
        >
          {{ tab.label }}
        </button>
      </div>

      <section v-if="activeTab === 'nodes'" class="grid gap-4 xl:grid-cols-[380px_minmax(0,1fr)]">
        <div class="space-y-4">
          <div class="rounded-lg border border-gray-200 bg-white p-4 dark:border-dark-700 dark:bg-dark-800">
            <h2 class="font-semibold text-gray-900 dark:text-white">
              {{ t('admin.clashProxy.addNode') }}
            </h2>
            <form class="mt-4 space-y-3" @submit.prevent="createNode">
              <input v-model="nodeForm.name" class="input" :placeholder="t('admin.clashProxy.nodeName')" />
              <input
                v-model="nodeForm.url"
                class="input"
                :placeholder="t('admin.clashProxy.nodeURLPlaceholder')"
              />
              <button class="btn btn-primary w-full" :disabled="submitting || !nodeForm.url.trim()">
                {{ t('admin.clashProxy.addNode') }}
              </button>
            </form>
          </div>

          <div class="rounded-lg border border-gray-200 bg-white p-4 dark:border-dark-700 dark:bg-dark-800">
            <h2 class="font-semibold text-gray-900 dark:text-white">
              {{ t('admin.clashProxy.importSubscription') }}
            </h2>
            <form class="mt-4 space-y-3" @submit.prevent="importNodes">
              <Select v-model="importForm.format" :options="importFormatOptions" />
              <input
                v-model="importForm.url"
                class="input"
                :placeholder="t('admin.clashProxy.subscriptionURL')"
              />
              <textarea
                v-model="importForm.content"
                class="input min-h-[130px]"
                :placeholder="t('admin.clashProxy.importContent')"
              />
              <button
                class="btn btn-secondary w-full"
                :disabled="submitting || (!importForm.url.trim() && !importForm.content.trim())"
              >
                {{ t('admin.clashProxy.import') }}
              </button>
            </form>
          </div>
        </div>

        <div class="overflow-hidden rounded-lg border border-gray-200 bg-white dark:border-dark-700 dark:bg-dark-800">
          <div class="overflow-x-auto">
            <table class="min-w-full divide-y divide-gray-100 text-sm dark:divide-dark-700">
              <thead class="bg-gray-50 text-left text-xs uppercase text-gray-500 dark:bg-dark-900 dark:text-dark-400">
                <tr>
                  <th class="px-4 py-3">{{ t('admin.clashProxy.name') }}</th>
                  <th class="px-4 py-3">{{ t('admin.clashProxy.type') }}</th>
                  <th class="px-4 py-3">{{ t('admin.clashProxy.source') }}</th>
                  <th class="px-4 py-3">{{ t('admin.clashProxy.server') }}</th>
                  <th class="px-4 py-3"></th>
                </tr>
              </thead>
              <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
                <tr v-for="node in nodes" :key="node.id">
                  <td class="px-4 py-3 font-medium text-gray-900 dark:text-white">{{ node.name }}</td>
                  <td class="px-4 py-3 text-gray-600 dark:text-dark-300">{{ node.node_type }}</td>
                  <td class="px-4 py-3 text-gray-600 dark:text-dark-300">{{ node.source_type }}</td>
                  <td class="px-4 py-3 text-gray-600 dark:text-dark-300">
                    {{ node.config.server || '-' }}:{{ node.config.port || '-' }}
                  </td>
                  <td class="px-4 py-3 text-right">
                    <button class="text-red-600 hover:underline dark:text-red-300" @click="deleteNode(node.id)">
                      {{ t('common.delete') }}
                    </button>
                  </td>
                </tr>
                <tr v-if="nodes.length === 0">
                  <td colspan="5" class="px-4 py-10 text-center text-gray-500">
                    {{ t('admin.clashProxy.noNodes') }}
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </section>

      <section v-else-if="activeTab === 'profiles'" class="grid gap-4 xl:grid-cols-[380px_minmax(0,1fr)]">
        <div class="rounded-lg border border-gray-200 bg-white p-4 dark:border-dark-700 dark:bg-dark-800">
          <div class="flex items-center justify-between gap-2">
            <h2 class="font-semibold text-gray-900 dark:text-white">
              {{ editingProfileID ? t('admin.clashProxy.editProfile') : t('admin.clashProxy.createProfile') }}
            </h2>
            <button v-if="editingProfileID" class="text-sm text-gray-500 hover:underline" @click="resetProfileForm">
              {{ t('common.cancel') }}
            </button>
          </div>
          <form class="mt-4 space-y-3" @submit.prevent="saveProfile">
            <input v-model="profileForm.name" class="input" :placeholder="t('admin.clashProxy.profileName')" />
            <Select v-model="profileForm.strategy" :options="strategyOptions" />
            <input v-model="profileForm.test_url" class="input" :placeholder="defaultTestURL" />
            <input
              v-model.number="profileForm.interval_seconds"
              class="input"
              type="number"
              min="1"
              :placeholder="t('admin.clashProxy.intervalSeconds')"
            />
            <label
              class="flex cursor-pointer items-start gap-3 rounded-lg border border-gray-200 p-3 dark:border-dark-600"
            >
              <input v-model="profileForm.auto_start" type="checkbox" class="mt-0.5 h-4 w-4 rounded" />
              <span>
                <span class="block text-sm font-medium text-gray-800 dark:text-dark-100">
                  {{ t('admin.clashProxy.autoStart') }}
                </span>
                <span class="mt-0.5 block text-xs text-gray-500 dark:text-dark-400">
                  {{ t('admin.clashProxy.autoStartHint') }}
                </span>
              </span>
            </label>
            <div class="max-h-64 space-y-2 overflow-y-auto rounded-lg border border-gray-200 p-3 dark:border-dark-600">
              <label
                v-for="node in nodes"
                :key="node.id"
                class="flex cursor-pointer items-center gap-2 text-sm text-gray-700 dark:text-dark-200"
              >
                <input v-model="selectedNodeIDs" type="checkbox" :value="node.id" class="h-4 w-4 rounded" />
                <span>{{ node.name }} · {{ node.node_type }}</span>
              </label>
              <div v-if="nodes.length === 0" class="py-4 text-center text-sm text-gray-500">
                {{ t('admin.clashProxy.createNodeFirst') }}
              </div>
            </div>
            <button
              class="btn btn-primary w-full"
              :disabled="submitting || !profileForm.name.trim() || selectedNodeIDs.length === 0"
            >
              {{ editingProfileID ? t('common.save') : t('admin.clashProxy.createProfile') }}
            </button>
          </form>
        </div>

        <div class="space-y-3">
          <div
            v-for="profile in profiles"
            :key="profile.id"
            class="rounded-lg border border-gray-200 bg-white p-4 dark:border-dark-700 dark:bg-dark-800"
          >
            <div class="flex flex-wrap items-start justify-between gap-3">
              <div>
                <h3 class="font-semibold text-gray-900 dark:text-white">{{ profile.name }}</h3>
                <div class="mt-1 text-sm text-gray-500 dark:text-dark-400">
                  #{{ profile.id }} · {{ profile.strategy }} · {{ runtimeOf(profile.id).status }} ·
                  {{ profile.auto_start ? t('admin.clashProxy.autoStartEnabled') : t('admin.clashProxy.autoStartDisabled') }} ·
                  {{ t('admin.clashProxy.boundAccounts', { count: profileBindingCount(profile.id) }) }}
                </div>
                <div v-if="runtimeOf(profile.id).proxy_url" class="mt-1 font-mono text-xs text-gray-500">
                  {{ runtimeOf(profile.id).proxy_url }}
                </div>
              </div>
              <div class="flex flex-wrap gap-2">
                <button class="btn btn-secondary px-3 py-1.5" @click="editProfile(profile)">
                  {{ t('common.edit') }}
                </button>
                <button
                  class="btn btn-secondary px-3 py-1.5"
                  :disabled="busyProfileID === profile.id"
                  @click="runProfileAction(profile.id, 'start')"
                >
                  {{ t('admin.clashProxy.start') }}
                </button>
                <button
                  class="btn btn-secondary px-3 py-1.5"
                  :disabled="busyProfileID === profile.id"
                  @click="runProfileAction(profile.id, 'stop')"
                >
                  {{ t('admin.clashProxy.stop') }}
                </button>
                <button
                  class="btn btn-secondary px-3 py-1.5"
                  :disabled="busyProfileID === profile.id"
                  @click="runProfileAction(profile.id, 'restart')"
                >
                  {{ t('admin.clashProxy.restart') }}
                </button>
                <button
                  class="btn btn-secondary px-3 py-1.5"
                  :disabled="busyProfileID === profile.id"
                  @click="runProfileAction(profile.id, 'test')"
                >
                  {{ t('admin.clashProxy.test') }}
                </button>
                <button
                  class="btn btn-primary px-3 py-1.5"
                  :disabled="busyProfileID === profile.id || runtimeOf(profile.id).status !== 'running'"
                  @click="bindUnboundOpenAIOAuthAccounts(profile.id)"
                >
                  {{ t('admin.clashProxy.bindUnboundOpenAI') }}
                </button>
              </div>
            </div>
            <div
              v-if="runtimeOf(profile.id).status === 'running' && profileBindingCount(profile.id) === 0"
              class="mt-3 rounded-md bg-amber-50 px-3 py-2 text-sm text-amber-800 dark:bg-amber-950/40 dark:text-amber-200"
            >
              {{ t('admin.clashProxy.noEffectiveBindingsWarning') }}
            </div>
            <div
              v-if="profileResults[profile.id]"
              class="mt-3 rounded-md bg-gray-50 px-3 py-2 text-sm text-gray-700 dark:bg-dark-900 dark:text-dark-200"
            >
              {{ profileResults[profile.id] }}
            </div>
            <div v-if="runtimeOf(profile.id).last_error" class="mt-2 text-sm text-red-600 dark:text-red-300">
              {{ runtimeOf(profile.id).last_error }}
            </div>
          </div>
          <div
            v-if="profiles.length === 0"
            class="rounded-lg border border-gray-200 bg-white p-10 text-center text-gray-500 dark:border-dark-700 dark:bg-dark-800"
          >
            {{ t('admin.clashProxy.noProfiles') }}
          </div>
        </div>
      </section>

      <section v-else class="grid gap-4 xl:grid-cols-[380px_minmax(0,1fr)]">
        <div class="rounded-lg border border-gray-200 bg-white p-4 dark:border-dark-700 dark:bg-dark-800">
          <h2 class="font-semibold text-gray-900 dark:text-white">
            {{ t('admin.clashProxy.bindAccount') }}
          </h2>
          <p class="mt-1 text-sm text-gray-500 dark:text-dark-400">
            {{ t('admin.clashProxy.bindingHint') }}
          </p>
          <form class="mt-4 space-y-3" @submit.prevent="createBinding">
            <Select
              v-model="bindingForm.account_id"
              :options="accountOptions"
              searchable
              :placeholder="t('admin.clashProxy.selectAccount')"
            />
            <Select
              v-model="bindingForm.profile_id"
              :options="profileOptions"
              :placeholder="t('admin.clashProxy.selectProfile')"
            />
            <button
              class="btn btn-primary w-full"
              :disabled="submitting || bindingForm.account_id <= 0 || bindingForm.profile_id <= 0"
            >
              {{ t('admin.clashProxy.bindAccount') }}
            </button>
          </form>
        </div>

        <div class="overflow-hidden rounded-lg border border-gray-200 bg-white dark:border-dark-700 dark:bg-dark-800">
          <div class="overflow-x-auto">
            <table class="min-w-full divide-y divide-gray-100 text-sm dark:divide-dark-700">
              <thead class="bg-gray-50 text-left text-xs uppercase text-gray-500 dark:bg-dark-900 dark:text-dark-400">
                <tr>
                  <th class="px-4 py-3">{{ t('admin.clashProxy.account') }}</th>
                  <th class="px-4 py-3">{{ t('admin.clashProxy.platform') }}</th>
                  <th class="px-4 py-3">{{ t('admin.clashProxy.profile') }}</th>
                  <th class="px-4 py-3">{{ t('admin.clashProxy.previousProxy') }}</th>
                  <th class="px-4 py-3"></th>
                </tr>
              </thead>
              <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
                <tr v-for="binding in bindings" :key="binding.id">
                  <td class="px-4 py-3 font-medium text-gray-900 dark:text-white">
                    {{ binding.account_name }} (#{{ binding.account_id }})
                  </td>
                  <td class="px-4 py-3 text-gray-600 dark:text-dark-300">{{ binding.account_platform }}</td>
                  <td class="px-4 py-3 text-gray-600 dark:text-dark-300">{{ binding.profile_name }}</td>
                  <td class="px-4 py-3 text-gray-600 dark:text-dark-300">
                    {{ binding.previous_proxy_id ? `#${binding.previous_proxy_id}` : t('admin.clashProxy.direct') }}
                  </td>
                  <td class="px-4 py-3 text-right">
                    <button class="text-red-600 hover:underline dark:text-red-300" @click="deleteBinding(binding.id)">
                      {{ t('admin.clashProxy.unbind') }}
                    </button>
                  </td>
                </tr>
                <tr v-if="bindings.length === 0">
                  <td colspan="5" class="px-4 py-10 text-center text-gray-500">
                    {{ t('admin.clashProxy.noBindings') }}
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </section>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { RouterLink } from 'vue-router'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import Icon from '@/components/icons/Icon.vue'
import Select from '@/components/common/Select.vue'
import { adminAPI } from '@/api/admin'
import clashProxyAPI, {
  type ClashProxyBinding,
  type ClashProxyNode,
  type ClashProxyProfile,
  type ClashProxyRuntime,
  type ClashProxyRuntimeStatus
} from '@/api/admin/clashProxy'
import type { Account } from '@/types'
import { useAppStore } from '@/stores/app'

const { t } = useI18n()
const appStore = useAppStore()
const defaultTestURL = 'https://www.gstatic.com/generate_204'
const loading = ref(false)
const submitting = ref(false)
const error = ref('')
const activeTab = ref<'nodes' | 'profiles' | 'bindings'>('nodes')
const nodes = ref<ClashProxyNode[]>([])
const profiles = ref<ClashProxyProfile[]>([])
const bindings = ref<ClashProxyBinding[]>([])
const accounts = ref<Account[]>([])
const runtimes = reactive<Record<number, ClashProxyRuntime>>({})
const runtimeStatus = ref<ClashProxyRuntimeStatus>({ total: 0, starting: 0, running: 0, failed: 0, stopped: 0 })
const profileResults = reactive<Record<number, string>>({})
const busyProfileID = ref<number | null>(null)
const editingProfileID = ref<number | null>(null)
const selectedNodeIDs = ref<number[]>([])

const nodeForm = reactive({ name: '', url: '' })
const importForm = reactive({ format: 'auto', url: '', content: '' })
const profileForm = reactive({
  name: '',
  strategy: 'select',
  test_url: defaultTestURL,
  interval_seconds: 300,
  auto_start: true
})
const bindingForm = reactive({ account_id: 0, profile_id: 0 })

const tabs = computed(() => [
  { key: 'nodes', label: t('admin.clashProxy.nodes') },
  { key: 'profiles', label: t('admin.clashProxy.profiles') },
  { key: 'bindings', label: t('admin.clashProxy.bindings') }
] as const)

const runtimeCards = computed(() => [
  { label: t('admin.clashProxy.running'), value: runtimeStatus.value.running },
  { label: t('admin.clashProxy.starting'), value: runtimeStatus.value.starting },
  { label: t('admin.clashProxy.failed'), value: runtimeStatus.value.failed },
  { label: t('admin.clashProxy.stopped'), value: runtimeStatus.value.stopped }
])

const importFormatOptions = computed(() => [
  { value: 'auto', label: t('admin.clashProxy.formatAuto') },
  { value: 'clash_yaml', label: 'Clash YAML' },
  { value: 'uri', label: 'URI' }
])

const strategyOptions = computed(() => [
  { value: 'select', label: 'select' },
  { value: 'url_test', label: 'url-test' },
  { value: 'fallback', label: 'fallback' },
  { value: 'load_balance', label: 'load-balance' }
])

const accountOptions = computed(() => accounts.value.map((account) => ({
  value: account.id,
  label: `${account.name} · ${account.platform} · #${account.id}`
})))

const profileOptions = computed(() => profiles.value.map((profile) => ({
  value: profile.id,
  label: `${profile.name} · ${runtimeOf(profile.id).status}`,
  disabled: runtimeOf(profile.id).status !== 'running'
})))

function profileBindingCount(profileID: number): number {
  return bindings.value.filter((binding) => binding.profile_id === profileID && binding.enabled).length
}

function messageOf(err: unknown): string {
  const value = err as { message?: string; response?: { data?: { message?: string } } }
  return value.message || value.response?.data?.message || t('admin.clashProxy.operationFailed')
}

function runtimeOf(profileID: number): ClashProxyRuntime {
  return runtimes[profileID] || { profile_id: profileID, runtime_type: 'mihomo', status: 'stopped' }
}

async function loadAll() {
  loading.value = true
  error.value = ''
  try {
    const [nodeItems, profileItems, bindingItems, status, accountPage] = await Promise.all([
      clashProxyAPI.listNodes(),
      clashProxyAPI.listProfiles(),
      clashProxyAPI.listBindings(),
      clashProxyAPI.getRuntimeStatus(),
      adminAPI.accounts.list(1, 200, { lite: 'true', sort_by: 'id', sort_order: 'asc' })
    ])
    nodes.value = nodeItems
    profiles.value = profileItems
    bindings.value = bindingItems
    runtimeStatus.value = status
    accounts.value = accountPage.items || []
    const runtimeItems = await Promise.all(profileItems.map((profile) => clashProxyAPI.getProfileRuntime(profile.id)))
    for (const key of Object.keys(runtimes)) delete runtimes[Number(key)]
    for (const runtime of runtimeItems) runtimes[runtime.profile_id] = runtime
  } catch (err) {
    error.value = messageOf(err)
  } finally {
    loading.value = false
  }
}

async function run(action: () => Promise<void>, successMessage: string) {
  submitting.value = true
  error.value = ''
  try {
    await action()
    appStore.showSuccess(successMessage)
    await loadAll()
  } catch (err) {
    error.value = messageOf(err)
    appStore.showError(error.value)
  } finally {
    submitting.value = false
  }
}

function createNode() {
  return run(async () => {
    await clashProxyAPI.createNode({ name: nodeForm.name.trim(), url: nodeForm.url.trim() })
    nodeForm.name = ''
    nodeForm.url = ''
  }, t('admin.clashProxy.nodeCreated'))
}

function importNodes() {
  return run(async () => {
    await clashProxyAPI.importNodes({
      format: importForm.format,
      url: importForm.url.trim() || undefined,
      content: importForm.content.trim() || undefined
    })
    importForm.url = ''
    importForm.content = ''
  }, t('admin.clashProxy.nodesImported'))
}

function deleteNode(id: number) {
  return run(() => clashProxyAPI.deleteNode(id), t('admin.clashProxy.nodeDeleted'))
}

function editProfile(profile: ClashProxyProfile) {
  editingProfileID.value = profile.id
  profileForm.name = profile.name
  profileForm.strategy = profile.strategy
  profileForm.test_url = profile.test_url
  profileForm.interval_seconds = profile.interval_seconds
  profileForm.auto_start = profile.auto_start
  selectedNodeIDs.value = [...profile.node_ids]
}

function resetProfileForm() {
  editingProfileID.value = null
  profileForm.name = ''
  profileForm.strategy = 'select'
  profileForm.test_url = defaultTestURL
  profileForm.interval_seconds = 300
  profileForm.auto_start = true
  selectedNodeIDs.value = []
}

function saveProfile() {
  const input = {
    name: profileForm.name.trim(),
    strategy: profileForm.strategy,
    test_url: profileForm.test_url.trim(),
    interval_seconds: profileForm.interval_seconds,
    auto_start: profileForm.auto_start,
    node_ids: [...selectedNodeIDs.value]
  }
  return run(async () => {
    if (editingProfileID.value) {
      await clashProxyAPI.updateProfile(editingProfileID.value, input)
    } else {
      await clashProxyAPI.createProfile(input)
    }
    resetProfileForm()
  }, t('admin.clashProxy.profileSaved'))
}

async function runProfileAction(profileID: number, action: 'start' | 'stop' | 'restart' | 'test') {
  busyProfileID.value = profileID
  error.value = ''
  try {
    if (action === 'test') {
      const result = await clashProxyAPI.testProfile(profileID)
      profileResults[profileID] = result.healthy
        ? `${t('admin.clashProxy.healthy')}: ${result.version || result.proxy_url || result.status}`
        : `${t('admin.clashProxy.failed')}: ${result.error || result.status}`
    } else {
      const runtime = action === 'start'
        ? await clashProxyAPI.startProfile(profileID)
        : action === 'stop'
          ? await clashProxyAPI.stopProfile(profileID)
          : await clashProxyAPI.restartProfile(profileID)
      profileResults[profileID] = `${runtime.status}${runtime.proxy_url ? ` · ${runtime.proxy_url}` : ''}`
    }
    await loadAll()
  } catch (err) {
    error.value = messageOf(err)
    appStore.showError(error.value)
  } finally {
    busyProfileID.value = null
  }
}

async function bindUnboundOpenAIOAuthAccounts(profileID: number) {
  if (!window.confirm(t('admin.clashProxy.bulkBindConfirm'))) return
  busyProfileID.value = profileID
  error.value = ''
  try {
    const result = await clashProxyAPI.bindUnboundOpenAIOAuthAccounts(profileID)
    appStore.showSuccess(t('admin.clashProxy.bulkBindCompleted', {
      bound: result.bound,
      eligible: result.eligible,
      failed: result.failed
    }))
    await loadAll()
  } catch (err) {
    error.value = messageOf(err)
    appStore.showError(error.value)
  } finally {
    busyProfileID.value = null
  }
}

function createBinding() {
  return run(async () => {
    await clashProxyAPI.createBinding({
      account_id: bindingForm.account_id,
      profile_id: bindingForm.profile_id
    })
    bindingForm.account_id = 0
  }, t('admin.clashProxy.accountBound'))
}

function deleteBinding(id: number) {
  return run(() => clashProxyAPI.deleteBinding(id), t('admin.clashProxy.accountUnbound'))
}

onMounted(loadAll)
</script>
