import { apiClient } from '../client'

export interface CodexAttestationCollectorStatus {
  running: boolean
  session_ttl_seconds: number
  max_records_per_session: number
  active_sessions: number
  started_at?: string
}

export interface CodexAttestationCollectorSession {
  token: string
  expires_at: string
  bridge_query: string
  header_name: string
}

export interface CodexAttestationCaptureRecord {
  id: string
  captured_at: string
  event: 'initialize' | 'generate' | string
  api_key_id: number
  account_id: number
  connection_id: string
  session_id?: string
  thread_id?: string
  client_name?: string
  client_version?: string
  request_attestation: boolean
  initialize_id?: string
  generate_request_id?: number
  status: string
  proof_length?: number
  proof_sha256?: string
}

export async function status(): Promise<CodexAttestationCollectorStatus> {
  const { data } = await apiClient.get<CodexAttestationCollectorStatus>('/admin/codex-attestation-collector/status')
  return data
}

export async function start(): Promise<CodexAttestationCollectorStatus> {
  const { data } = await apiClient.post<CodexAttestationCollectorStatus>('/admin/codex-attestation-collector/start')
  return data
}

export async function stop(): Promise<CodexAttestationCollectorStatus> {
  const { data } = await apiClient.post<CodexAttestationCollectorStatus>('/admin/codex-attestation-collector/stop')
  return data
}

export async function createSession(): Promise<CodexAttestationCollectorSession> {
  const { data } = await apiClient.post<CodexAttestationCollectorSession>('/admin/codex-attestation-collector/sessions')
  return data
}

export async function listCaptures(token: string): Promise<CodexAttestationCaptureRecord[]> {
  const { data } = await apiClient.get<CodexAttestationCaptureRecord[]>(`/admin/codex-attestation-collector/sessions/${encodeURIComponent(token)}/captures`)
  return data
}

export async function deleteSession(token: string): Promise<{ deleted: boolean }> {
  const { data } = await apiClient.delete<{ deleted: boolean }>(`/admin/codex-attestation-collector/sessions/${encodeURIComponent(token)}`)
  return data
}

const codexAttestationCollectorAPI = {
  status,
  start,
  stop,
  createSession,
  listCaptures,
  deleteSession
}

export default codexAttestationCollectorAPI
