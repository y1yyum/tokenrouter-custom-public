import { ref, reactive, onUnmounted } from 'vue'
import { useIntervalFn } from '@vueuse/core'
import { adminAPI } from '@/api/admin'
import { useTableLoader } from '@/composables/useTableLoader'
import { setPersistedPageSize } from '@/composables/usePersistedPageSize'
import { SEARCH_DEBOUNCE_MS } from '@/constants/ui'
import { supportsUpstreamUsageQuery } from '@/utils/upstreamUsage'
import { buildGrokUsageRefreshKey, buildOpenAIUsageRefreshKey } from '@/utils/providerUsageRefresh'
import type { Provider } from '@/api/admin/providers.types'

// useProviderList.ts loads provider table rows and schedules incremental
// refreshes with ETags. Initial loading requests a lightweight response, and
// later requests use the current filters, pagination, sorting, and
// scheduler-column visibility. The page supplies callbacks for usage hydration,
// selection clearing, and references held by open dialogs.
//
// Incremental refresh compares persisted and runtime fields before replacing a
// row, retaining unchanged row objects so their usage cells keep their state.
// Missing or changed rows notify the page before the resulting array is
// published. Refreshes pause during interactions and after local edits, while
// startup remains controlled by the page after its auxiliary metadata has
// loaded.

// ProviderSortOrder identifies the ascending or descending order requested for
// the table.
export type ProviderSortOrder = 'asc' | 'desc'
// ProviderAPIFilters derives the accepted filter fields from the administrator
// list endpoint.
type ProviderAPIFilters = NonNullable<Parameters<typeof adminAPI.providers.list>[2]>
// ProviderListParams keeps initialized table filters required while the
// first-load lite flag remains optional.
export type ProviderListParams = Required<Omit<ProviderAPIFilters, 'lite'>> &
  Pick<ProviderAPIFilters, 'lite'>
// ProviderListOptions connects list refreshes to the page-owned selection,
// usage, and dialog state.
interface ProviderListOptions {
  // includeSchedulerScore reads current column visibility immediately before a
  // list request. Return true when the page needs scheduler details so all
  // loading paths request the same fields.
  includeSchedulerScore: () => boolean
  // clearSelection invalidates selected IDs before a debounced filter reload is
  // scheduled. The callback also supersedes pending cross-page selection work
  // owned by the selection controller.
  clearSelection: () => void
  // onLoaded restores usage observations after accepting a list or 304.
  onLoaded: () => void
  // refreshTodayStats batches the displayed providers only when usage is visible.
  refreshTodayStats: () => Promise<void>
  // isSortKeyVisible applies the same composite-column visibility as the table.
  isSortKeyVisible: (key: string) => boolean
  // invalidateTodayStats retires metrics when a query changes or usage is hidden.
  invalidateTodayStats: () => void
  // onRowMissing invalidates observations for an upstream-capable row missing
  // from the next server page. It runs before reconciliation publishes the
  // resulting rows and may clear provider cache state.
  onRowMissing: (provider: Provider) => void
  // onRowReplaced receives the current and replacement row before publication. It
  // invalidates changed cache identities and updates references held by page
  // dialogs while both versions remain available.
  onRowReplaced: (current: Provider, next: Provider) => void
  // isRefreshPaused reports whether dialogs or menus currently prevent interval
  // refresh. The controller checks this after loading admission and before
  // decrementing its countdown or requesting another page.
  isRefreshPaused: () => boolean
}

// ProviderListRequest identifies the current ordinary, scheduled, or incremental request.
interface ProviderListRequest {
  // The ordinary loader resolves cancellation without a page or return status.
  receivedPage: boolean
}

