import { usePersistedTableColumns } from '@/composables/usePersistedTableColumns'

// useDenseTablePreferences.ts restores visibility and order for the provider and usage
// tables before their first request or table mount. A composite column can represent
// several fields from older profiles. Hiding just one of those fields must still leave
// the rest of the group accessible. Team owners and personal users share preference
// keys, so the caller group always includes the legacy member field during migration.
//
// Visibility and order have independent destination keys. A completed write survives
// even when storage rejects the other one, and old clients retain their own settings.
// Mandatory columns stay visible while selection and action cells retain their positions.

// DenseTablePage identifies independent browser preference namespaces for the three pages.
type DenseTablePage = 'providers' | 'adminUsage' | 'userUsage'

// ColumnSources maps canonical display order to the legacy fields represented by each column.
type ColumnSources = Record<string, readonly string[]>

// Provider columns include structural cells; useTableColumnOrder keeps those cells fixed.
const providerSources: ColumnSources = {
  select: ['select'],
  identity: ['name', 'id', 'notes'],
  platform_type: ['platform_type', 'proxy'],
  status_schedule: ['status', 'schedulable', 'scheduler_score'],
  capacity: ['capacity'],
  usage: ['usage', 'today_stats'],
  groups: ['groups', 'priority', 'rate_multiplier'],
  timestamps: ['last_used_at', 'created_at', 'expires_at'],
  actions: ['actions'],
}

// Usage roles share the same composite keys. Only the administrator had separate
// provider and upstream request columns in the earlier table.
const usageSources: ColumnSources = {
  time: ['created_at'],
  caller: ['user', 'api_key', 'group'],
  route: ['endpoint'],
  model_spec: ['model', 'reasoning_effort', 'stream', 'billing_mode'],
  tokens: ['tokens'],
  cost: ['cost'],
  performance: ['latency', 'tps'],
  request_trace: ['request_id'],
  client_network: ['ip_address', 'user_agent'],
}

// Each page keeps its original version marker for replaying the previous
// default-hidden adjustment before projecting legacy preferences onto the new layout.
const layouts = {
  providers: {
    hidden: 'provider-hidden-columns',
    order: 'admin-providers-column-order',
    mandatory: ['select', 'identity', 'actions'],
    sources: providerSources,
    legacyVersion: 'scheduler-score-hidden-by-default',
    legacyAddition: 'scheduler_score',
  },
  adminUsage: {
    hidden: 'usage-hidden-columns',
    order: 'admin-usage-column-order',
    mandatory: ['time', 'caller'],
    sources: {
      ...usageSources,
      route: ['provider', 'endpoint'],
      request_trace: ['request_id', 'upstream_request_id'],
    } as ColumnSources,
    legacyVersion: 'upstream-request-id-hidden-by-default',
    legacyAddition: 'upstream_request_id',
  },
  userUsage: {
    hidden: 'user-usage-hidden-columns',
    order: 'user-usage-column-order',
    mandatory: ['time'],
    sources: usageSources,
    legacyVersion: '',
    legacyAddition: '',
  },
}

// readKeys distinguishes a missing key from an existing empty or damaged value.
// This distinction prevents old preferences from overwriting a user's explicit reset.
function readKeys(key: string): string[] | null {
  const raw = localStorage.getItem(key)
  if (raw === null) return null
  try {
    const parsed: unknown = JSON.parse(raw)
    return Array.isArray(parsed)
      ? [...new Set(parsed.filter((k): k is string => typeof k === 'string'))]
      : []
  } catch {
    // A malformed destination takes the new default, independently of any intact
    // earlier profile. A malformed source also contributes no legacy fields.
    return []
  }
}

// useDenseTablePreferences initializes the page's preferences before the list controller
// consumes visibility. The table reads the returned order key when its desktop or mobile
// layout mounts. User changes then use the shared persistence and reordering mechanisms.
export function useDenseTablePreferences(page: DenseTablePage) {
  const layout = layouts[page]
  const entries = Object.entries(layout.sources)
  const keys = new Set(Object.keys(layout.sources))
  const mandatory = new Set(layout.mandatory)
  const storageKey = layout.hidden + '-dense-layout-v1'
  const columnOrderStorageKey = layout.order + '-v2'
  const prefs = usePersistedTableColumns({
    storageKey,
    defaultHiddenColumns: [],
    alwaysVisibleColumns: mandatory,
    getValidColumnKeys: () => keys,
  })

  try {
    const current = readKeys(storageKey)
    let hidden = current
    if (hidden === null) {
      const previous = readKeys(layout.hidden)
      const legacy = new Set(previous)
      // Older saved arrays predate one default-hidden field on each administrator page.
      // Missing profiles start with all nine columns visible.
      if (
        previous &&
        layout.legacyVersion &&
        localStorage.getItem(layout.hidden + '-version') !== layout.legacyVersion
      ) {
        legacy.add(layout.legacyAddition)
      }
      hidden = entries
        .filter(([, fields]) => fields.every((field) => legacy.has(field)))
        .map(([key]) => key)
    }
    for (const key of hidden) {
      if (keys.has(key) && !mandatory.has(key)) prefs.hiddenColumns.add(key)
    }
    // Publish the in-memory selection first so storage failures leave this visit usable.
    const normalized = [...prefs.hiddenColumns]
    if (JSON.stringify(normalized) !== localStorage.getItem(storageKey))
      prefs.saveColumnsToStorage()
  } catch {
    // Storage access can be denied in browser privacy modes. Empty visibility preferences
    // expose all columns and the shared toggle still updates the current page.
  }

  // Keep the migrated order available to the current mount even when storage is full.
  let initialColumnOrder: string[] = []
  try {
    const movable = [...keys].filter((key) => key !== 'select' && key !== 'actions')
    const current = readKeys(columnOrderStorageKey)
    const byField = new Map(
      entries.flatMap(([key, fields]) => fields.map((field) => [field, key] as const)),
    )
    const previous = current ?? (readKeys(layout.order) ?? []).map((key) => byField.get(key) ?? '')
    initialColumnOrder = [
      ...new Set([...previous.filter((key) => movable.includes(key)), ...movable]),
    ]
    const serialized = JSON.stringify(initialColumnOrder)
    // Write order under its own key so a failed visibility migration cannot
    // overwrite a working drag order on the following visit, or vice versa.
    if (serialized !== localStorage.getItem(columnOrderStorageKey))
      localStorage.setItem(columnOrderStorageKey, serialized)
  } catch {
    // The table independently supports in-memory dragging when storage rejects this
    // migration. An unwritten destination can be migrated on a later visit.
  }
  return {
    hiddenColumns: prefs.hiddenColumns,
    toggleColumn: prefs.toggleColumn,
    isColumnVisible: prefs.isColumnVisible,
    columnOrderStorageKey,
    initialColumnOrder,
  }
}
