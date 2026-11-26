/**
 * userAgent.ts summarizes recorded user agents for narrow client columns. It
 * prioritizes specific browser tokens that appear alongside compatibility tokens
 * and keeps the leading product and version for SDKs and command-line clients.
 *
 * User agents can contain several browser names or operating system markers.
 * Browser precedence distinguishes Edge from Chrome and keeps Safari's reported
 * version separate from its engine version. Platform precedence recognizes Android
 * and iOS before their compatibility markers. The result is display text, so
 * consumers showing or copying full request data keep the original field.
 */

/**
 * Browser patterns pair the display name with a major-version capture in order of
 * specificity. Compatible browsers can include both Chrome and Safari tokens.
 */
const browserPatterns = [
  ['Edge', /\b(?:EdgA?|EdgiOS|Edge)\/(\d+)/i],
  ['Opera', /\b(?:OPR|OPiOS)\/(\d+)/i],
  ['Samsung Internet', /\bSamsungBrowser\/(\d+)/i],
  ['Chrome', /\b(?:Chrome|CriOS|Chromium)\/(\d+)/i],
  ['Firefox', /\b(?:Firefox|FxiOS)\/(\d+)/i],
  ['Safari', /\bVersion\/(\d+)[\d.]*\b[^)]*\bSafari\//i],
] as const

/**
 * Platform patterns identify the first matching system marker in a browser user
 * agent, putting mobile and ChromeOS variants before broader compatibility markers.
 */
const platformPatterns = [
  ['Android', /\bAndroid\b/i],
  ['iOS', /\b(?:iPhone|iPad|iPod)\b/i],
  ['Win', /\bWindows\b/i],
  ['ChromeOS', /\bCrOS\b/i],
  ['Mac', /\b(?:Macintosh|Mac OS X)\b/i],
  ['Linux', /\bLinux\b/i],
] as const

/**
 * formatCompactUserAgent returns a browser and platform summary or a client product
 * with its version. It trims display whitespace and returns empty text for absent
 * input. Unstructured values keep a bounded preview, including agents with an
 * unrecognized browser after a Mozilla compatibility token. Use the original input
 * for tools that require the exact recorded header, including clipboard and titles.
 */
export function formatCompactUserAgent(ua: string | null | undefined): string {
  const value = ua?.trim() ?? ''
  if (!value) return ''

  const product = /^[\w.+-]+\/[\w.+-]+/.exec(value)?.[0]
  if (product && !/^Mozilla\//i.test(product)) return product

  for (const [browser, pattern] of browserPatterns) {
    const match = pattern.exec(value)
    if (!match) continue

    const platform = platformPatterns.find(([, marker]) => marker.test(value))?.[0]
    const version = `${browser} ${match[1]}`
    return platform ? `${version} · ${platform}` : version
  }

  // Leave one character for an ellipsis on unstructured agents that need truncation.
  const characters = Array.from(value)
  const maxPreviewLength = 32
  return characters.length > maxPreviewLength
    ? `${characters.slice(0, maxPreviewLength - 1).join('')}…`
    : value
}
