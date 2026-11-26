import { useProtocolCatalogFixture } from '@/__tests__/helpers/protocolCatalog'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import type { Column } from '@/components/common/table/types'
import { SEARCH_DEBOUNCE_MS } from '@/constants/ui'

import ProvidersView from '../ProvidersView.vue'

// ProvidersView.priorityColumn.spec.ts checks priority column visibility and
// sorting across saved preference versions. The table stub exposes column
// metadata and emits sorting events so the assertions connect browser storage
// decisions to provider list request parameters.

// listProviders records the sort and enrichment parameters produced by stored
// column preferences, while the remaining admin endpoints return minimal page
// metadata.
const { listProviders, getBatchTodayStats, listWithEtag } = vi.hoisted(() => ({
  listProviders: vi.fn(),
  getBatchTodayStats: vi.fn(),
  listWithEtag: vi.fn(),
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    providers: {
      list: listProviders,
      listWithEtag,
      getBatchTodayStats,
      getUpstreamBillingProbeSettings: vi
        .fn()
        .mockResolvedValue({ enabled: true, interval_minutes: 30 }),
      delete: vi.fn(),
      batchClearError: vi.fn(),
      batchRefresh: vi.fn(),
      toggleSchedulable: vi.fn(),
    },
    proxies: { getAll: vi.fn().mockResolvedValue([]) },
    groups: {
      getAll: vi.fn().mockResolvedValue([]),
      getAllIncludingInactive: vi.fn().mockResolvedValue([]),
    },
  },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn(), showInfo: vi.fn() }),
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ token: 'test-token' }),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

// DataTableStub exposes the column contracts and delegates a selected sort key to the view.
const DataTableStub = {
  props: ['columns', 'data', 'density'],
  emits: ['sort'],
  template: '<div><div v-for="row in data" :key="row.id" :data-id="row.id"><slot name="cell-usage" :row="row" /></div></div>',
}

// PaginationStub exposes the existing page-change event independently of layout.
const PaginationStub = { emits: ['update:page'], template: '<div />' }
const FiltersStub = { emits: ['update:searchQuery'], template: '<div />' }
const UsageStub = { props: ['todayStats', 'todayStatsError', 'todayStatsLoading'], template: '<div />' }
let wrapper: ReturnType<typeof mount>

// mountView renders provider preferences through a table stub exposing
// sortable columns. List mocks must be configured before mounting; callers
// await pending requests before inspecting columns or emitting a sort action.
function mountView() {
  wrapper = mount(ProvidersView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: {
          template:
            '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>',
        },
        DataTable: DataTableStub,
        ProviderTableActions: { template: '<div><slot name="after" /></div>' },
        ProviderTableFilters: FiltersStub,
        ProviderBulkActionsBar: true,
        Pagination: PaginationStub,
        ConfirmDialog: true,
        ProviderActionMenu: true,
        ImportDataModal: true,
        ReAuthProviderModal: true,
        ProviderTestModal: true,
        ProviderStatsModal: true,
        ScheduledTestsPanel: true,
        SyncFromCrsModal: true,
        TempUnschedStatusModal: true,
        ErrorPassthroughRulesModal: true,
        TLSFingerprintProfilesModal: true,
        CreateProviderModal: true,
        EditProviderModal: true,
        BulkEditProviderModal: true,
        PlatformTypeBadge: true,
        ProviderCapacityCell: true,
        ProviderStatusIndicator: true,
        ProviderTodayStatsCell: true,
        ProviderGroupsCell: true,
        ProviderUsageCell: UsageStub,
        HelpTooltip: true,
        Icon: true,
        Teleport: true,
      },
    },
  })
  return wrapper
}

// page fixtures allow the request assertions to distinguish two pages by ID.
function page(id?: number) {
  return { items: id == null ? [] : [{ id }], total: id == null ? 0 : 2, page: 1, page_size: 20, pages: 2 }
}

// setColumn toggles an existing view preference through the provider tools menu.
async function setColumn(key: string, visible: boolean) {
  const selector = '[data-toggle-column="' + key + '"]'
  if (!wrapper.find(selector).exists()) {
    await wrapper.get('button[title="admin.providers.moreActions"]').trigger('click')
  }
  const button = wrapper.get(selector)
  if (button.attributes('aria-pressed') !== String(visible)) await button.trigger('click')
}

