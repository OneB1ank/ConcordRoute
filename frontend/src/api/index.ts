/**
 * API Client for Sub2API Backend
 * Central export point for all API modules
 */

// Re-export the HTTP client
export { apiClient, buildApiUrl, buildGatewayUrl } from './client'

// Auth API
export { authAPI, isTotp2FARequired, type LoginResponse } from './auth'

// User APIs
export { keysAPI } from './keys'
export { usageAPI } from './usage'
export { userAPI } from './user'
export { redeemAPI, type RedeemHistoryItem } from './redeem'
export { paymentAPI } from './payment'
export { userGroupsAPI } from './groups'
export * as batchImageAPI from './batchImage'
export { totpAPI } from './totp'
export { teamAPI } from './team'
export { marketplaceAPI } from './marketplace'
export { dataSharingAPI } from './dataSharing'
export { passkeyAPI, type PasskeyCredentialSummary } from './passkey'
export { default as announcementsAPI } from './announcements'
export { channelMonitorUserAPI } from './channelMonitor'

// Admin APIs
export { adminAPI } from './admin'

// Default export
export { default } from './client'
