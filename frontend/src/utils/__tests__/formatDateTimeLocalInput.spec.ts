import { describe, expect, it } from 'vitest'

import {
  formatDateTimeLocalInput,
  getBrowserTimeZone,
  parseDateTimeLocalInput
} from '../format'

describe('formatDateTimeLocalInput', () => {
  it('往返转换分钟精度的浏览器本地时间', () => {
    const timestamp = Math.floor(new Date(2026, 7, 30, 7, 24).getTime() / 1000)

    expect(formatDateTimeLocalInput(timestamp)).toBe('2026-08-30T07:24')
    expect(parseDateTimeLocalInput('2026-08-30T07:24')).toBe(timestamp)
  })
})

describe('parseDateTimeLocalInput', () => {
  it('按本地日期组件解析，并拒绝携带时区的值', () => {
    const expected = Math.floor(new Date(2026, 7, 30, 7, 24, 10).getTime() / 1000)

    expect(parseDateTimeLocalInput('2026-08-30T07:24:10')).toBe(expected)
    expect(parseDateTimeLocalInput('2026-08-30T07:24:10+08:00')).toBeNull()
    expect(parseDateTimeLocalInput('2026-08-30T07:24:10Z')).toBeNull()
  })

  it('拒绝空值、格式错误和溢出的日历值', () => {
    expect(parseDateTimeLocalInput('')).toBeNull()
    expect(parseDateTimeLocalInput('2026-08-30 07:24')).toBeNull()
    expect(parseDateTimeLocalInput('2026-02-30T07:24')).toBeNull()
    expect(parseDateTimeLocalInput('2026-08-30T24:00')).toBeNull()
    expect(parseDateTimeLocalInput('2026-08-30T07:60')).toBeNull()
  })

  it('接受可选秒和小数秒', () => {
    const expected = Math.floor(new Date(2026, 7, 30, 7, 24, 10, 120).getTime() / 1000)

    expect(parseDateTimeLocalInput('2026-08-30T07:24:10.12')).toBe(expected)
  })
})

describe('getBrowserTimeZone', () => {
  it('返回时区标识或 UTC 回退值', () => {
    expect(getBrowserTimeZone()).toBeTruthy()
  })
})
