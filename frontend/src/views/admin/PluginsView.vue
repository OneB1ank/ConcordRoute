<template>
  <AppLayout>
    <div class="space-y-6">
      <section
        class="flex flex-col gap-4 border-b border-gray-200 pb-5 dark:border-dark-700 sm:flex-row sm:items-end sm:justify-between"
      >
        <div class="min-w-0">
          <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
            {{ t("admin.plugins.title") }}
          </h2>
          <p class="mt-1 max-w-3xl text-sm text-gray-500 dark:text-gray-400">
            {{ t("admin.plugins.description") }}
          </p>
          <div
            class="mt-3 flex flex-wrap gap-2 text-xs text-gray-600 dark:text-gray-300"
          >
            <span class="rounded bg-gray-100 px-2 py-1 dark:bg-dark-700">{{
              t("admin.plugins.onlyOpenAI")
            }}</span>
            <span class="rounded bg-gray-100 px-2 py-1 dark:bg-dark-700">{{
              t("admin.plugins.noAccountCoupling")
            }}</span>
          </div>
        </div>

        <div class="flex flex-shrink-0 items-center gap-2">
          <input
            ref="fileInput"
            class="hidden"
            type="file"
            accept=".s2plugin,application/zip"
            @change="handleFileSelected"
          />
          <input ref="upgradeFileInput" class="hidden" type="file" accept=".s2plugin,application/zip" @change="handleUpgradeFile" />
          <button
            type="button"
            class="btn btn-primary"
            :disabled="uploading"
            @click="fileInput?.click()"
          >
            <Icon name="upload" size="sm" />
            {{ uploading ? t("common.processing") : t("admin.plugins.upload") }}
          </button>
          <button
            type="button"
            class="btn btn-secondary"
            :disabled="loading"
            :title="t('common.refresh')"
            @click="loadPlugins"
          >
            <Icon name="refresh" size="sm" />
            <span class="sr-only">{{ t("common.refresh") }}</span>
          </button>
        </div>
      </section>

      <p class="text-xs text-gray-500 dark:text-gray-400">
        {{ t("admin.plugins.uploadHint") }}
      </p>

      <details class="text-xs text-gray-500 dark:text-gray-400">
        <summary class="w-fit cursor-pointer font-medium">{{ t("admin.plugins.managementHelp") }}</summary>
        <p class="mt-2">{{ t("admin.plugins.runtimeNotice") }}</p>
        <p class="mt-1">{{ t("admin.plugins.menuNotice") }}</p>
      </details>

      <div v-if="plugins.length" class="flex flex-wrap items-center justify-between gap-3">
        <p class="text-sm text-gray-500" role="status">{{ t("admin.plugins.pluginCount", { shown: filteredPlugins.length, total: plugins.length }) }}</p>
        <div class="flex flex-wrap gap-2">
          <input v-model="searchQuery" type="search" class="input w-60 max-w-full" :placeholder="t('admin.plugins.search')" :aria-label="t('admin.plugins.search')" />
          <div class="flex flex-wrap gap-1" role="group" :aria-label="t('admin.plugins.filterState')">
            <button
              v-for="state in ['all', 'enabled', 'disabled', 'starting', 'error', 'incompatible']"
              :key="state"
              type="button"
              class="btn btn-secondary btn-sm"
              :class="{ 'ring-2 ring-primary-500': stateFilter === state }"
              :aria-pressed="stateFilter === state"
              @click="stateFilter = state"
            >{{ state === 'all' ? t('admin.plugins.allStates') : t(`admin.plugins.${state}`) }}</button>
          </div>
        </div>
      </div>

      <div
        v-if="loading"
        class="flex min-h-48 items-center justify-center text-sm text-gray-500"
      >
        {{ t("common.loading") }}
      </div>

      <div
        v-else-if="plugins.length === 0"
        class="flex min-h-56 flex-col items-center justify-center border border-dashed border-gray-300 px-6 text-center dark:border-dark-600"
      >
        <Icon name="cube" size="xl" class="text-gray-400" />
        <p class="mt-3 font-medium text-gray-800 dark:text-gray-200">
          {{ t("admin.plugins.empty") }}
        </p>
        <p class="mt-1 max-w-lg text-sm text-gray-500 dark:text-gray-400">
          {{ t("admin.plugins.emptyHint") }}
        </p>
      </div>

      <div v-else-if="filteredPlugins.length === 0" class="py-12 text-center text-sm text-gray-500">{{ t('admin.plugins.noMatches') }}</div>
      <div v-else class="divide-y divide-gray-200 overflow-hidden rounded-xl border border-gray-200 bg-white dark:divide-dark-700 dark:border-dark-700 dark:bg-dark-800">
        <article
          v-for="plugin in filteredPlugins"
          :key="plugin.id"
          class="min-w-0"
          :data-plugin-id="plugin.id"
        >
          <div
            class="flex flex-col gap-3 px-5 py-4 lg:flex-row lg:items-center lg:justify-between"
          >
            <div class="min-w-0 flex-1">
              <div class="flex flex-wrap items-center gap-2">
                <h3
                  class="truncate text-base font-semibold text-gray-900 dark:text-white"
                >
                  {{ plugin.name }}
                </h3>
                <span class="font-mono text-xs text-gray-500"
                  >v{{ plugin.version }}</span
                >
                <span
                  class="rounded px-2 py-0.5 text-xs font-medium"
                  :class="stateClass(plugin.state)"
                >
                  {{ t(`admin.plugins.${plugin.state}`) }}
                </span>
                <span v-if="!plugin.compatibility.compatible" class="text-xs text-red-600">{{ t('admin.plugins.incompatible') }}</span>
                <span v-else-if="plugin.runtime_healthy" class="text-xs text-emerald-700 dark:text-emerald-300">{{ t('admin.plugins.healthy') }}</span>
              </div>
              <p class="mt-1 truncate text-xs text-gray-500" :title="plugin.plugin_key">
                {{ plugin.plugin_key
                }}<span v-if="plugin.author"> · {{ plugin.author }}</span>
              </p>
              <p
                v-if="plugin.description"
                class="mt-1 truncate text-sm text-gray-600 dark:text-gray-300"
                :title="plugin.description"
              >
                {{ plugin.description }}
              </p>
              <p v-if="plugin.last_error" class="mt-1 line-clamp-2 text-xs text-red-600 dark:text-red-400">{{ plugin.last_error }}</p>
            </div>
            <div class="flex flex-shrink-0 flex-wrap items-center gap-2">
            <button
              type="button"
              class="btn btn-secondary btn-sm"
              @click="openConfiguration(plugin)"
            >
              <Icon name="cog" size="sm" />
              {{ t("admin.plugins.configure") }}
            </button>
            <button v-if="hasEnabledBinding(plugin)" type="button" class="btn btn-secondary btn-sm" :disabled="busyID === plugin.id" @click="disablePlugin(plugin)">
              <Icon name="ban" size="sm" />{{ t('admin.plugins.disable') }}
            </button>
            <button v-else type="button" class="btn btn-primary btn-sm" :disabled="busyID === plugin.id || plugin.state === 'starting' || !plugin.compatibility.compatible" @click="enablePlugin(plugin)">
              <Icon name="play" size="sm" />{{ t('admin.plugins.enable') }}
            </button>
            <button type="button" class="btn btn-secondary btn-sm" :aria-expanded="expandedPlugins.has(plugin.id)" :aria-controls="`plugin-details-${plugin.id}`" @click="toggleDetails(plugin.id)">
              {{ expandedPlugins.has(plugin.id) ? t('admin.plugins.collapseDetails') : t('admin.plugins.showDetails') }}
            </button>
            </div>
          </div>

          <div v-show="expandedPlugins.has(plugin.id)" :id="`plugin-details-${plugin.id}`" class="border-t border-gray-100 bg-gray-50/60 dark:border-dark-700 dark:bg-dark-900/30">
          <div class="grid grid-cols-1 gap-x-6 gap-y-4 p-5 md:grid-cols-2">
            <div>
              <p class="text-xs font-medium uppercase text-gray-500">
                {{ t("admin.plugins.compatibility") }}
              </p>
              <div class="mt-2 flex items-center gap-2">
                <span
                  class="rounded px-2 py-0.5 text-xs font-medium"
                  :class="compatibilityClass(plugin.compatibility.status)"
                >
                  {{ t(`admin.plugins.${plugin.compatibility.status}`) }}
                </span>
                <span class="text-xs text-gray-500 dark:text-gray-400">{{
                  plugin.compatibility.message
                }}</span>
              </div>
              <dl
                class="mt-3 grid grid-cols-[auto,1fr] gap-x-3 gap-y-1 text-xs"
              >
                <dt class="text-gray-500">
                  {{ t("admin.plugins.currentVersion") }}
                </dt>
                <dd class="font-mono text-gray-800 dark:text-gray-200">
                  {{ plugin.compatibility.current_sub2api_version }}
                </dd>
                <dt class="text-gray-500">
                  {{ t("admin.plugins.requiredVersion") }}
                </dt>
                <dd class="font-mono text-gray-800 dark:text-gray-200">
                  {{ plugin.compatibility.required_sub2api_version }}
                </dd>
                <dt class="text-gray-500">
                  {{ t("admin.plugins.recommendedVersion") }}
                </dt>
                <dd class="font-mono text-gray-800 dark:text-gray-200">
                  {{ plugin.compatibility.recommended_sub2api_version || "-" }}
                </dd>
              </dl>
            </div>

            <div>
              <p class="text-xs font-medium uppercase text-gray-500">
                {{ t("admin.plugins.runtime") }}
              </p>
              <div class="mt-2 flex flex-wrap gap-2 text-xs">
                <span
                  class="rounded px-2 py-0.5"
                  :class="
                    plugin.runtime_healthy
                      ? 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300'
                      : 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300'
                  "
                >
                  {{
                    plugin.runtime_healthy
                      ? t("admin.plugins.healthy")
                      : t("admin.plugins.unhealthy")
                  }}
                </span>
                <span
                  class="rounded bg-gray-100 px-2 py-0.5 text-gray-600 dark:bg-dark-700 dark:text-gray-300"
                >
                  {{ t("admin.plugins.signature") }}:
                  {{ t(`admin.plugins.${plugin.signature_status}`) }}
                </span>
              </div>
              <p
                v-if="plugin.last_error"
                class="mt-3 break-words text-xs text-red-600 dark:text-red-400"
              >
                {{ plugin.last_error }}
              </p>
              <p
                v-else-if="plugin.runtime_message"
                class="mt-3 break-words text-xs text-gray-500"
              >
                {{ plugin.runtime_message }}
              </p>
              <p v-if="plugin.runtime_isolation" class="mt-2 text-xs text-gray-500">
                {{ t("admin.plugins.isolation") }}: {{ t(`admin.plugins.isolation_${plugin.runtime_isolation}`) }}
              </p>
            </div>

            <section class="space-y-3 md:col-span-2" :aria-label="t('admin.plugins.capabilities')">
              <h4 class="text-xs font-medium text-gray-500">{{ t("admin.plugins.capabilities") }}</h4>
              <div v-for="capability in plugin.manifest.capabilities" :key="capability.id" class="border-l-2 border-gray-200 pl-3 text-xs dark:border-dark-600">
                <p class="break-all font-mono text-gray-800 dark:text-gray-200">{{ capability.id }}</p>
                <dl class="mt-2 grid grid-cols-[auto,1fr] gap-x-3 gap-y-1">
                  <dt class="text-gray-500">{{ t("admin.plugins.scope") }}</dt>
                  <dd class="text-gray-700 dark:text-gray-300">{{ capability.platform }} / {{ capability.account_type }}</dd>
                  <template v-if="capability.kind">
                    <dt class="text-gray-500">{{ t("admin.plugins.permissions") }}</dt>
                    <dd class="break-words font-mono text-gray-700 dark:text-gray-300">{{ capability.permissions?.join(", ") || "—" }}</dd>
                    <dt class="text-gray-500">{{ t("admin.plugins.timeout") }}</dt>
                    <dd class="tabular-nums text-gray-700 dark:text-gray-300">{{ plugin.bindings.find(binding => binding.capability === capability.id)?.timeout_ms || capability.timeout_ms }} ms</dd>
                    <dt class="text-gray-500">{{ t("admin.plugins.failurePolicy") }}</dt>
                    <dd class="text-gray-700 dark:text-gray-300">{{ t(`admin.plugins.${capability.failure_mode}`) }}</dd>
                  </template>
                </dl>
                <p v-if="capability.id === 'request.preprocess.v1'" class="mt-2 text-gray-500">{{ t("admin.plugins.preprocessScope") }}</p>
              </div>
              <div v-for="status in plugin.capability_runtime || []" :key="status.capability" class="flex flex-wrap gap-x-4 gap-y-1 text-xs text-gray-500">
                <span>{{ t("admin.plugins.calls") }}: {{ status.calls }}</span>
                <span>{{ t("admin.plugins.failures") }}: {{ status.errors }}</span>
                <span>{{ t("admin.plugins.denied") }}: {{ status.denied }}</span>
                <span>{{ t("admin.plugins.inFlight") }}: {{ status.in_flight }} / {{ status.concurrency_limit }}</span>
                <span v-if="status.circuit_open" class="text-amber-700 dark:text-amber-300">{{ t("admin.plugins.circuitOpen") }}</span>
                <span v-if="status.message" class="w-full break-words text-red-600 dark:text-red-400">{{ status.message }}</span>
              </div>
            </section>

            <div class="md:col-span-2">
              <label
                class="flex items-center justify-between gap-4 text-xs font-medium text-gray-600 dark:text-gray-300"
              >
                <span>{{ t("admin.plugins.rollout") }}</span>
                <span class="w-11 text-right font-mono"
                  >{{
                    rolloutValues[plugin.id] ?? currentRollout(plugin)
                  }}%</span
                >
              </label>
              <input
                :value="rolloutValues[plugin.id] ?? currentRollout(plugin)"
                type="range"
                :min="plugin.manifest.schema_version === 2 ? 0 : 1"
                max="100"
                step="1"
                class="mt-2 w-full accent-primary-600"
                :disabled="hasEnabledBinding(plugin)"
                @input="setRollout(plugin.id, $event)"
              />
            </div>
          </div>

          <div
            class="flex flex-wrap justify-end gap-2 border-t border-gray-100 px-5 py-4 dark:border-dark-700"
          >
            <button v-if="plugin.manifest.schema_version === 2" type="button" class="btn btn-secondary btn-sm" :disabled="busyID === plugin.id || !plugin.compatibility.compatible || plugin.state === 'starting'" @click="openRouting(plugin)">
              {{ t("admin.plugins.routing") }}
            </button>
            <button v-if="plugin.manifest.schema_version === 2" type="button" class="btn btn-secondary btn-sm" :disabled="busyID === plugin.id" @click="openHostServices(plugin)">
              {{ t("admin.plugins.hostServices") }}
            </button>
            <button type="button" class="btn btn-secondary btn-sm" :disabled="busyID === plugin.id || plugin.state === 'starting'" @click="selectUpgrade(plugin)">
              {{ t("admin.plugins.upgrade") }}
            </button>
            <button type="button" class="btn btn-secondary btn-sm" :disabled="busyID === plugin.id" @click="openVersions(plugin)">
              {{ t("admin.plugins.versions") }}
            </button>
            <button
              type="button"
              class="btn btn-secondary btn-sm"
              :disabled="busyID === plugin.id"
              @click="testPlugin(plugin)"
            >
              <Icon name="beaker" size="sm" />
              {{ t("admin.plugins.test") }}
            </button>
            <button
              type="button"
              class="btn btn-danger btn-sm"
              :disabled="busyID === plugin.id || hasEnabledBinding(plugin)"
              @click="uninstallPlugin(plugin)"
            >
              <Icon name="trash" size="sm" />
              {{ t("admin.plugins.uninstall") }}
            </button>
          </div>
          </div>
        </article>
      </div>

      <BaseDialog
        :show="configPlugin !== null"
        :title="
          t('admin.plugins.configTitle', { name: configPlugin?.name || '' })
        "
        width="wide"
        @close="closeConfiguration"
      >
        <div
          class="relative min-h-[min(480px,60dvh)] overflow-hidden bg-gray-50 dark:bg-dark-900"
          :style="{ height: `min(${iframeHeight}px, calc(100dvh - 180px))` }"
        >
          <div
            v-if="uiLoading"
            class="absolute inset-0 z-10 flex items-center justify-center bg-white text-sm text-gray-500 dark:bg-dark-900"
            role="status"
          >
            {{ t("admin.plugins.loadingUI") }}
          </div>
          <div
            v-if="uiError"
            class="absolute inset-0 z-20 flex flex-col items-center justify-center bg-white p-8 text-center dark:bg-dark-900"
            role="alert"
          >
            <Icon name="exclamationTriangle" size="xl" class="text-amber-500" />
            <p class="mt-3 font-medium text-gray-800 dark:text-gray-200">
              {{ t("admin.plugins.uiUnavailable") }}
            </p>
            <p class="mt-1 max-w-xl text-sm text-gray-500">{{ uiError }}</p>
            <button type="button" class="btn btn-secondary mt-4" @click="configPlugin && openConfiguration(configPlugin)">{{ t('admin.plugins.retryUI') }}</button>
          </div>
          <iframe
            v-if="uiSession"
            ref="pluginFrame"
            :src="uiSession.url"
            sandbox="allow-scripts"
            :referrerpolicy="pluginUIFrameReferrerPolicy"
            class="h-full w-full border-0 bg-white dark:bg-dark-900"
            :title="
              t('admin.plugins.configTitle', { name: configPlugin?.name || '' })
            "
            @load="handlePluginFrameLoad"
            @error="handlePluginFrameError"
          />
        </div>
      </BaseDialog>

      <BaseDialog :show="versionPlugin !== null" :title="t('admin.plugins.versions')" @close="versionPlugin = null">
        <div class="space-y-4 p-5">
          <p class="text-sm text-gray-500">{{ t("admin.plugins.rollbackRetention") }}</p>
          <p v-if="versionsLoading" class="text-sm text-gray-500">{{ t("common.loading") }}</p>
          <p v-else-if="versionError" class="text-sm text-red-600 dark:text-red-400">{{ versionError }}</p>
          <p v-else-if="versionHistory.length === 0" class="text-sm text-gray-500">{{ t("admin.plugins.noVersions") }}</p>
          <div v-for="version in versionHistory" :key="version.id" class="flex flex-wrap items-center justify-between gap-3 border-t border-gray-200 py-3 dark:border-dark-700">
            <div class="min-w-0 text-sm">
              <p class="font-medium">v{{ version.version }}</p>
              <p class="text-xs text-gray-500">{{ t("admin.plugins.expires") }}: {{ new Date(version.expires_at).toLocaleString() }}</p>
              <p class="text-xs text-gray-500">{{ version.compatibility.message }}</p>
            </div>
            <button type="button" class="btn btn-secondary btn-sm" :disabled="busyID !== null || !version.compatibility.compatible" @click="rollbackPlugin(version)">
              {{ t("admin.plugins.rollback") }}
            </button>
          </div>
        </div>
      </BaseDialog>

      <TotpStepUpDialog :controller="pluginStepUp" />
      <PluginInstallDialog
        :inspection="pendingInstall?.inspection ?? null"
        :file-name="pendingInstall?.file.name ?? ''"
        :busy="uploading"
        :upgrade="!!pendingInstall?.target"
        @close="closePublisherReview"
        @confirm="confirmPublisherInstall"
      />
      <PluginRoutingDialog :plugin="routingPlugin" :busy="routingPlugin !== null && busyID === routingPlugin.id" @close="routingPlugin = null" @save="savePluginRouting" />
      <PluginHostDialog :plugin="hostPlugin" :stats="hostSnapshot" :grants="hostGrants" :loading="hostLoading" :busy="hostPlugin !== null && busyID === hostPlugin.id" :error="hostError" @close="hostPlugin = null" @refresh="refreshHostServices" @grant="grantHostSecret" @revoke="revokeHostSecret" />
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from "vue";
import { useI18n } from "vue-i18n";
import {
  adminAPI,
  type PluginInstallation,
  type PluginVersion,
  type PluginRoutingPolicy,
  type PluginHostSnapshot,
  type PluginSecretGrant,
  type PluginUISession,
  type PluginPackageInspection,
  type PluginPublisherApproval,
} from "@/api/admin";
import { useAppStore } from "@/stores";
import { buildApiUrl } from "@/api/url";
import {
  pluginBridgeExpectsResponse,
  pluginUIFrameReferrerPolicy,
} from "@/utils/pluginUIBridge";
import AppLayout from "@/components/layout/AppLayout.vue";
import BaseDialog from "@/components/common/BaseDialog.vue";
import Icon from "@/components/icons/Icon.vue";
import PluginRoutingDialog from "@/components/plugins/PluginRoutingDialog.vue";
import PluginHostDialog from "@/components/plugins/PluginHostDialog.vue";
import PluginInstallDialog from "@/components/plugins/PluginInstallDialog.vue";
import TotpStepUpDialog from "@/components/auth/TotpStepUpDialog.vue";
import {
  isStepUpBlocked,
  isStepUpCancelled,
  stepUpBlockReason,
  useStepUp,
} from "@/composables/useStepUp";