// useProviderList owns provider rows, loading parameters, pagination, and
// incremental refresh scheduling. The page supplies callbacks for selection and
// usage state plus references held by dialogs. Call resumeAutoRefresh after the
// page's auxiliary data has settled; the interval starts paused.
//
// Ordinary loads reset ETag state, while incremental refreshes retain unchanged
// row objects and notify callers before replacing changed rows. Callback
// execution follows the row-reconciliation order, and refresh admission checks
// page interaction state together with loading and the post-edit silent window.
export function useProviderList(options: ProviderListOptions) {
  // PROVIDER_SORT_STORAGE_KEY identifies the shared browser setting read here and
  // written by the table sorting control.
  const PROVIDER_SORT_STORAGE_KEY = 'provider-table-sort'

  // ProviderSortState pairs the active table column key with its requested
  // direction.
  type ProviderSortState = {
    sort_by: string // Active column key.
    sort_order: ProviderSortOrder // Requested sort direction.
  }

  // defaultSort is also the table's fallback when a composite column is hidden.
  const defaultSort: ProviderSortState = { sort_by: 'name', sort_order: 'asc' }

  // saveSort keeps the table's persisted preference aligned with the API query.
  const saveSort = (state: ProviderSortState) => {
    try {
      localStorage.setItem(
        PROVIDER_SORT_STORAGE_KEY,
        JSON.stringify({ key: state.sort_by, order: state.sort_order }),
      )
    } catch {
      // Keep the accepted sort in memory when browser storage is unavailable.
    }
  }

  // loadInitialProviderSortState validates the legacy sort against migrated visibility
  // before the first list request or table render can use it.
  const loadInitialProviderSortState = (): ProviderSortState => {
    try {
      const raw = localStorage.getItem(PROVIDER_SORT_STORAGE_KEY)
      if (raw === null) return defaultSort

      const parsed = JSON.parse(raw)
      if (typeof parsed?.key === 'string' && options.isSortKeyVisible(parsed.key)) {
        return { sort_by: parsed.key, sort_order: parsed.order === 'desc' ? 'desc' : 'asc' }
      }
    } catch {
      // Malformed preferences use the same fallback as a hidden sort field.
    }
    saveSort(defaultSort)
    return defaultSort
  }

  // sortState supplies the currently accepted API sort.
  const sortState = reactive<ProviderSortState>({ ...loadInitialProviderSortState() })

  // AUTO_REFRESH_STORAGE_KEY and autoRefreshIntervals define the persisted
  // preference namespace and supported durations shown by the refresh control.
  const AUTO_REFRESH_STORAGE_KEY = 'provider-auto-refresh'
  const autoRefreshIntervals = [5, 10, 15, 30] as const

  // autoRefreshEnabled, autoRefreshIntervalSeconds, and autoRefreshCountdown
  // coordinate the saved preference with the active interval and its visible
  // countdown.
  const autoRefreshEnabled = ref(false)
  const autoRefreshIntervalSeconds = ref<(typeof autoRefreshIntervals)[number]>(30)
  const autoRefreshCountdown = ref(0)

  // autoRefreshETag belongs to the current page query and score projection.
  const autoRefreshETag = ref<string | null>(null)

  // AUTO_REFRESH_SILENT_WINDOW_MS and autoRefreshSilentUntil delay polling after
  // local edits so recently updated rows remain visible before the next
  // reconciliation.
  const AUTO_REFRESH_SILENT_WINDOW_MS = 15000
  const autoRefreshSilentUntil = ref(0)

  // hasPendingListSync marks local removals awaiting a full reload.
  const hasPendingListSync = ref(false)
  // Only the current request can publish rows or consume a deferred reveal. The
  // reveal follows replacements while usage stays visible and clears on hide.
  let activeListRequest: ProviderListRequest | null = null
  let statsRevealPending = false
  let firstListRequest = true
  let filterTimer: ReturnType<typeof setTimeout> | null = null
  let incrementalController: AbortController | null = null

  // loadSavedAutoRefresh restores the enabled flag and a supported interval from
  // browser storage. Invalid or unavailable storage leaves the initialized
  // defaults, while unsupported interval values leave the default interval in
  // place.
  const loadSavedAutoRefresh = () => {
    try {
      const saved = localStorage.getItem(AUTO_REFRESH_STORAGE_KEY)
      if (!saved) return
      const parsed = JSON.parse(saved) as {
        enabled?: boolean // Saved polling preference.
        interval_seconds?: number // Polling interval seconds.
      }
      autoRefreshEnabled.value = parsed.enabled === true
      const interval = Number(parsed.interval_seconds)
      if (autoRefreshIntervals.includes(interval as (typeof autoRefreshIntervals)[number])) {
        autoRefreshIntervalSeconds.value = interval as (typeof autoRefreshIntervals)[number]
      }
    } catch (e) {
      // Saved refresh preferences are optional; retain the initialized timer
      // settings when parsing or browser-storage access fails during setup.
      console.error('Failed to load saved auto refresh settings:', e)
    }
  }

  // saveAutoRefreshToStorage persists the current enabled flag and interval in
  // browser storage. Storage errors are logged while the active page keeps its
  // in-memory refresh settings and scheduling behavior.
  const saveAutoRefreshToStorage = () => {
    try {
      localStorage.setItem(
        AUTO_REFRESH_STORAGE_KEY,
        JSON.stringify({
          enabled: autoRefreshEnabled.value,
          interval_seconds: autoRefreshIntervalSeconds.value,
        }),
      )
    } catch (e) {
      // The active timer already follows the in-memory preference, so storage
      // failure affects persistence across visits rather than this interval.
      console.error('Failed to save auto refresh settings:', e)
    }
  }

  // setAutoRefreshEnabled stores the requested refresh preference and starts or
  // pauses the interval. Enabling resets the countdown to the selected duration;
  // disabling pauses the timer and clears the visible countdown.
  const setAutoRefreshEnabled = (enabled: boolean) => {
    autoRefreshEnabled.value = enabled
    saveAutoRefreshToStorage()
    if (enabled) {
      autoRefreshCountdown.value = autoRefreshIntervalSeconds.value
      resumeAutoRefresh()
    } else {
      pauseAutoRefresh()
      autoRefreshCountdown.value = 0
    }
  }

  // setAutoRefreshInterval stores one supported refresh duration for subsequent
  // ticks. An enabled timer restarts its countdown from that duration; a disabled
  // timer retains its current countdown until enabled.
  const setAutoRefreshInterval = (seconds: (typeof autoRefreshIntervals)[number]) => {
    autoRefreshIntervalSeconds.value = seconds
    saveAutoRefreshToStorage()
    if (autoRefreshEnabled.value) {
      autoRefreshCountdown.value = seconds
    }
  }

  // syncProviderListDerivedParams updates scheduler-score inclusion from current
  // column visibility. Every list-loading path calls it before requesting rows so
  // the server computes scores only when the page needs that column.
  const syncProviderListDerivedParams = () => {
    // Scheduler scores depend on current visibility, including when filters and sorting reuse the same request-parameter object.
    const requestParams = params
    requestParams.include_scheduler_score = options.includeSchedulerScore() ? '1' : '0'
  }

  // The shared loader still owns ordinary request cancellation and row publication.
  // Check the signal and request identity so even an abort-ignoring transport
  // cannot publish an obsolete page or its former projection.
  const { items: providers, loading, params, pagination, load: baseLoad } =
    useTableLoader<Provider, ProviderListParams>({
      fetchFn: async (page, size, requestParams, fetchOptions) => {
        const request = activeListRequest
        const filters = firstListRequest ? { ...requestParams, lite: '1' } : { ...requestParams }
        firstListRequest = false
        try {
          const result = await adminAPI.providers.list(page, size, filters, fetchOptions)
          if (!request || request !== activeListRequest || fetchOptions?.signal?.aborted) {
            throw new DOMException('Superseded provider list query', 'AbortError')
          }
          request.receivedPage = true
          return result
        } catch (error) {
          if (request !== activeListRequest || fetchOptions?.signal?.aborted) {
            throw new DOMException('Superseded provider list query', 'AbortError')
          }
          throw error
        }
      },
      initialParams: {
        platform: '',
        type: '',
        status: '',
        privacy_mode: '',
        group: '',
        search: '',
        include_scheduler_score: options.includeSchedulerScore() ? '1' : '0',
        sort_by: sortState.sort_by,
        sort_order: sortState.sort_order,
      },
    })

  // cancelFilterReload removes a pending search when an immediate action consumes its filters.
  const cancelFilterReload = () => {
    if (filterTimer !== null) clearTimeout(filterTimer)
    filterTimer = null
  }

  // invalidateQuery retires the previous projection, validator, and statistics.
  const invalidateQuery = () => {
    activeListRequest = null
    cancelFilterReload()
    autoRefreshETag.value = null
    incrementalController?.abort()
    incrementalController = null
    hasPendingListSync.value = false
    options.invalidateTodayStats()
  }

  // startListRequest reserves ownership before a debounce timer or transport starts.
  const startListRequest = () => {
    const request: ProviderListRequest = { receivedPage: false }
    activeListRequest = request
    return request
  }

  // runListRequest releases ownership once. A successful page refreshes stats as usual.
  // A failed page refreshes the retained rows only for a pending column reveal.
  const runListRequest = async (request: ProviderListRequest, fetchPage: () => Promise<boolean>) => {
    let accepted = false
    try {
      accepted = await fetchPage()
    } finally {
      if (activeListRequest === request) {
        activeListRequest = null
        const refreshStats = accepted || statsRevealPending
        statsRevealPending = false
        if (accepted) options.onLoaded()
        if (refreshStats) await options.refreshTodayStats()
      }
    }
  }

  // An ordinary loader rejection propagates through runListRequest. If the loader
  // absorbed cancellation, only a received page counts as a successful load.
  const loadRequestedPage = (request: ProviderListRequest) => {
    hasPendingListSync.value = false
    syncProviderListDerivedParams()
    return runListRequest(request, async () => {
      await baseLoad()
      return request.receivedPage
    })
  }

  // load consumes pending filters from the first page and accepts only the current
  // list completion for cache hydration and daily statistics.
  const load = () => {
    if (filterTimer !== null) pagination.page = 1
    invalidateQuery()
    return loadRequestedPage(startListRequest())
  }

  // reload applies filter and mutation changes from the first page.
  const reload = () => {
    pagination.page = 1
    return load()
  }

  // debouncedReload invalidates pending results as soon as filters change, then
  // waits for the final input before restarting pagination.
  const debouncedReload = () => {
    options.clearSelection()
    invalidateQuery()
    const request = startListRequest()
    filterTimer = setTimeout(() => {
      filterTimer = null
      pagination.page = 1
      void loadRequestedPage(request).catch((error) => console.error('Failed to reload providers:', error))
    }, SEARCH_DEBOUNCE_MS)
  }

  // handlePageChange preserves the current query and clamps the requested page.
  const handlePageChange = (page: number) => {
    pagination.page = Math.max(1, Math.min(page, pagination.pages || 1))
    return load()
  }

  // handlePageSizeChange saves the shared page-size preference before reloading.
  const handlePageSizeChange = (size: number) => {
    pagination.page_size = size
    setPersistedPageSize(size)
    return reload()
  }

  // handleSort changes the accepted sort only for a visible key. Ignoring
  // duplicate events lets the table reconcile its own fallback without a second request.
  const handleSort = (key: string, order: ProviderSortOrder) => {
    if (!options.isSortKeyVisible(key)) return
    if (sortState.sort_by === key && sortState.sort_order === order) return

    sortState.sort_by = key
    sortState.sort_order = order
    params.sort_by = key
    params.sort_order = order
    saveSort(sortState)
    return reload()
  }

  // reconcileColumnVisibility combines sort fallback and projection changes into
  // one request, preserving the current page for projection-only updates.
  const reconcileColumnVisibility = (key: string) => {
    if (!options.isSortKeyVisible(sortState.sort_by)) {
      return handleSort(defaultSort.sort_by, defaultSort.sort_order)
    }
    if (key === 'status_schedule') return load()
  }

  // setTodayStatsVisible coalesces repeated reveals while a list request is pending.
  const setTodayStatsVisible = (visible: boolean) => {
    statsRevealPending = visible && activeListRequest !== null
    if (!visible) options.invalidateTodayStats()
    else if (!statsRevealPending) void options.refreshTodayStats()
  }

  onUnmounted(() => {
    statsRevealPending = false
    invalidateQuery()
  })

  // enterAutoRefreshSilentWindow delays automatic refresh after a local provider
  // edit. It samples the current time, extends the silent deadline, and resets
  // the visible countdown to the configured interval.
  const enterAutoRefreshSilentWindow = () => {
    autoRefreshSilentUntil.value = Date.now() + AUTO_REFRESH_SILENT_WINDOW_MS
    autoRefreshCountdown.value = autoRefreshIntervalSeconds.value
  }

  // inAutoRefreshSilentWindow reports whether the post-edit refresh deadline is
  // still ahead.
  const inAutoRefreshSilentWindow = () => {
    return Date.now() < autoRefreshSilentUntil.value
  }

  // shouldReplaceAutoRefreshRow compares persisted configuration and runtime
  // observations that affect provider cells. It checks timestamps, scheduling
  // state, concurrency, cost, session counts, temporary restrictions, proxy
  // revision, and platform-specific usage-refresh identities. Upstream-capable
  // rows additionally compare connection settings and query configuration.
  //
  // A false result lets reconciliation retain the current row object and its
  // mounted usage-cell state. Array order is checked separately by the caller, so
  // an unchanged record can still move to its new server position without being
  // replaced.
  const shouldReplaceAutoRefreshRow = (current: Provider, next: Provider) => {
    const upstreamConnectionChanged =
      supportsUpstreamUsageQuery(current) || supportsUpstreamUsageQuery(next)
        ? current.type !== next.type ||
          current.platform !== next.platform ||
          current.proxy_id !== next.proxy_id ||
          current.proxy?.updated_at !== next.proxy?.updated_at ||
          current.credentials?.base_url !== next.credentials?.base_url ||
          JSON.stringify(current.extra?.upstream_usage_query ?? null) !==
            JSON.stringify(next.extra?.upstream_usage_query ?? null)
        : false
    return (
      current.updated_at !== next.updated_at ||
      current.current_concurrency !== next.current_concurrency ||
      current.current_window_cost !== next.current_window_cost ||
      current.active_sessions !== next.active_sessions ||
      current.schedulable !== next.schedulable ||
      current.status !== next.status ||
      current.rate_limit_reset_at !== next.rate_limit_reset_at ||
      current.overload_until !== next.overload_until ||
      current.temp_unschedulable_until !== next.temp_unschedulable_until ||
      current.proxy?.updated_at !== next.proxy?.updated_at ||
      JSON.stringify(current.scheduler_scores ?? null) !==
        JSON.stringify(next.scheduler_scores ?? null) ||
      JSON.stringify(current.scheduler_score ?? null) !==
        JSON.stringify(next.scheduler_score ?? null) ||
      current.last_used_at !== next.last_used_at ||
      current.current_rpm !== next.current_rpm ||
      current.quota_used !== next.quota_used ||
      current.quota_daily_used !== next.quota_daily_used ||
      current.quota_weekly_used !== next.quota_weekly_used ||
      current.quota_monthly_used !== next.quota_monthly_used ||
      upstreamConnectionChanged ||
      buildOpenAIUsageRefreshKey(current) !== buildOpenAIUsageRefreshKey(next) ||
      buildGrokUsageRefreshKey(current) !== buildGrokUsageRefreshKey(next)
    )
  }

  // mergeProvidersIncrementally reconciles a server page with the currently
  // displayed rows by provider ID. Missing upstream-capable rows notify the
  // caller first. New rows enter the result directly; changed rows notify the
  // caller before replacement, and unchanged rows retain their current object
  // identity.
  //
  // The list reference changes when the row count, a compared record, or ID order
  // differs. Reusing the existing array when all three remain stable avoids
  // unnecessary table updates, while callbacks keep cache invalidation and dialog
  // references synchronized with replacements.
  const mergeProvidersIncrementally = (nextRows: Provider[]) => {
    const currentRows = providers.value
    const currentByID = new Map(currentRows.map((row) => [row.id, row]))
    const nextIDs = new Set(nextRows.map((row) => row.id))
    for (const currentRow of currentRows) {
      if (supportsUpstreamUsageQuery(currentRow) && !nextIDs.has(currentRow.id)) {
        options.onRowMissing(currentRow)
      }
    }
    let changed = nextRows.length !== currentRows.length
    const mergedRows = nextRows.map((nextRow) => {
      const currentRow = currentByID.get(nextRow.id)
      if (!currentRow) {
        changed = true
        return nextRow
      }
      if (shouldReplaceAutoRefreshRow(currentRow, nextRow)) {
        // Notify the page before publishing the replacement array so cached query
        // observations and open-dialog references follow the accepted row
        // revision.
        options.onRowReplaced(currentRow, nextRow)
        changed = true
        return nextRow
      }
      return currentRow
    })
    if (!changed) {
      for (let i = 0; i < mergedRows.length; i += 1) {
        if (mergedRows[i].id !== currentRows[i]?.id) {
          changed = true
          break
        }
      }
    }
    // Publish a replacement array only after membership, compared values, or
    // order changes; unchanged rows keep their existing mounted-cell identity.
    if (changed) {
      providers.value = mergedRows
    }
  }

  // refreshProvidersIncrementally reconciles accepted responses and refreshes
  // daily statistics for both changed and 304 pages.
  const refreshProvidersIncrementally = async () => {
    if (incrementalController || activeListRequest || loading.value) return
    syncProviderListDerivedParams()
    const controller = new AbortController()
    incrementalController = controller
    const request = startListRequest()
    try {
      await runListRequest(request, async () => {
        const result = await adminAPI.providers.listWithEtag(
          pagination.page,
          pagination.page_size,
          { ...params },
          { etag: autoRefreshETag.value, signal: controller.signal },
        )
        if (activeListRequest !== request || controller.signal.aborted) return false
        if (result.etag) autoRefreshETag.value = result.etag

        if (!result.notModified && result.data) {
          pagination.total = result.data.total || 0
          pagination.pages = result.data.pages || 0
          mergeProvidersIncrementally(result.data.items || [])
          hasPendingListSync.value = false
        }
        return true
      })
    } catch (error) {
      if (!controller.signal.aborted) console.error('Auto refresh failed:', error)
    } finally {
      if (incrementalController === controller) incrementalController = null
    }
  }

  // The interval starts paused because the page resumes it after auxiliary
  // metadata resolves; VueUse owns interval cleanup when the scope is disposed.
  const { pause: pauseAutoRefresh, resume: resumeAutoRefresh } = useIntervalFn(
    async () => {
      if (!autoRefreshEnabled.value) return
      if (document.hidden) return
      if (activeListRequest || loading.value || incrementalController) return
      if (options.isRefreshPaused()) return
      if (inAutoRefreshSilentWindow()) {
        // Round remaining milliseconds up to seconds so the countdown includes
        // the unfinished final second of the silent window.
        autoRefreshCountdown.value = Math.max(
          0,
          Math.ceil((autoRefreshSilentUntil.value - Date.now()) / 1000),
        )
        return
      }

      if (autoRefreshCountdown.value <= 0) {
        autoRefreshCountdown.value = autoRefreshIntervalSeconds.value
        await refreshProvidersIncrementally()
        return
      }

      autoRefreshCountdown.value -= 1
    },
    1000,
    { immediate: false },
  )
  if (typeof window !== 'undefined') loadSavedAutoRefresh()
  return {
    PROVIDER_SORT_STORAGE_KEY,
    providers,
    loading,
    params,
    pagination,
    sortState,
    hasPendingListSync,
    autoRefreshIntervals,
    autoRefreshEnabled,
    autoRefreshIntervalSeconds,
    autoRefreshCountdown,
    reconcileColumnVisibility,
    setTodayStatsVisible,
    setAutoRefreshEnabled,
    setAutoRefreshInterval,
    load,
    reload,
    debouncedReload,
    handlePageChange,
    handlePageSizeChange,
    handleSort,
    enterAutoRefreshSilentWindow,
    pauseAutoRefresh,
    resumeAutoRefresh,
  }
}