// usePage advances through the same pagination event the provider page handles.
function usePage(number: number) {
  wrapper.getComponent(PaginationStub).vm.$emit('update:page', number)
}

// deferred leaves a request pending until its test releases the response.
function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

// mountWithUsageHidden keeps four rows visible while skipping the initial stats batch.
async function mountWithUsageHidden() {
  localStorage.setItem('provider-hidden-columns', JSON.stringify(['usage', 'today_stats']))
  listProviders.mockResolvedValueOnce({ ...page(1), items: [1, 2, 3, 4].map((id) => ({ id })) })
  mountView()
  await flushPromises()
}

// startHiddenUsageRefresh leaves one automatic list request pending over four visible rows.
async function startHiddenUsageRefresh() {
  vi.useFakeTimers()
  vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
  localStorage.setItem('provider-auto-refresh', JSON.stringify({ enabled: true, interval_seconds: 5 }))
  const pending = deferred<{ notModified: boolean; etag: string; data: ReturnType<typeof page> | null }>()
  listWithEtag.mockReturnValueOnce(pending.promise)
  await mountWithUsageHidden()
  await vi.advanceTimersByTimeAsync(6000)
  expect(listWithEtag).toHaveBeenCalledTimes(1)

  await setColumn('usage', true)
  expect(getBatchTodayStats).not.toHaveBeenCalled()
  return pending
}