interface PluginBridgeMessage {
  source?: string;
  bridge_token?: string;
  type?: string;
  request_id?: string;
  config?: unknown;
  height?: unknown;
  level?: unknown;
  message?: unknown;
}

const { t } = useI18n();
const appStore = useAppStore();
const pluginStepUp = useStepUp();
const plugins = ref<PluginInstallation[]>([]);
const searchQuery = ref("");
const stateFilter = ref("all");
const expandedPlugins = ref(new Set<number>());
const filteredPlugins = computed(() => {
  const query = searchQuery.value.trim().toLocaleLowerCase();
  return plugins.value.filter(plugin =>
    (stateFilter.value === "all" || plugin.state === stateFilter.value) &&
    (!query || [plugin.name, plugin.plugin_key, plugin.author, plugin.description].join(" ").toLocaleLowerCase().includes(query)),
  );
});
function toggleDetails(id: number): void {
  const expanded = new Set(expandedPlugins.value);
  if (expanded.has(id)) expanded.delete(id);
  else expanded.add(id);
  expandedPlugins.value = expanded;
}
const loading = ref(false);
const uploading = ref(false);
const pendingInstall = ref<{ file: File; inspection: PluginPackageInspection; target: PluginInstallation | null } | null>(null);
let packageGeneration = 0;
const busyID = ref<number | null>(null);
const fileInput = ref<HTMLInputElement | null>(null);
const upgradeFileInput = ref<HTMLInputElement | null>(null);
const upgradeTarget = ref<PluginInstallation | null>(null);
const versionPlugin = ref<PluginInstallation | null>(null);
const versionHistory = ref<PluginVersion[]>([]);
const versionsLoading = ref(false);
const versionError = ref("");
const routingPlugin = ref<PluginInstallation | null>(null);
const hostPlugin = ref<PluginInstallation | null>(null);
const hostSnapshot = ref<PluginHostSnapshot | null>(null);
const hostGrants = ref<PluginSecretGrant[]>([]);
const hostLoading = ref(false);
const hostError = ref("");
const rolloutValues = ref<Record<number, number>>({});
const configPlugin = ref<PluginInstallation | null>(null);
const uiSession = ref<PluginUISession | null>(null);
const pluginFrame = ref<HTMLIFrameElement | null>(null);
const uiLoading = ref(false);
const uiError = ref("");
const iframeHeight = ref(640);
const pluginFrameLoaded = ref(false);
const pendingBridgeRequests = new Map<string, number>();
let frameGeneration = 0;
let uiReadyTimeout: number | undefined;

