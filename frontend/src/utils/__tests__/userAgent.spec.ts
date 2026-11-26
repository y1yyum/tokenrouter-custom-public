import { describe, expect, it } from 'vitest'

import { formatCompactUserAgent } from '../userAgent'

/**
 * userAgent.spec.ts checks browser, SDK, and unknown header summaries without
 * depending on the browser running the tests. Recorded header strings exercise
 * compatibility marker precedence, platform identification, and display length.
 */

describe('formatCompactUserAgent', () => {
  /**
   * SDK and CLI headers carry environment settings after their product versions.
   * The summary retains the first product and version while omitting that environment text.
   */
  it.each([
    ['claude-cli/2.1.12 (external, cli)', 'claude-cli/2.1.12'],
    ['  openai-python/1.68.2 Python/3.12.3 macOS/14.7  ', 'openai-python/1.68.2'],
    ['codex_cli_rs/0.45.0 (Linux 6.10.0; x86_64)', 'codex_cli_rs/0.45.0'],
  ])('保留客户端产品与版本 %s', (ua, expected) => {
    expect(formatCompactUserAgent(ua)).toBe(expected)
  })

  /**
   * Browser headers include engine aliases and overlapping platform markers. The
   * summary uses the browser's major version and the most specific supported system.
   */
  it.each([
    [
      'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/131.0.0.0 Safari/537.36',
      'Chrome 131 · Win',
    ],
    [
      'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/131.0 Safari/537.36 Edg/131.0.12',
      'Edge 131 · Win',
    ],
    [
      'Mozilla/5.0 (X11; Linux x86_64; rv:133.0) Gecko/20100101 Firefox/133.0',
      'Firefox 133 · Linux',
    ],
    [
      'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1 Version/18.0 Mobile/15E148 Safari/604.1',
      'Safari 18 · iOS',
    ],
    [
      'Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 Chrome/131.0 Mobile Safari/537.36',
      'Chrome 131 · Android',
    ],
  ])('识别浏览器与系统 %s', (ua, expected) => {
    expect(formatCompactUserAgent(ua)).toBe(expected)
  })

  /**
   * Unknown text can be short or exceed the display preview. Short text is trimmed
   * and kept, while long text receives an ellipsis after a bounded number of characters.
   */
  it('未知 UA 保留可辨识的短预览', () => {
    expect(formatCompactUserAgent('  Internal request client  ')).toBe('Internal request client')
    expect(formatCompactUserAgent('Mozilla/5.0 (Unknown browser platform) UnknownBrowser')).toBe(
      'Mozilla/5.0 (Unknown browser pl…',
    )
  })

  /**
   * A missing or blank user-agent header has no useful summary. The caller receives
   * empty text to drive the cell's existing placeholder behavior.
   */
  it.each([undefined, null, '', '   '])('空 UA 返回空字符串 %s', (value) => {
    expect(formatCompactUserAgent(value)).toBe('')
  })
})
