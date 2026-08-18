import axios from 'axios'
import type { ApiResponse } from '@/types'
import { buildApiUrl } from './url'

const AUTH_TOKEN_KEY = 'auth_token'
const AUTH_USER_KEY = 'auth_user'
const REFRESH_TOKEN_KEY = 'refresh_token'
const TOKEN_EXPIRES_AT_KEY = 'token_expires_at'
// 锁名需要与升级前仍打开的页面保持一致，避免并发刷新令牌。
const TOKEN_REFRESH_LOCK_NAME = 'tokenrouter-auth-token-refresh'
const TOKEN_REFRESH_TIMEOUT_MS = 30_000
const TOKEN_REFRESH_BUFFER_MS = 120_000
const PEER_REFRESH_WAIT_MS = 1_000
const PEER_REFRESH_GRACE_MS = 1_000
const PEER_REFRESH_POLL_MS = 25

export interface RefreshTokenResponse {
  access_token: string
  refresh_token: string
  expires_in: number
  token_type: string
}

export interface RefreshAuthTokensOptions {
  /** 收到 401 的原请求所携带的 access token。 */
  failedAccessToken?: string | null
}

interface AuthSnapshot {
  accessToken: string | null
  refreshToken: string
  expiresAt: number
  userID: number | null
}

let inFlightRefresh: Promise<RefreshTokenResponse> | null = null

function getStoredUserID(): number | null {
  const rawUser = localStorage.getItem(AUTH_USER_KEY)
  if (!rawUser) {
    return null
  }

  try {
    const id = Number((JSON.parse(rawUser) as { id?: unknown }).id)
    return Number.isFinite(id) && id > 0 ? id : null
  } catch {
    return null
  }
}

function readAuthSnapshot(): AuthSnapshot {
  const refreshToken = localStorage.getItem(REFRESH_TOKEN_KEY)
  if (!refreshToken) {
    throw new Error('No refresh token available')
  }

  return {
    accessToken: localStorage.getItem(AUTH_TOKEN_KEY),
    refreshToken,
    expiresAt: Number(localStorage.getItem(TOKEN_EXPIRES_AT_KEY)),
    userID: getStoredUserID()
  }
}

function readStoredTokenPair(snapshot: AuthSnapshot): RefreshTokenResponse | null {
  const accessToken = localStorage.getItem(AUTH_TOKEN_KEY)
  const refreshToken = localStorage.getItem(REFRESH_TOKEN_KEY)
  const expiresAt = Number(localStorage.getItem(TOKEN_EXPIRES_AT_KEY))

  if (
    !accessToken ||
    !refreshToken ||
    !Number.isFinite(expiresAt) ||
    expiresAt <= Date.now() ||
    getStoredUserID() !== snapshot.userID
  ) {
    return null
  }

  return {
    access_token: accessToken,
    refresh_token: refreshToken,
    expires_in: Math.max(1, Math.ceil((expiresAt - Date.now()) / 1000)),
    token_type: 'Bearer'
  }
}

function readPeerRefreshResult(
  snapshot: AuthSnapshot,
  failedAccessToken?: string | null
): RefreshTokenResponse | null {
  const storedPair = readStoredTokenPair(snapshot)
  if (!storedPair) {
    return null
  }

  if (storedPair.refresh_token !== snapshot.refreshToken) {
    return storedPair
  }

  if (
    failedAccessToken &&
    snapshot.accessToken !== failedAccessToken &&
    storedPair.access_token === snapshot.accessToken
  ) {
    return storedPair
  }

  if (!failedAccessToken) {
    const expiresAt = Number(localStorage.getItem(TOKEN_EXPIRES_AT_KEY))
    if (
      expiresAt === snapshot.expiresAt &&
      storedPair.access_token === snapshot.accessToken &&
      expiresAt > Date.now() + TOKEN_REFRESH_BUFFER_MS
    ) {
      return storedPair
    }
  }

  return null
}

async function waitForPeerRefresh(
  snapshot: AuthSnapshot,
  failedAccessToken?: string | null,
  deadline = Date.now() + PEER_REFRESH_WAIT_MS
): Promise<RefreshTokenResponse | null> {
  while (Date.now() < deadline) {
    const peerResult = readPeerRefreshResult(snapshot, failedAccessToken)
    if (peerResult) {
      return peerResult
    }
    await new Promise((resolve) => window.setTimeout(resolve, PEER_REFRESH_POLL_MS))
  }

  return readPeerRefreshResult(snapshot, failedAccessToken)
}