function clearUIReadyTimeout(): void {
  if (uiReadyTimeout !== undefined) window.clearTimeout(uiReadyTimeout);
  uiReadyTimeout = undefined;
}
function waitForUIReady(): void {
  clearUIReadyTimeout();
  uiReadyTimeout = window.setTimeout(() => {
    uiLoading.value = false;
    uiError.value = t("admin.plugins.uiReadyTimeout");
  }, 15_000);
}

function errorMessage(error: unknown): string {
  if (typeof error === "object" && error !== null && "message" in error) {
    return String(
      (error as { message?: unknown }).message || t("common.unknownError"),
    );
  }
  return t("common.unknownError");
}

function reportSensitiveActionError(error: unknown): void {
  if (isStepUpCancelled(error)) return;
  if (isStepUpBlocked(error)) {
    appStore.showError(
      stepUpBlockReason(error) === "STEP_UP_ADMIN_API_KEY_FORBIDDEN"
        ? t("stepUp.adminApiKeyForbidden")
        : t("stepUp.notEnabled"),
    );
    return;
  }
  appStore.showError(errorMessage(error));
}

async function loadPlugins(): Promise<void> {
  loading.value = true;
  try {
    plugins.value = await adminAPI.plugins.list();
    for (const plugin of plugins.value) {
      rolloutValues.value[plugin.id] = currentRollout(plugin);
    }
  } catch (error: unknown) {
    appStore.showError(errorMessage(error));
  } finally {
    loading.value = false;
  }
}

