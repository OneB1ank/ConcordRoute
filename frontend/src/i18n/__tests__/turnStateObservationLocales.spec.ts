import { describe, expect, it } from 'vitest'
import en from '../locales/en'
import zh from '../locales/zh'

describe('Turn-State observation direction', () => {
  // 响应缺失不能被界面标成出站请求没有携带 state。
  it('中文明确显示响应方向及证据边界', () => {
    expect(zh.usage.codexTurnStateAbsent).toBe('响应 state · 未返回')
    expect(zh.usage.codexTurnStatePresent).toBe('响应 state · {bytes} B')
    expect(zh.usage.codexTurnStateHint).toContain('不表示请求是否携带')
  })

  it('英文使用相同的响应方向', () => {
    expect(en.usage.codexTurnStateAbsent).toBe('Response state · absent')
    expect(en.usage.codexTurnStatePresent).toBe('Response state · {bytes} B')
    expect(en.usage.codexTurnStateHint).toContain('does not indicate whether the request')
  })
})
