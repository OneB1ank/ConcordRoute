import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

const source = readFileSync(resolve(dirname(fileURLToPath(import.meta.url)), '../GroupsView.vue'), 'utf8')

// 与现有分组表单静态契约测试一致，防止新增开关遗漏编辑回填或请求展开。
describe('audio transcription group controls', () => {
  it('keeps audio independent from Live and default-off', () => {
    expect(source.match(/allow_audio_transcription: false/g)).toHaveLength(2)
    expect(source).toContain('v-model="createForm.allow_audio_transcription"')
    expect(source).toContain('v-model="editForm.allow_audio_transcription"')
    expect(source).toContain('editForm.allow_audio_transcription = group.allow_audio_transcription ?? false')
    expect(source).toContain('...createForm')
    expect(source).toContain('...editForm')
    expect(source).toContain('v-model.number="createForm.audio_stt_price_per_hour"')
    expect(source).toContain('v-model.number="editForm.audio_stt_price_per_hour"')
  })
})