async function handleFileSelected(event: Event): Promise<void> {
  const target = event.target as HTMLInputElement;
  const file = target.files?.[0];
  target.value = "";
  if (!file || !file.name.toLowerCase().endsWith(".s2plugin")) {
    appStore.showError(t("admin.plugins.fileRequired"));
    return;
  }
  await inspectAndInstall(file, null);
}

async function writePluginPackage(file: File, target: PluginInstallation | null, approval?: PluginPublisherApproval): Promise<void> {
  if (target) {
    if (approval) await adminAPI.plugins.upgrade(target.id, file, true, approval);
    else await adminAPI.plugins.upgrade(target.id, file, true);
    closeConfiguration();
  } else if (approval) await adminAPI.plugins.upload(file, approval);
  else await adminAPI.plugins.upload(file);
}

async function installSucceeded(target: PluginInstallation | null): Promise<void> {
  appStore.showSuccess(t(target ? "admin.plugins.upgradeSuccess" : "admin.plugins.uploadSuccess"));
  await loadPlugins();
}

async function inspectAndInstall(file: File, target: PluginInstallation | null): Promise<void> {
  const generation = ++packageGeneration;
  pendingInstall.value = null;
  uploading.value = true;
  busyID.value = target?.id ?? null;
  try {
    const installed = await pluginStepUp.run(async () => {
      await adminAPI.plugins.authorizeUpload();
      const inspection = await adminAPI.plugins.inspect(file);
      if (generation !== packageGeneration) return false;
      if (inspection.signature_status === "untrusted") {
        pendingInstall.value = { file, inspection, target };
        return false;
      }
      await writePluginPackage(file, target);
      return true;
    });
    if (installed && generation === packageGeneration) await installSucceeded(target);
  } catch (error: unknown) {
    if (generation === packageGeneration) reportSensitiveActionError(error);
  } finally {
    if (generation === packageGeneration) { uploading.value = false; busyID.value = null; }
  }
}

