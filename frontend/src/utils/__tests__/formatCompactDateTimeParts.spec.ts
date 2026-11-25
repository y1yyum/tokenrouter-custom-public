import { afterEach, describe, expect, it, vi } from 'vitest'

import { formatCompactDateTimeParts, truncateMiddle } from '../format'

/**
 * formatCompactDateTimeParts.spec.ts checks compact dates and identifiers using
 * known local wall-clock values and fixed system time. The fixtures exercise
 * visible text, full tooltips, and short identifiers that need no shortening.
 */

afterEach(() => vi.useRealTimers())

describe('formatCompactDateTimeParts', () => {
  /**
   * A timestamp falls in the current local year and begins at midnight. The
   * default date omits the year, and the explicit format includes its last two digits.
   */
  it('同年默认省略年份，可按需保留两位年份', () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date(2026, 10, 25, 12))
    const instant = new Date(2026, 10, 25, 0, 6, 18)

    expect(formatCompactDateTimeParts(instant)).toEqual({
      date: '11-25',
      time: '00:06:18',
      full: '2026-11-25 00:06:18',
    })
    expect(formatCompactDateTimeParts(instant, 'always').date).toBe('26-11-25')
  })

  /**
   * A UTC timestamp is created from a local instant in the previous year. Parsing
   * the string and comparing years uses the same local calendar as the tooltip.
   */
  it('ISO 时间按本地日期和时钟判断跨年', () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date(2026, 0, 1, 0, 0, 1))
    const instant = new Date(2025, 11, 31, 23, 59, 59).toISOString()

    expect(formatCompactDateTimeParts(instant)).toEqual({
      date: '25-12-31',
      time: '23:59:59',
      full: '2025-12-31 23:59:59',
    })
  })

  /**
   * The caller receives no timestamp or a value that cannot be parsed. All display
   * parts are empty so a cell can show one placeholder without an invalid tooltip.
   */
  it.each([undefined, null, '', 'invalid', new Date(NaN)])(
    '空值或无效时间返回空字段 %s',
    (value) => {
      expect(formatCompactDateTimeParts(value)).toEqual({ date: '', time: '', full: '' })
    },
  )
})

describe('truncateMiddle', () => {
  /**
   * A long request ID is rendered with both visible ends and one ellipsis. Values
   * short enough to fit the same length and missing values keep their text.
   */
  it('保留长 ID 的前六位、后四位和短 ID 原样', () => {
    expect(truncateMiddle('6d431b22-4d38-42f1-9f09-0f5a110b1be2')).toBe('6d431b…1be2')
    expect(truncateMiddle('12345678901')).toBe('12345678901')
    expect(truncateMiddle('abc')).toBe('abc')
    expect(truncateMiddle(undefined)).toBe('')
  })

  /**
   * A caller supplies custom lengths and a string containing supplementary Unicode
   * characters. The selected ends retain complete code points, including when the
   * caller asks for no suffix.
   */
  it('支持自定义长度、空后缀和非 BMP 字符', () => {
    expect(truncateMiddle('😀甲乙丙丁🚀', 1, 1)).toBe('😀…🚀')
    expect(truncateMiddle('sample-value', 3, 0)).toBe('sam…')
  })
})
