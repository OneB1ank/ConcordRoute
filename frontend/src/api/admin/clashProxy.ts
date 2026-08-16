import { apiClient } from '../client'

const baseURL = '/admin/proxies/clash'

export interface ClashProxyNode {
  id: number
  name: string
  node_type: string
  source_type: string
  config: Record<string, unknown>
  status: string
}

export interface ClashProxyProfile {
  id: number
  name: string
  strategy: string
  test_url: string
  interval_seconds: number
  status: string
  node_ids: number[]
  managed_proxy_id?: number
  config: Record<string, unknown>
}

export interface ClashProxyBinding {
  id: number
  account_id: number
  account_name: string
  account_platform: string
  profile_id: number
  profile_name: string
  previous_proxy_id?: number
  enabled: boolean
}

export interface ClashProxyRuntime {
  profile_id: number
  runtime_type: string
  pid?: number
  mixed_port?: number
  controller_port?: number
  status: string
  last_error?: string
  proxy_url?: string
}

export interface ClashProxyRuntimeStatus {
  total: number
  starting: number
  running: number
  failed: number
  stopped: number
}

export interface ClashProxyProfileTest {
  profile_id: number
  healthy: boolean
  status: string
  version?: string
  error?: string
  proxy_url?: string
}

export interface ClashProxyProfileInput {
  name: string
  strategy: string
  test_url: string
  interval_seconds: number
  node_ids: number[]
}

async function listNodes(): Promise<ClashProxyNode[]> {
  const { data } = await apiClient.get<ClashProxyNode[]>(`${baseURL}/nodes`)
  return data || []
}

async function createNode(input: { name: string; url: string }): Promise<ClashProxyNode> {
  const { data } = await apiClient.post<ClashProxyNode>(`${baseURL}/nodes`, input)
  return data
}

async function importNodes(input: { format: string; content?: string; url?: string }): Promise<ClashProxyNode[]> {
  const { data } = await apiClient.post<ClashProxyNode[]>(`${baseURL}/nodes/import`, input)
  return data || []
}

async function deleteNode(id: number): Promise<void> {
  await apiClient.delete(`${baseURL}/nodes/${id}`)
}

async function listProfiles(): Promise<ClashProxyProfile[]> {
  const { data } = await apiClient.get<ClashProxyProfile[]>(`${baseURL}/profiles`)
  return data || []
}

async function createProfile(input: ClashProxyProfileInput): Promise<ClashProxyProfile> {
  const { data } = await apiClient.post<ClashProxyProfile>(`${baseURL}/profiles`, input)
  return data
}

async function updateProfile(id: number, input: ClashProxyProfileInput): Promise<ClashProxyProfile> {
  const { data } = await apiClient.put<ClashProxyProfile>(`${baseURL}/profiles/${id}`, input)
  return data
}

async function startProfile(id: number): Promise<ClashProxyRuntime> {
  const { data } = await apiClient.post<ClashProxyRuntime>(`${baseURL}/profiles/${id}/start`)
  return data
}

async function stopProfile(id: number): Promise<ClashProxyRuntime> {
  const { data } = await apiClient.post<ClashProxyRuntime>(`${baseURL}/profiles/${id}/stop`)
  return data
}

async function restartProfile(id: number): Promise<ClashProxyRuntime> {
  const { data } = await apiClient.post<ClashProxyRuntime>(`${baseURL}/profiles/${id}/restart`)
  return data
}

async function testProfile(id: number): Promise<ClashProxyProfileTest> {
  const { data } = await apiClient.post<ClashProxyProfileTest>(`${baseURL}/profiles/${id}/test`)
  return data
}

async function getProfileRuntime(id: number): Promise<ClashProxyRuntime> {
  const { data } = await apiClient.get<ClashProxyRuntime>(`${baseURL}/profiles/${id}/runtime`)
  return data
}

async function getRuntimeStatus(): Promise<ClashProxyRuntimeStatus> {
  const { data } = await apiClient.get<ClashProxyRuntimeStatus>(`${baseURL}/runtime/status`)
  return data
}

async function listBindings(): Promise<ClashProxyBinding[]> {
  const { data } = await apiClient.get<ClashProxyBinding[]>(`${baseURL}/bindings`)
  return data || []
}

async function createBinding(input: { account_id: number; profile_id: number }): Promise<ClashProxyBinding> {
  const { data } = await apiClient.post<ClashProxyBinding>(`${baseURL}/bindings`, input)
  return data
}

async function deleteBinding(id: number): Promise<void> {
  await apiClient.delete(`${baseURL}/bindings/${id}`)
}

export default {
  listNodes,
  createNode,
  importNodes,
  deleteNode,
  listProfiles,
  createProfile,
  updateProfile,
  startProfile,
  stopProfile,
  restartProfile,
  testProfile,
  getProfileRuntime,
  getRuntimeStatus,
  listBindings,
  createBinding,
  deleteBinding
}
