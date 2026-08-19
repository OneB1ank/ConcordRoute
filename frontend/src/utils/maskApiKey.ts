// API Key 只展示固定前后缀，避免管理界面泄露完整密钥。
export function maskApiKey(key: string): string {
  if (!key) return ''
  if (key.length <= 12) return `${key.slice(0, 4)}***`
  return `${key.slice(0, 6)}...${key.slice(-4)}`
}
