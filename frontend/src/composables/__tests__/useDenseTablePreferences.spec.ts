import { beforeEach, describe, expect, it, vi } from 'vitest'
import { useDenseTablePreferences } from '../useDenseTablePreferences'

// useDenseTablePreferences.spec.ts exercises upgrades across the three browser
// preference namespaces. It seeds the same storage consumed by page setup and keeps
// the old values available so returning to an older client remains possible.
describe('dense table preferences', () => {
  beforeEach(() => localStorage.clear())

  // Earlier releases hid a score, upstream identifier, or user agent by default.
  // Each belongs to a larger group with visible data after the upgrade.
  it.each([
    [
      'providers',
      'provider-hidden-columns',
      ['notes', 'proxy', 'scheduler_score', 'rate_multiplier', 'today_stats'],
    ],
    [
      'adminUsage',
      'usage-hidden-columns',
      ['reasoning_effort', 'upstream_request_id', 'user_agent'],
    ],
    ['userUsage', 'user-usage-hidden-columns', ['user_agent']],
  ] as const)('shows all columns for an unchanged %s profile', (page, key, fields) => {
    const raw = JSON.stringify(fields)
    localStorage.setItem(key, raw)
    expect([...useDenseTablePreferences(page).hiddenColumns]).toEqual([])
    expect(localStorage.getItem(key)).toBe(raw)
    expect(localStorage.getItem(key + '-dense-layout-v1')).toBe('[]')
  })

  // Users may have hidden complete groups and reordered fields before upgrading.
  // The first field fixes the composite position, including columns that are currently hidden.
  it('maps entire groups and merges order once without structural or unknown keys', () => {
    localStorage.setItem(
      'provider-hidden-columns',
      JSON.stringify(['today_stats', 'usage', 'notes', 'select', 'actions']),
    )
    localStorage.setItem(
      'admin-providers-column-order',
      JSON.stringify([
        'expires_at',
        'actions',
        'id',
        'created_at',
        'notes',
        'groups',
        'removed',
        'select',
      ]),
    )
    const prefs = useDenseTablePreferences('providers')
    expect([...prefs.hiddenColumns]).toEqual(['usage'])
    const order = JSON.parse(localStorage.getItem(prefs.columnOrderStorageKey)!)
    expect(order).toEqual([
      'timestamps',
      'identity',
      'groups',
      'platform_type',
      'status_schedule',
      'capacity',
      'usage',
    ])
    prefs.toggleColumn('usage')
    prefs.toggleColumn('identity')
    localStorage.setItem(prefs.columnOrderStorageKey, JSON.stringify(order.toReversed()))
    const second = useDenseTablePreferences('providers')
    expect([...second.hiddenColumns]).toEqual([])
    expect(JSON.parse(localStorage.getItem(second.columnOrderStorageKey)!)).toEqual(
      order.toReversed(),
    )
  })

  // Membership can resolve after page setup. The same stored choice must preserve
  // the member row when only key and group had been hidden.
  it('requires all three caller fields for both user roles', () => {
    const old = 'user-usage-hidden-columns'
    localStorage.setItem(old, '["api_key","group"]')
    expect(useDenseTablePreferences('userUsage').isColumnVisible('caller')).toBe(true)
    localStorage.removeItem(old + '-dense-layout-v1')
    localStorage.setItem(old, '["api_key","group","user","created_at"]')
    expect([...useDenseTablePreferences('userUsage').hiddenColumns]).toEqual(['caller'])
  })

  // Migrating a profile with an older version marker replays its ID default.
  // A valid new empty array always wins over any earlier hidden selection.
  it('preserves complete administrator groups and respects current destinations', () => {
    localStorage.setItem('usage-hidden-columns', '["request_id","user","created_at"]')
    const prefs = useDenseTablePreferences('adminUsage')
    expect([...prefs.hiddenColumns]).toEqual(['request_trace'])
    localStorage.setItem('usage-hidden-columns-dense-layout-v1', '{')
    expect([...useDenseTablePreferences('adminUsage').hiddenColumns]).toEqual([])
    expect(localStorage.getItem('usage-hidden-columns-dense-layout-v1')).toBe('[]')
  })

  // A persistence failure on the second key leaves visibility complete. Its retry
  // can still read the old order without reviving a previously hidden column.
  it('retries the unwritten order independently', () => {
    localStorage.setItem('user-usage-column-order', '["latency","ip_address"]')
    const original = Storage.prototype.setItem
    const spy = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(function (
      this: Storage,
      key,
      value,
    ) {
      if (key === 'user-usage-column-order-v2') throw new Error('full')
      original.call(this, key, value)
    })
    try {
      useDenseTablePreferences('userUsage').toggleColumn('tokens')
    } finally {
      spy.mockRestore()
    }
    const prefs = useDenseTablePreferences('userUsage')
    expect([...prefs.hiddenColumns]).toEqual(['tokens'])
    expect(JSON.parse(localStorage.getItem(prefs.columnOrderStorageKey)!).slice(0, 2)).toEqual([
      'performance',
      'client_network',
    ])
  })
})