describe('admin ProvidersView compact columns and requests', () => {
  beforeEach(() => {
    localStorage.clear()
    listProviders.mockReset().mockResolvedValue(page(1))
    getBatchTodayStats.mockReset().mockResolvedValue({ stats: {} })
    listWithEtag.mockReset().mockResolvedValue({ notModified: true, etag: null, data: null })
  })

  afterEach(() => {
    wrapper?.unmount()
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  // The page offers exactly nine composites and sends every advertised key to the server.
  it('exposes nine columns and their nine service-side sorts', async () => {
    mountView()
    await flushPromises()

    const table = wrapper.getComponent(DataTableStub)
    const columns = table.props('columns') as Column[]
    expect(table.props('density')).toBe('compact')
    expect(columns.map(({ key }) => key)).toEqual([
      'select', 'identity', 'platform_type', 'status_schedule', 'capacity',
      'usage', 'groups', 'timestamps', 'actions',
    ])
    expect(columns.reduce((width, column) => width + Number.parseInt(column.width ?? '', 10), 0)).toBe(1510)

    const sortKeys = columns.flatMap((column) => column.sortOptions?.map(({ key }) => key) ?? [])
    expect(sortKeys).toEqual([
      'name', 'id', 'status', 'schedulable', 'priority', 'rate_multiplier',
      'last_used_at', 'created_at', 'expires_at',
    ])
    expect(listProviders.mock.calls[0][2]).toEqual(expect.objectContaining({
      lite: '1', include_scheduler_score: '1', sort_by: 'name', sort_order: 'asc',
    }))
    for (const key of sortKeys) {
      table.vm.$emit('sort', key, 'desc')
      await flushPromises()
      expect(listProviders).toHaveBeenLastCalledWith(
        1, 20, expect.objectContaining({ sort_by: key, sort_order: 'desc' }),
        expect.objectContaining({ signal: expect.any(AbortSignal) }),
      )
      expect(listProviders.mock.lastCall?.[2]).not.toHaveProperty('lite')
    }
  })

  // Every old field must be hidden before a composite and its saved sort disappear.
  it.each([
    { legacy: ['priority', 'today_stats'], expectedHidden: false },
    { legacy: ['groups', 'priority', 'rate_multiplier'], expectedHidden: true },
  ])('migrates groups visibility before restoring the saved sort', async ({ legacy, expectedHidden }) => {
    localStorage.setItem('provider-hidden-columns', JSON.stringify(legacy))
    localStorage.setItem('provider-table-sort', JSON.stringify({ key: 'priority', order: 'desc' }))

    mountView()
    await flushPromises()
    const columns = wrapper.getComponent(DataTableStub).props('columns') as Column[]
    expect(columns.some(({ key }) => key === 'groups')).toBe(!expectedHidden)
    expect(JSON.parse(localStorage.getItem('provider-hidden-columns') || '[]')).toEqual(legacy)
    const sort = expectedHidden ? { key: 'name', order: 'asc' } : { key: 'priority', order: 'desc' }
    expect(JSON.parse(localStorage.getItem('provider-table-sort') || '{}')).toEqual(sort)
    expect(listProviders.mock.calls[0][2]).toMatchObject({ sort_by: sort.key, sort_order: sort.order })
  })

  // Hiding the active status sort changes page and projection in one request.
  it('falls back to name while turning off score projection in the same request', async () => {
    localStorage.setItem('provider-table-sort', JSON.stringify({ key: 'status', order: 'desc' }))
    mountView()
    await flushPromises()
    usePage(2)
    await flushPromises()

    await setColumn('status_schedule', false)
    await flushPromises()
    expect(listProviders.mock.calls.map(([pageNumber, , filters]) => [
      pageNumber, filters.sort_by, filters.include_scheduler_score,
    ])).toEqual([[1, 'status', '1'], [2, 'status', '1'], [1, 'name', '0']])
    expect(JSON.parse(localStorage.getItem('provider-table-sort') || '{}')).toEqual({
      key: 'name', order: 'asc',
    })

    usePage(2)
    await flushPromises()
    await setColumn('status_schedule', true)
    await flushPromises()
    expect(listProviders.mock.lastCall?.slice(0, 3)).toEqual([
      2, 20, expect.objectContaining({ sort_by: 'name', include_scheduler_score: '1' }),
    ])
  })

  // Old hidden status and scheduling fields, plus the legacy score default, hide
  // the entire composite before its first list request.
  it('restores score projection and sort from migrated visibility before first load', async () => {
    localStorage.setItem('provider-hidden-columns', JSON.stringify(['status', 'schedulable']))
    localStorage.setItem('provider-table-sort', JSON.stringify({ key: 'schedulable', order: 'desc' }))
    mountView()
    await flushPromises()
    expect(listProviders.mock.calls[0][2]).toMatchObject({
      lite: '1', sort_by: 'name', sort_order: 'asc', include_scheduler_score: '0',
    })
    expect(listProviders).toHaveBeenCalledTimes(1)
  })

  // A projection toggle during debounced input consumes the latest filters on
  // page one, without a second request when the original deadline passes.
  it('combines a pending search with the score projection refresh', async () => {
    vi.useFakeTimers()
    mountView()
    await flushPromises()
    usePage(2)
    await flushPromises()

    wrapper.getComponent(FiltersStub).vm.$emit('update:searchQuery', 'needle')
    await setColumn('status_schedule', false)
    await vi.advanceTimersByTimeAsync(SEARCH_DEBOUNCE_MS)
    expect(listProviders.mock.calls.map(([pageNumber, , filters]) => [
      pageNumber, filters.search, filters.include_scheduler_score,
    ])).toEqual([[1, '', '1'], [2, '', '1'], [1, 'needle', '0']])
    expect(getBatchTodayStats.mock.calls).toEqual([[[1]], [[1]], [[1]]])
  })

  // Showing usage during a page load waits for the accepted page and issues one batch.
  it('batches today only for the visible, accepted page', async () => {
    localStorage.setItem('provider-hidden-columns', JSON.stringify(['usage', 'today_stats']))
    mountView()
    await flushPromises()
    expect(getBatchTodayStats).not.toHaveBeenCalled()

    const next = deferred<ReturnType<typeof page>>()
    listProviders.mockReturnValueOnce(next.promise)
    usePage(2)
    await setColumn('usage', true)
    expect(getBatchTodayStats).not.toHaveBeenCalled()
    next.resolve(page(2))
    await flushPromises()
    expect(getBatchTodayStats.mock.calls).toEqual([[[2]]])

    await setColumn('usage', false)
    await setColumn('usage', true)
    await flushPromises()
    expect(getBatchTodayStats.mock.calls).toEqual([[[2]], [[2]]])

    listProviders.mockResolvedValueOnce(page())
    usePage(1)
    await flushPromises()
    expect(getBatchTodayStats).toHaveBeenCalledTimes(2)
  })

  // An ordinary projection request and a scheduled search both own a deferred reveal.
  it.each(['ordinary', 'debounced'])('restores a deferred reveal after the current %s list fails', async (kind) => {
    vi.useFakeTimers()
    vi.spyOn(console, 'error').mockImplementation(() => {})
    await mountWithUsageHidden()
    const pending = deferred<ReturnType<typeof page>>()
    listProviders.mockReturnValueOnce(pending.promise)

    if (kind === 'ordinary') await setColumn('status_schedule', false)
    else wrapper.getComponent(FiltersStub).vm.$emit('update:searchQuery', 'needle')
    await setColumn('usage', true)
    expect(getBatchTodayStats).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(SEARCH_DEBOUNCE_MS)
    expect(listProviders).toHaveBeenCalledTimes(2)

    pending.reject(new Error('HTTP 500'))
    await flushPromises()
    expect(getBatchTodayStats.mock.calls).toEqual([[[1, 2, 3, 4]]])
    expect(wrapper.findAll('[data-id]')).toHaveLength(4)
  })

  // New filters and repeated reveals leave one outstanding stats demand for the current list.
  it('carries a visible reveal to the latest query without letting the old request fulfill it', async () => {
    vi.useFakeTimers()
    vi.spyOn(console, 'error').mockImplementation(() => {})
    await mountWithUsageHidden()
    const previous = deferred<ReturnType<typeof page>>()
    const current = deferred<ReturnType<typeof page>>()
    listProviders.mockReturnValueOnce(previous.promise).mockReturnValueOnce(current.promise)

    await setColumn('status_schedule', false)
    await setColumn('usage', true)
    const filters = wrapper.getComponent(FiltersStub)
    filters.vm.$emit('update:searchQuery', 'first')
    await setColumn('usage', false)
    await setColumn('usage', true)
    filters.vm.$emit('update:searchQuery', 'last')
    previous.reject(new Error('obsolete HTTP 500'))
    await flushPromises()
    expect(getBatchTodayStats).not.toHaveBeenCalled()

    await vi.advanceTimersByTimeAsync(SEARCH_DEBOUNCE_MS)
    expect(listProviders).toHaveBeenCalledTimes(3)
    expect(listProviders.mock.lastCall?.[2].search).toBe('last')
    current.reject(new Error('current HTTP 500'))
    await flushPromises()
    expect(getBatchTodayStats.mock.calls).toEqual([[[1, 2, 3, 4]]])
  })

  // The shared loader resolves recognized cancellations without a successful page.
  it('does not refresh an already visible column after a swallowed cancellation', async () => {
    mountView()
    await flushPromises()
    expect(getBatchTodayStats.mock.calls).toEqual([[[1]]])

    listProviders.mockRejectedValueOnce(new DOMException('Cancelled', 'AbortError'))
    await setColumn('status_schedule', false)
    await flushPromises()
    expect(getBatchTodayStats.mock.calls).toEqual([[[1]]])
  })

  // A failed incremental list request keeps the existing page, while an accepted
  // response uses its current rows. Each visible completion needs exactly one batch.
  it.each([
    { outcome: 'HTTP 500', ids: [1, 2, 3, 4] },
    { outcome: '304', ids: [1, 2, 3, 4] },
    { outcome: '200', ids: [5] },
  ])('batches once after revealing usage during an incremental $outcome', async ({ outcome, ids }) => {
    const pending = await startHiddenUsageRefresh()
    if (outcome === 'HTTP 500') {
      vi.spyOn(console, 'error').mockImplementation(() => {})
      pending.reject(new Error('HTTP 500'))
    } else {
      pending.resolve({ notModified: outcome === '304', etag: 'current', data: outcome === '304' ? null : page(5) })
    }
    await flushPromises()
    expect(getBatchTodayStats.mock.calls).toEqual([[ids]])
    expect(wrapper.findAll('[data-id]').map((row) => Number(row.attributes('data-id')))).toEqual(ids)
  })

  // A reveal cancelled by hiding the column must leave the page without a stats request.
  it('skips the failure fallback if usage was hidden again', async () => {
    const pending = await startHiddenUsageRefresh()
    vi.spyOn(console, 'error').mockImplementation(() => {})
    await setColumn('usage', false)
    pending.reject(new Error('HTTP 500'))
    await flushPromises()
    expect(getBatchTodayStats).not.toHaveBeenCalled()
  })

  // A replaced incremental request has no ownership over the activating reveal.
  it('skips an obsolete failure while the replacement page is loading', async () => {
    const pending = await startHiddenUsageRefresh()
    const replacement = deferred<ReturnType<typeof page>>()
    listProviders.mockReturnValueOnce(replacement.promise)
    usePage(2)
    expect(listWithEtag.mock.calls[0][3].signal.aborted).toBe(true)
    pending.reject(new Error('stale HTTP 500'))
    await flushPromises()
    expect(getBatchTodayStats).not.toHaveBeenCalled()

    replacement.resolve(page(5))
    await flushPromises()
    expect(getBatchTodayStats.mock.calls).toEqual([[[5]]])
  })

  // Both success and failure from superseded statistics requests are discarded.
  it.each(['success', 'failure'])('ignores late statistics %s after a page change', async (outcome) => {
    const old = deferred<{ stats: Record<string, { requests: number }> }>()
    getBatchTodayStats.mockReturnValueOnce(old.promise)
    mountView()
    await flushPromises()
    expect(getBatchTodayStats).toHaveBeenCalledWith([1])

    const pending = deferred<{ stats: Record<string, { requests: number }> }>()
    listProviders.mockResolvedValueOnce(page(2))
    getBatchTodayStats.mockReturnValueOnce(pending.promise)
    usePage(2)
    await flushPromises()
    expect(wrapper.getComponent(UsageStub).props('todayStatsLoading')).toBe(true)

    if (outcome === 'success') old.resolve({ stats: { '1': { requests: 99 } } })
    else old.reject(new Error('superseded'))
    await flushPromises()
    expect(wrapper.getComponent(UsageStub).props()).toMatchObject({
      todayStats: null, todayStatsError: null, todayStatsLoading: true,
    })
    pending.resolve({ stats: { '2': { requests: 3 } } })
    await flushPromises()
    expect(wrapper.getComponent(UsageStub).props()).toMatchObject({
      todayStats: { requests: 3 }, todayStatsError: null, todayStatsLoading: false,
    })
  })

  // A transport that ignored cancellation cannot put old projected rows or an ETag back.
  it('discards a late incremental response after a projection change', async () => {
    vi.useFakeTimers()
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
    localStorage.setItem('provider-auto-refresh', JSON.stringify({ enabled: true, interval_seconds: 5 }))
    const old = deferred<{ notModified: boolean; etag: string; data: ReturnType<typeof page> }>()
    listWithEtag.mockReturnValueOnce(old.promise)
    mountView()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(6000)

    await setColumn('status_schedule', false)
    await flushPromises()
    expect(listWithEtag.mock.calls[0][3].signal.aborted).toBe(true)
    expect(listProviders.mock.lastCall?.[2].include_scheduler_score).toBe('0')

    old.resolve({ notModified: false, etag: 'obsolete', data: page(99) })
    await flushPromises()
    expect(wrapper.find('[data-id="99"]').exists()).toBe(false)
    expect(getBatchTodayStats.mock.calls).toEqual([[[1]], [[1]]])

    await wrapper.get('button[title="admin.providers.moreActions"]').trigger('click')
    await vi.advanceTimersByTimeAsync(6000)
    expect(listWithEtag.mock.lastCall?.[2].include_scheduler_score).toBe('0')
    expect(listWithEtag.mock.lastCall?.[3].etag).toBeNull()
    expect(getBatchTodayStats.mock.calls).toEqual([[[1]], [[1]], [[1]]])
  })
})

useProtocolCatalogFixture()