function closePublisherReview(): void {
  if (uploading.value) return;
  packageGeneration++;
  pendingInstall.value = null;
}

async function confirmPublisherInstall(): Promise<void> {
  const pending = pendingInstall.value;
  if (!pending?.inspection.publisher || uploading.value) return;
  const generation = packageGeneration;
  uploading.value = true;
  busyID.value = pending.target?.id ?? null;
  try {
    await pluginStepUp.run(async () => {
      await adminAPI.plugins.authorizeUpload();
      await writePluginPackage(pending.file, pending.target, {
        package_sha256: pending.inspection.package_sha256,
        publisher_fingerprint: pending.inspection.publisher!.fingerprint,
      });
    });
    if (generation === packageGeneration) {
      pendingInstall.value = null;
      await installSucceeded(pending.target);
    }
  } catch (error: unknown) {
    if (generation === packageGeneration) reportSensitiveActionError(error);
  } finally {
    if (generation === packageGeneration) { uploading.value = false; busyID.value = null; }
  }
}

function currentRollout(plugin: PluginInstallation): number {
  return plugin.bindings[0]?.rollout_percent ?? 100;
}

function selectUpgrade(plugin: PluginInstallation): void {
  upgradeTarget.value = plugin;
  upgradeFileInput.value?.click();
}

