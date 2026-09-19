import { apiClient } from '../client'

export interface PluginCapability {
  id: string
  platform: string
  account_type: string
  kind?: 'hook' | 'provider' | 'event_sink' | 'worker'
  permissions?: string[]
  timeout_ms?: number
  failure_mode?: 'fail_closed' | 'fail_open' | 'async'
  synchronous?: boolean
}

export interface PluginRequirements {
  sub2api: string
  recommended_sub2api_version?: string
  tested_sub2api_versions?: string[]
  plugin_protocol: number
  transport_api?: number
  extension_api?: number
  ui_bridge: number
}

export interface PluginManifest {
  schema_version: number
  id: string
  name: string
  version: string
  description?: string
  author?: string
  requires: PluginRequirements
  capabilities: PluginCapability[]
  ui: { entrypoint: string }
}

export interface PluginCompatibility {
  compatible: boolean
  tested: boolean
  status: 'compatible' | 'untested' | 'incompatible'
  message: string
  current_sub2api_version: string
  required_sub2api_version: string
  recommended_sub2api_version: string
  plugin_protocol: number
  transport_api: number
  ui_bridge: number
}

export interface PluginBinding {
  id: number
  plugin_id: number
  capability: string
  platform: string
  account_type: string
  enabled: boolean
  rollout_percent: number
  priority?: number
  account_ids?: number[]
  user_ids?: number[]
  group_ids?: number[]
  max_concurrency?: number
  timeout_ms?: number
}

export interface PluginRoutingPolicy {
  capability: string
  priority: number
  account_ids: number[]
  user_ids: number[]
  group_ids: number[]
  rollout_percent: number
  max_concurrency: number
  timeout_ms: number
}

export interface PluginSecretGrant {
  plugin_id: number
  capability: string
  alias: string
  expires_at: string
  updated_at: string
}
export interface PluginHostSnapshot {
  logs: number
  metrics: Record<string, number>
  events_accepted: number
  events_dropped: number
  recent_events: { sequence: number; capability: string; name: string; value: number; time: string }[]
}
export async function hostStats(id: number): Promise<PluginHostSnapshot> {
  const { data } = await apiClient.get<PluginHostSnapshot>(`/admin/plugins/${id}/host`)
  return data
}
export async function secretGrants(id: number): Promise<PluginSecretGrant[]> {
  const { data } = await apiClient.get<PluginSecretGrant[]>(`/admin/plugins/${id}/secret-grants`)
  return data
}
export async function putSecretGrant(id: number, capability: string, alias: string, value: string, ttlSeconds: number): Promise<void> {
  await apiClient.put(`/admin/plugins/${id}/secret-grants`, { capability, alias, value, ttl_seconds: ttlSeconds })
}
export async function deleteSecretGrant(id: number, capability: string, alias: string): Promise<void> {
  await apiClient.delete(`/admin/plugins/${id}/secret-grants`, { data: { capability, alias } })
}

export async function saveRouting(id: number, policies: PluginRoutingPolicy[], expectedUpdatedAt: string): Promise<PluginInstallation> {
  const { data } = await apiClient.put<PluginInstallation>(`/admin/plugins/${id}/routing`, {
    policies, expected_updated_at: expectedUpdatedAt
  })
  return data
}

export interface PluginInstallation {
  id: number
  plugin_key: string
  name: string
  version: string
  description: string
  author: string
  manifest: PluginManifest
  binary_sha256: string
  signature_status: 'trusted' | 'unsigned'
  state: 'disabled' | 'starting' | 'enabled' | 'error' | 'incompatible'
  last_error: string
  installed_at: string
  enabled_at?: string
  updated_at: string
  bindings: PluginBinding[]
  compatibility: PluginCompatibility
  runtime_healthy: boolean
  runtime_version?: string
  runtime_isolation?: 'process' | 'container'
  runtime_message: string
  capability_runtime?: {
    capability: string
    healthy: boolean
    message?: string
    in_flight: number
    calls: number
    errors: number
    denied: number
    circuit_open: boolean
    concurrency_limit: number
  }[]
}

export interface PluginPackageInspection {
  manifest: PluginManifest
  compatibility: PluginCompatibility
  package_sha256: string
  signature_status: 'trusted' | 'unsigned' | 'untrusted'
  publisher?: { key_id: string; fingerprint: string }
  runtime_isolation: 'process' | 'container'
}

export interface PluginPublisherApproval {
  package_sha256: string
  publisher_fingerprint: string
}

function appendPublisherApproval(form: FormData, approval?: PluginPublisherApproval): void {
  if (!approval) return
  form.append('trust_publisher', 'true')
  form.append('package_sha256', approval.package_sha256)
  form.append('publisher_fingerprint', approval.publisher_fingerprint)
}

