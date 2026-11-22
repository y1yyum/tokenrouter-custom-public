/**
 * embedded-url.ts builds the destination shared by a custom menu's iframe and
 * external link. The caller supplies the display settings. Configured paths,
 * unrelated query parameters, and fragments belong to the destination page.
 */

/**
 * These former host parameters are reserved even in saved menu URLs created
 * before display settings became the only context shared with embedded pages.
 */
const retiredHostParams = ['user_id', 'token', 'src_host', 'src_url']

/**
 * buildEmbeddedUrl applies the current theme, optional language and embedded
 * mode to an absolute web URL. An unusable address returns an empty string
 * so the page can hide both navigation controls.
 */
export function buildEmbeddedUrl(
  baseUrl: string,
  theme: 'light' | 'dark' = 'light',
  lang?: string,
): string {
  let url: URL
  try {
    url = new URL(baseUrl)
  } catch {
    // An invalid setting selects the page's unconfigured state before either
    // control can navigate relative to the current host.
    return ''
  }
  if ((url.protocol !== 'https:' && url.protocol !== 'http:') || url.username || url.password) {
    return ''
  }

  // Saved menu settings can contain repeated legacy fields. Delete every
  // occurrence while retaining destination-specific options and hash routes.
  for (const key of retiredHostParams) url.searchParams.delete(key)
  url.searchParams.set('theme', theme)
  if (lang) url.searchParams.set('lang', lang)
  url.searchParams.set('ui_mode', 'embedded')
  return url.toString()
}

/**
 * detectTheme reads the document root dark class for one URL-building
 * operation. It returns light without a document and does not subscribe to
 * later theme changes, so callers rebuild URLs when needed.
 */
export function detectTheme(): 'light' | 'dark' {
  if (typeof document === 'undefined') return 'light'
  return document.documentElement.classList.contains('dark') ? 'dark' : 'light' // check-ui-allow: One-time document theme detection for noncomponent URL construction.
}