async function savePluginRouting(policies: PluginRoutingPolicy[]): Promise<void> {
  const plugin = routingPlugin.value;
  if (!plugin) return;
  busyID.value = plugin.id;
  try {
    await pluginStepUp.run(() => adminAPI.plugins.saveRouting(plugin.id, policies, plugin.updated_at));
    routingPlugin.value = null;
    appStore.showSuccess(t("admin.plugins.routingSaved"));
    await loadPlugins();
  } catch (error: unknown) {
    reportSensitiveActionError(error);
  } finally {
    busyID.value = null;
  }
}

async function openRouting(plugin: PluginInstallation): Promise<void> {
  routingPlugin.value = plugin;
  busyID.value = plugin.id;
  try {
    const fresh = await adminAPI.plugins.get(plugin.id);
    if (routingPlugin.value?.id === plugin.id) routingPlugin.value = fresh;
  } catch (error: unknown) {
    if (routingPlugin.value?.id === plugin.id) routingPlugin.value = null;
    appStore.showError(errorMessage(error));
  } finally {
    if (busyID.value === plugin.id) busyID.value = null;
  }
}

async function openHostServices(plugin: PluginInstallation): Promise<void> {
  hostPlugin.value = plugin;
  hostSnapshot.value = null;
  hostGrants.value = [];
  await refreshHostServices();
}
async function refreshHostServices(): Promise<void> {
  const plugin = hostPlugin.value;
  if (!plugin) return;
  hostLoading.value = true;
  hostError.value = "";
  try {
    const [stats, grants] = await Promise.all([adminAPI.plugins.hostStats(plugin.id), adminAPI.plugins.secretGrants(plugin.id)]);
    if (hostPlugin.value?.id === plugin.id) {
      hostSnapshot.value = stats;
      hostGrants.value = grants;
    }
  } catch (error: unknown) {
    if (hostPlugin.value?.id === plugin.id) hostError.value = errorMessage(error);
  } finally {
    if (hostPlugin.value?.id === plugin.id) hostLoading.value = false;
  }
}
async function grantHostSecret(capability: string, alias: string, value: string, ttlSeconds: number): Promise<void> {
  const plugin = hostPlugin.value;
  if (!plugin) return;
  busyID.value = plugin.id;
  try {
    await pluginStepUp.run(() => adminAPI.plugins.putSecretGrant(plugin.id, capability, alias, value, ttlSeconds));
    appStore.showSuccess(t("admin.plugins.secretSaved"));
    await refreshHostServices();
  } catch (error: unknown) {
    reportSensitiveActionError(error);
  } finally { busyID.value = null; }
}
async function revokeHostSecret(grant: PluginSecretGrant): Promise<void> {
  const plugin = hostPlugin.value;
  if (!plugin || !window.confirm(t("admin.plugins.confirmRevokeSecret"))) return;
  busyID.value = plugin.id;
  try {
    await pluginStepUp.run(() => adminAPI.plugins.deleteSecretGrant(plugin.id, grant.capability, grant.alias));
    await refreshHostServices();
  } catch (error: unknown) {
    reportSensitiveActionError(error);
  } finally { busyID.value = null; }
}

async function handleUpgradeFile(event: Event): Promise<void> {
  const input = event.target as HTMLInputElement;
  const file = input.files?.[0];
  const plugin = upgradeTarget.value;
  input.value = "";
  upgradeTarget.value = null;
  if (!file || !plugin) return;
  if (!file.name.toLowerCase().endsWith(".s2plugin")) {
    appStore.showError(t("admin.plugins.fileRequired"));
    return;
  }
  if (!window.confirm(t("admin.plugins.confirmUpgrade"))) return;
  await inspectAndInstall(file, plugin);
}