export async function inspect(file: File): Promise<PluginPackageInspection> {
  const form = new FormData()
  form.append('plugin', file)
  const { data } = await apiClient.post<PluginPackageInspection>('/admin/plugins/inspect', form, {
    headers: { 'Content-Type': 'multipart/form-data' }, timeout: 120000
  })
  return data
}

export interface PluginTestResult {
  success: boolean
  message: string
  latency_ms: number
  status_json?: string
}

export interface PluginStatusResult {
  healthy: boolean
  message: string
  status_json?: string
}

export interface PluginVersion {
  id: number
  plugin_id: number
  version: string
  binary_sha256: string
  saved_at: string
  expires_at: string
  manifest: PluginManifest
  compatibility: PluginCompatibility
}

export async function versions(id: number): Promise<PluginVersion[]> {
  const { data } = await apiClient.get<PluginVersion[]>(`/admin/plugins/${id}/versions`)
  return data
}

export async function upgrade(id: number, file: File, acceptUntested: boolean, approval?: PluginPublisherApproval): Promise<PluginInstallation> {
  const form = new FormData()
  form.append('plugin', file)
  form.append('accept_untested', String(acceptUntested))
  appendPublisherApproval(form, approval)
  const { data } = await apiClient.post<PluginInstallation>(`/admin/plugins/${id}/upgrade`, form, {
    headers: { 'Content-Type': 'multipart/form-data' }, timeout: 120000
  })
  return data
}

export async function rollback(id: number, versionID: number, acceptUntested: boolean): Promise<PluginInstallation> {
  const { data } = await apiClient.post<PluginInstallation>(`/admin/plugins/${id}/rollback`, {
    version_id: versionID, accept_untested: acceptUntested
  }, { timeout: 120000 })
  return data
}

export interface PluginUISession {
  url: string
  bridge_token: string
  ui_bridge_version: number
  expires_at: string
}

export async function list(): Promise<PluginInstallation[]> {
  const { data } = await apiClient.get<PluginInstallation[]>('/admin/plugins')
  return data
}

export async function status(id: number): Promise<PluginStatusResult> {
  const { data } = await apiClient.get<PluginStatusResult>(`/admin/plugins/${id}/status`)
  return data
}

export async function get(id: number): Promise<PluginInstallation> {
  const { data } = await apiClient.get<PluginInstallation>(`/admin/plugins/${id}`)
  return data
}

export async function upload(file: File, approval?: PluginPublisherApproval): Promise<PluginInstallation> {
  const form = new FormData()
  form.append('plugin', file)
  appendPublisherApproval(form, approval)
  const { data } = await apiClient.post<PluginInstallation>('/admin/plugins/upload', form, {
    headers: { 'Content-Type': 'multipart/form-data' },
    timeout: 120000
  })
  return data
}

export async function authorizeUpload(): Promise<void> {
  await apiClient.post('/admin/plugins/authorize-upload', {})
}

export async function enable(
  id: number,
  rolloutPercent: number,
  acceptUntested: boolean
): Promise<PluginInstallation> {
  const { data } = await apiClient.post<PluginInstallation>(`/admin/plugins/${id}/enable`, {
    rollout_percent: rolloutPercent,
    accept_untested: acceptUntested
  })
  return data
}

export async function disable(id: number): Promise<PluginInstallation> {
  const { data } = await apiClient.post<PluginInstallation>(`/admin/plugins/${id}/disable`)
  return data
}

export async function remove(id: number): Promise<void> {
  await apiClient.delete(`/admin/plugins/${id}`)
}

export async function getConfig(id: number): Promise<Record<string, unknown>> {
  const { data } = await apiClient.get<Record<string, unknown>>(`/admin/plugins/${id}/config`)
  return data
}

export async function saveConfig(
  id: number,
  config: Record<string, unknown>
): Promise<Record<string, unknown>> {
  const { data } = await apiClient.put<Record<string, unknown>>(`/admin/plugins/${id}/config`, config)
  return data
}

export async function test(id: number): Promise<PluginTestResult> {
  const { data } = await apiClient.post<PluginTestResult>(`/admin/plugins/${id}/test`)
  return data
}

export async function createUISession(id: number): Promise<PluginUISession> {
  const { data } = await apiClient.post<PluginUISession>(`/admin/plugins/${id}/ui-session`)
  return data
}

export default {
  inspect,
  authorizeUpload,
  hostStats,
  secretGrants,
  putSecretGrant,
  deleteSecretGrant,
  get,
  saveRouting,
  versions,
  upgrade,
  rollback,
  list,
  upload,
  enable,
  disable,
  remove,
  getConfig,
  saveConfig,
  test,
  status,
  createUISession
}