function persistTokenPair(tokens: RefreshTokenResponse): void {
  localStorage.setItem(AUTH_TOKEN_KEY, tokens.access_token)
  localStorage.setItem(TOKEN_EXPIRES_AT_KEY, String(Date.now() + tokens.expires_in * 1000))
  // 轮换后的 refresh token 最后写入，其他标签页可把它的变化视为整组 token 的提交标记。
  localStorage.setItem(REFRESH_TOKEN_KEY, tokens.refresh_token)
}

async function requestTokenPair(
  snapshot: AuthSnapshot,
  failedAccessToken?: string | null,
  mayHaveUncoordinatedPeer = false
): Promise<RefreshTokenResponse> {
  // 一次性 token 的竞争失败方需要允许胜方用完整 HTTP 超时发布新 token，不能使用任意的短等待时间。
  const peerRefreshDeadline = Date.now() + TOKEN_REFRESH_TIMEOUT_MS + PEER_REFRESH_GRACE_MS

  try {
    const response = await axios.post<ApiResponse<RefreshTokenResponse>>(
      buildApiUrl('/auth/refresh'),
      { refresh_token: snapshot.refreshToken },
      { headers: { 'Content-Type': 'application/json' }, timeout: TOKEN_REFRESH_TIMEOUT_MS }
    )
    const payload = response.data
    if (payload.code !== 0 || !payload.data) {
      throw new Error(payload.message || 'Token refresh failed')
    }

    if (
      localStorage.getItem(REFRESH_TOKEN_KEY) !== snapshot.refreshToken ||
      getStoredUserID() !== snapshot.userID
    ) {
      const peerResult = readPeerRefreshResult(snapshot, failedAccessToken)
      if (peerResult) {
        return peerResult
      }
      throw new Error('Session changed during token refresh')
    }

    persistTokenPair(payload.data)
    return payload.data
  } catch (error) {
    // 4xx 可能先于竞争胜方的响应抵达，因此无锁竞争要等到共享请求期限；其他错误只做短暂对账。
    const responseStatus = (error as { response?: { status?: unknown } }).response?.status
    const isTokenRejection =
      typeof responseStatus === 'number' && responseStatus >= 400 && responseStatus < 500
    const peerResult = await waitForPeerRefresh(
      snapshot,
      failedAccessToken,
      isTokenRejection && mayHaveUncoordinatedPeer
        ? peerRefreshDeadline
        : Date.now() + PEER_REFRESH_WAIT_MS
    )
    if (peerResult) {
      return peerResult
    }
    throw error
  }
}

async function runRefresh(options: RefreshAuthTokensOptions): Promise<RefreshTokenResponse> {
  const snapshot = readAuthSnapshot()
  const refresh = async (mayHaveUncoordinatedPeer = false): Promise<RefreshTokenResponse> => {
    const peerResult = readPeerRefreshResult(snapshot, options.failedAccessToken)
    if (peerResult) {
      return peerResult
    }
    return requestTokenPair(snapshot, options.failedAccessToken, mayHaveUncoordinatedPeer)
  }

  if (typeof navigator !== 'undefined' && navigator.locks) {
    return navigator.locks.request(TOKEN_REFRESH_LOCK_NAME, () => refresh(false))
  }

  return refresh(true)
}

/**
 * 刷新并持久化浏览器会话。
 *
 * 同一文档中的调用共享一个 Promise；Web Locks 串行化跨标签页刷新，快照检查则采用同一用户已轮换的 token。
 */
export function refreshAuthTokens(
  options: RefreshAuthTokensOptions = {}
): Promise<RefreshTokenResponse> {
  if (inFlightRefresh) {
    return inFlightRefresh
  }

  const pending = runRefresh(options)
  inFlightRefresh = pending
  const clearPending = (): void => {
    if (inFlightRefresh === pending) {
      inFlightRefresh = null
    }
  }
  void pending.then(clearPending, clearPending)
  return pending
}