async function openVersions(plugin: PluginInstallation): Promise<void> {
  versionPlugin.value = plugin;
  versionHistory.value = [];
  versionError.value = "";
  versionsLoading.value = true;
  try {
    const history = await adminAPI.plugins.versions(plugin.id);
    if (versionPlugin.value?.id === plugin.id) versionHistory.value = history;
  } catch (error: unknown) {
    if (versionPlugin.value?.id === plugin.id) versionError.value = errorMessage(error);
  } finally {
    if (versionPlugin.value?.id === plugin.id) versionsLoading.value = false;
  }
}

async function rollbackPlugin(version: PluginVersion): Promise<void> {
  const plugin = versionPlugin.value;
  if (!plugin || !window.confirm(t("admin.plugins.confirmRollback", { version: version.version }))) return;
  const acceptUntested = !version.compatibility.tested;
  if (acceptUntested && !window.confirm(t("admin.plugins.confirmUntested"))) return;
  busyID.value = plugin.id;
  try {
    await pluginStepUp.run(() => adminAPI.plugins.rollback(plugin.id, version.id, acceptUntested));
    closeConfiguration();
    versionPlugin.value = null;
    appStore.showSuccess(t("admin.plugins.rollbackSuccess"));
    await loadPlugins();
  } catch (error: unknown) {
    reportSensitiveActionError(error);
  } finally {
    busyID.value = null;
  }
}

function hasEnabledBinding(plugin: PluginInstallation): boolean {
  return plugin.bindings.some((binding) => binding.enabled);
}

function setRollout(id: number, event: Event): void {
  const value = Number((event.target as HTMLInputElement).value);
  const min = plugins.value.find(plugin => plugin.id === id)?.manifest.schema_version === 2 ? 0 : 1;
  rolloutValues.value[id] = Math.min(100, Math.max(min, value));
}

async function enablePlugin(plugin: PluginInstallation): Promise<void> {
  let acceptUntested = false;
  if (!plugin.compatibility.tested) {
    acceptUntested = window.confirm(t("admin.plugins.confirmUntested"));
    if (!acceptUntested) return;
  }
  busyID.value = plugin.id;
  try {
    await pluginStepUp.run(() =>
      adminAPI.plugins.enable(
        plugin.id,
        rolloutValues.value[plugin.id] ?? 100,
        acceptUntested,
      ),
    );
    appStore.showSuccess(t("admin.plugins.enableSuccess"));
    await loadPlugins();
  } catch (error: unknown) {
    reportSensitiveActionError(error);
  } finally {
    busyID.value = null;
  }
}

async function disablePlugin(plugin: PluginInstallation): Promise<void> {
  if (!window.confirm(t("admin.plugins.confirmDisable"))) return;
  busyID.value = plugin.id;
  try {
    await pluginStepUp.run(() => adminAPI.plugins.disable(plugin.id));
    appStore.showSuccess(t("admin.plugins.disableSuccess"));
    await loadPlugins();
  } catch (error: unknown) {
    reportSensitiveActionError(error);
  } finally {
    busyID.value = null;
  }
}

async function uninstallPlugin(plugin: PluginInstallation): Promise<void> {
  if (!window.confirm(t("admin.plugins.confirmUninstall"))) return;
  busyID.value = plugin.id;
  try {
    await pluginStepUp.run(() => adminAPI.plugins.remove(plugin.id));
    appStore.showSuccess(t("admin.plugins.uninstallSuccess"));
    await loadPlugins();
  } catch (error: unknown) {
    reportSensitiveActionError(error);
  } finally {
    busyID.value = null;
  }
}

async function testPlugin(plugin: PluginInstallation): Promise<void> {
  busyID.value = plugin.id;
  try {
    const result = await pluginStepUp.run(() =>
      adminAPI.plugins.test(plugin.id),
    );
    if (result.success)
      appStore.showSuccess(result.message || t("admin.plugins.testSuccess"));
    else appStore.showError(result.message || t("common.error"));
  } catch (error: unknown) {
    reportSensitiveActionError(error);
  } finally {
    busyID.value = null;
  }
}

async function openConfiguration(plugin: PluginInstallation): Promise<void> {
  const generation = ++frameGeneration;
  clearUIReadyTimeout();
  configPlugin.value = plugin;
  uiSession.value = null;
  pluginFrameLoaded.value = false;
  clearPendingBridgeRequests();
  uiLoading.value = true;
  uiError.value = "";
  iframeHeight.value = 640;
  try {
    const session = await adminAPI.plugins.createUISession(plugin.id);
    if (generation !== frameGeneration) return;
    if (!session.url.startsWith('/api/v1/plugin-ui/')) throw new Error(t('admin.plugins.bridgeRejected'));
    uiSession.value = { ...session, url: buildApiUrl(session.url) };
    waitForUIReady();
  } catch (error: unknown) {
    if (generation !== frameGeneration) return;
    uiLoading.value = false;
    uiError.value = errorMessage(error);
  }
}

function closeConfiguration(): void {
  frameGeneration++;
  clearUIReadyTimeout();
  clearPendingBridgeRequests();
  pluginFrameLoaded.value = false;
  configPlugin.value = null;
  uiSession.value = null;
  uiLoading.value = false;
  uiError.value = "";
}

function clearPendingBridgeRequests(): void {
  for (const timeout of pendingBridgeRequests.values()) window.clearTimeout(timeout);
  pendingBridgeRequests.clear();
}

