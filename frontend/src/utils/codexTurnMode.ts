export type CodexTurnMode = 'passthrough' | 'converge'

// 旧账号及异常值统一按透传显示，与服务端缺省行为一致。
export function readCodexTurnMode(extra?: Record<string, unknown> | null): CodexTurnMode {
  return extra?.codex_turn_mode === 'converge' ? 'converge' : 'passthrough'
}
