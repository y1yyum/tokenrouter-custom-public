import { afterEach, describe, expect, it } from 'vitest'
import { buildEmbeddedUrl, detectTheme } from '../embedded-url'

/**
 * embedded-url.spec.ts checks the URL contract shared by custom menu frames
 * and new tabs. All cases inspect strings locally, including saved settings
 * with obsolete parameter names and destinations with repeated query options.
 */

describe('embedded-url', () => {
  afterEach(() => {
    document.documentElement.classList.remove('dark')
  })

  // A saved checkout URL includes retired host fields and a fragment route.
  // Only its display settings change, and repeated destination options retain order.
  it('removes retired fields and preserves configured options and the fragment', () => {
    const base = 'https://checkout.example.test/pay?plan=a&plan=b&theme=old&lang=en&ui_mode=old'
    const result = buildEmbeddedUrl(
      base + '&user_id=&token=&token=&src_host=&src_url=#/checkout',
      'dark',
      'zh',
    )
    expect(result).toBe(
      'https://checkout.example.test/pay?plan=a&plan=b&theme=dark&lang=zh&ui_mode=embedded#/checkout',
    )
  })

  // A caller supplies only the checkout address. Its defaults select light
  // embedded mode and leave the destination free of any host-derived parameters.
  it('adds only display defaults to the configured address', () => {
    expect(buildEmbeddedUrl('http://checkout.example.test/pay')).toBe(
      'http://checkout.example.test/pay?theme=light&ui_mode=embedded',
    )
  })

  // Each address falls outside the menu's supported web URL contract. An empty
  // result lets the view hide both the frame and the external link.
  it.each(['', '/relative', 'https://', 'about:blank', 'https://example@checkout.example.test'])(
    'rejects unsupported destination %j',
    (address) => {
      expect(buildEmbeddedUrl(address)).toBe('')
    },
  )

  // The document root has the dark theme class at detection time. The helper
  // must return dark for the URL-building caller using that current document state.
  it('detects dark mode from document root class', () => {
    document.documentElement.classList.add('dark')
    expect(detectTheme()).toBe('dark')
  })
})
