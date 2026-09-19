import { describe, expect, it } from 'vitest'
import { readCodexTurnMode } from '../codexTurnMode'

describe('readCodexTurnMode', () => {
  // 与后端保持严格匹配，异常值不会意外启用实验策略。
  it.each([undefined, null, {}, { codex_turn_mode: true }, { codex_turn_mode: 'CONVERGE' }])(
    '缺失或非法配置默认为透传：%s',
    (extra) => expect(readCodexTurnMode(extra)).toBe('passthrough')
  )
  it('只接受明确的实验模式', () => {
    expect(readCodexTurnMode({ codex_turn_mode: 'converge' })).toBe('converge')
    expect(readCodexTurnMode({ codex_turn_mode: 'passthrough' })).toBe('passthrough')
  })
})