function handlePluginFrameLoad(): void {
  // A load can also be caused by a plugin navigating its iframe. Drop all
  // outstanding responses so a late config response is never sent to the new document.
  if (pluginFrameLoaded.value) {
    frameGeneration++;
    clearPendingBridgeRequests();
    uiLoading.value = true;
    waitForUIReady();
  }
  pluginFrameLoaded.value = true;
  // load also fires for blocked/error documents. Only a validated ready
  // message from this sandbox window proves that the plugin UI is usable.
}

function handlePluginFrameError(): void {
  clearUIReadyTimeout();
  uiLoading.value = false;
  uiError.value = t('admin.plugins.uiLoadFailed');
}

function registerBridgeRequest(requestID: string): void {
  const timeout = window.setTimeout(() => {
    pendingBridgeRequests.delete(requestID);
  }, 30_000);
  pendingBridgeRequests.set(requestID, timeout);
}

function postBridgeResult(
  request: PluginBridgeMessage,
  payload: Record<string, unknown>,
  generation: number,
): void {
  if (generation !== frameGeneration) return;
  if (!pluginFrame.value?.contentWindow || !uiSession.value) return;
  const requestID = typeof request.request_id === "string" ? request.request_id.trim() : "";
  const timeout = pendingBridgeRequests.get(requestID);
  if (!requestID || timeout === undefined) return;
  window.clearTimeout(timeout);
  pendingBridgeRequests.delete(requestID);
  pluginFrame.value.contentWindow.postMessage(
    {
      source: "sub2api-plugin-host",
      bridge_token: uiSession.value.bridge_token,
      type: `${request.type}.result`,
      request_id: requestID,
      ...payload,
    },
    // The sandboxed iframe has an opaque origin, so no fixed target origin exists.
    // Pending request tracking plus load invalidation prevents cross-navigation leaks.
    "*",
  );
}

async function handleBridgeMessage(event: MessageEvent): Promise<void> {
  if (
    !uiSession.value ||
    !configPlugin.value ||
    event.source !== pluginFrame.value?.contentWindow ||
    event.origin !== "null"
  )
    return;

  const generation = frameGeneration;
  const pluginID = configPlugin.value.id;
  const message = event.data as PluginBridgeMessage;
  if (
    !message ||
    message.source !== "sub2api-plugin-ui" ||
    message.bridge_token !== uiSession.value.bridge_token
  )
    return;

  const requestID = typeof message.request_id === "string" ? message.request_id.trim() : "";
  const expectsResponse = pluginBridgeExpectsResponse(message.type);
  if (expectsResponse) {
    if (!requestID || pendingBridgeRequests.has(requestID)) return;
    registerBridgeRequest(requestID);
  }

  try {
    switch (message.type) {
      case "sub2api.plugin.ready":
        clearUIReadyTimeout();
        uiLoading.value = false;
        uiError.value = "";
        break;
      case "config.load": {
        const config = await adminAPI.plugins.getConfig(pluginID);
        postBridgeResult(message, { ok: true, config }, generation);
        break;
      }
      case "config.save": {
        if (
          !message.config ||
          typeof message.config !== "object" ||
          Array.isArray(message.config)
        ) {
          throw new Error(t("admin.plugins.bridgeRejected"));
        }
        const config = await pluginStepUp.run(() =>
          adminAPI.plugins.saveConfig(
            pluginID,
            message.config as Record<string, unknown>,
          ),
        );
        postBridgeResult(message, { ok: true, config }, generation);
        appStore.showSuccess(t("common.saved"));
        break;
      }
      case "config.test": {
        const result = await pluginStepUp.run(() =>
          adminAPI.plugins.test(pluginID),
        );
        postBridgeResult(message, { ok: result.success, result }, generation);
        if (result.success)
          appStore.showSuccess(
            result.message || t("admin.plugins.testSuccess"),
          );
        else appStore.showError(result.message || t("common.error"));
        break;
      }
      case "plugin.status": {
        const result = await adminAPI.plugins.status(pluginID);
        postBridgeResult(message, { ok: true, result }, generation);
        break;
      }
      case "ui.resize": {
        const height = Number(message.height);
        if (Number.isFinite(height))
          iframeHeight.value = Math.min(960, Math.max(520, Math.round(height)));
        break;
      }
      case "ui.notify": {
        const text =
          typeof message.message === "string"
            ? message.message.slice(0, 500)
            : "";
        if (!text) break;
        if (message.level === "error") appStore.showError(text);
        else if (message.level === "success") appStore.showSuccess(text);
        else appStore.showInfo(text);
        break;
      }
    }
  } catch (error: unknown) {
    if (isStepUpBlocked(error)) reportSensitiveActionError(error);
    postBridgeResult(message, {
      ok: false,
      error: isStepUpCancelled(error) ? t("common.cancel") : errorMessage(error),
    }, generation);
  }
}

function stateClass(state: PluginInstallation["state"]): string {
  if (state === "enabled")
    return "bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300";
  if (state === "error" || state === "incompatible")
    return "bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-300";
  if (state === "starting")
    return "bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300";
  return "bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-gray-300";
}

function compatibilityClass(
  status: PluginInstallation["compatibility"]["status"],
): string {
  if (status === "compatible")
    return "bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300";
  if (status === "untested")
    return "bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300";
  return "bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-300";
}

onMounted(() => {
  window.addEventListener("message", handleBridgeMessage);
  void loadPlugins();
});

onBeforeUnmount(() => {
  packageGeneration++;
  pendingInstall.value = null;
  window.removeEventListener("message", handleBridgeMessage);
  clearPendingBridgeRequests();
  clearUIReadyTimeout();
  frameGeneration++;
});
</script>
