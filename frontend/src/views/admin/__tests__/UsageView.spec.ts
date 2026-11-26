import type { Column } from '@/components/common/table/types'
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent, ref } from 'vue'

import UsageView from '../UsageView.vue'

// UsageView.spec.ts tests administrator usage queries, endpoint caching, route hydration,
// and background CSV export panel integration. API substitutes control response order and
// child controls expose page actions, while column preference cases retain the real page
// storage operations.

// Administrator API substitutes record list, summary, chart, user, error, and export-task
// queries; routeQuery supplies mutable initial navigation filters for hydration cases.
const {
  list,
  getStats,
  getSnapshotV2,
  getById,
  getModelStats,
  listErrorLogs,
  listExportTasks,
  routeQuery,
} = vi.hoisted(() => {
  vi.stubGlobal('localStorage', {
    getItem: vi.fn(() => null),
    setItem: vi.fn(),
    removeItem: vi.fn(),
  })

  return {
    list: vi.fn(),
    getStats: vi.fn(),
    getSnapshotV2: vi.fn(),
    getById: vi.fn(),
    getModelStats: vi.fn(),
    listErrorLogs: vi.fn(),
    listExportTasks: vi.fn().mockResolvedValue({
      items: [],
      total: 0,
      page: 1,
      page_size: 10,
      pages: 0,
    }),
    routeQuery: {} as Record<string, string>,
  }
})

// messages supplies administrator chart, table, and column-setting labels so tests locate
// controls and compare visible headings.
const messages: Record<string, string> = {
  'usage.tps': 'TPS',
  'admin.dashboard.timeRange': 'Time Range',
  'admin.dashboard.day': 'Day',
  'admin.dashboard.hour': 'Hour',
  'admin.usage.failedToLoadUser': 'Failed to load user',
  'admin.usage.requestId': 'Request ID',
  'admin.usage.upstreamRequestId': 'Upstream ID',
  'usage.requestedModel': 'Requested model',
  'usage.sentUpstreamModel': 'Sent upstream model',
  'usage.upstreamResponseModel': 'Upstream response model',
  'usage.upstreamModelMismatch': 'Upstream model mismatch',
  'common.yes': 'Yes',
  'common.no': 'No',
}

// formatLocalDate formats fixture dates using browser-local calendar components for
// expected query values.
const formatLocalDate = (date: Date): string => {
  const year = date.getFullYear()
  const month = String(date.getMonth() + 1).padStart(2, '0')
  const day = String(date.getDate()).padStart(2, '0')
  return `${year}-${month}-${day}`
}

vi.mock('@/api/admin', () => ({
  adminAPI: {
    usage: {
      list,
      getStats,
    },
    dashboard: {
      getSnapshotV2,
      getModelStats,
    },
    users: {
      getById,
    },
  },
}))

// Minute history reads are recorded independently from native list and export requests.
const { getImportedCalls } = vi.hoisted(() => ({ getImportedCalls: vi.fn() }))
vi.mock('@/api/admin/usage', () => ({
  getImportedCalls,
  adminUsageAPI: {
    listExportTasks,
  },
}))

vi.mock('@/api/admin/ops', () => ({
  listErrorLogs,
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showWarning: vi.fn(),
    showSuccess: vi.fn(),
    showInfo: vi.fn(),
  }),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => messages[key] ?? key,
    }),
  }
})

vi.mock('vue-router', () => ({
  useRoute: () => ({
    query: routeQuery,
  }),
}))

// AppLayoutStub renders the administrator page slot so page-loading tests can inspect
// sections independently of the surrounding application layout.
const AppLayoutStub = { template: '<div><slot /></div>' }
// UsageFiltersStub exposes label revision, label setter, and filter summary methods used by
// route hydration and background export tests, with a settings slot for column controls.
const UsageFiltersStub = defineComponent({
  props: {
    activeExportCount: {
      type: Number,
      default: 0,
    },
  },
  emits: ['change', 'refresh', 'reset', 'cleanup', 'export'],
  // setup exposes a mutable user label and revision-aware setter for route-hydration tests.
  setup(_, { expose }) {
    const userLabel = ref('')
    let userLabelRevision = 0
    // setUserLabel advances the fixture label revision before publishing the requested
    // label. This lets hydration tests simulate a later selection even when the numeric
    // user filter remains unchanged.
    const setUserLabel = (email: string) => {
      userLabelRevision += 1
      userLabel.value = email
    }
    const getFilterSummaryLabels = () =>
      userLabel.value ? [{ key: 'user_id', label: 'User', value: userLabel.value }] : []
    expose({
      getUserLabelRevision: () => userLabelRevision,
      setUserLabel,
      getFilterSummaryLabels,
    })
    return { userLabel }
  },
  template:
    '<div><span data-test="user-filter-label">{{ userLabel }}</span><span data-test="active-export-count">{{ activeExportCount }}</span><slot name="after-reset" /></div>',
})
// UsageExportDialogStub captures inherited date, filter, and chip props passed by admin UsageView.
const UsageExportDialogStub = {
  name: 'UsageExportDialog',
  props: [
    'show', // modal visibility flag
    'mode', // user or administrator mode
    'startDate', // inherited inclusive start date
    'endDate', // inherited inclusive end date
    'filters', // inherited filter object
    'filterLabels', // human-readable filter summary chips
  ],
  emits: ['close', 'tasksChanged', 'activeIdsChanged'],
  template: '<div data-test="usage-export-dialog" />',
}
// UsageTableStub accepts visible column definitions so preference tests observe the
// exact columns passed by the administrator page.
const UsageTableStub = {
  props: [
    'columns', // visible table column definitions
    'columnOrderStorageKey', // compact drag order preference
  ],
  emits: ['userClick', 'sort'],
  template:
    '<div data-test="usage-table"><button class="user-click" @click="$emit(\'userClick\', 2)">user</button></div>',
}
// UserTokenRankingStub exposes a row-selection event so the page can exercise lazy
// mounting and ranking drill-down through the rendered button.
const UserTokenRankingStub = {
  emits: ['select-user'],
  template:
    '<div data-test="ranking"><button class="pick-user" @click="$emit(\'select-user\', 5, \'rank@test.com\')">pick</button></div>',
}
// ModelDistributionChartStub exposes the selected metric and emits changes so tests
// inspect independent model-chart display choices.
const ModelDistributionChartStub = {
  props: [
    'metric', // selected chart measurement key
  ],
  emits: ['update:metric'],
  template: `
    <div data-test="model-chart">
      <span class="metric">{{ metric }}</span>
      <button class="switch-metric" @click="$emit('update:metric', 'actual_cost')">switch</button>
    </div>
  `,
}
// GroupDistributionChartStub exposes the metric so tests compare group-chart
// selection with the independent model chart while using the same snapshot.
const GroupDistributionChartStub = {
  props: [
    'metric', // selected chart measurement key
  ],
  emits: ['update:metric'],
  template: `
    <div data-test="group-chart">
      <span class="metric">{{ metric }}</span>
      <button class="switch-metric" @click="$emit('update:metric', 'actual_cost')">switch</button>
    </div>
  `,
}
// EndpointDistributionChartStub marks the endpoint card without drawing a canvas,
// keeping page-level loader and layout tests independent of Chart.js rendering.
const EndpointDistributionChartStub = {
  template: '<div data-test="endpoint-chart" />',
}

// mountRouteFilteredUsageView renders the administrator page with controlled child
// components and the route-label search stub. Tests retain the real page loaders while
// recording query parameters and observing filter-label changes.
const mountRouteFilteredUsageView = (attachTo?: HTMLElement) =>
  mount(UsageView, {
    attachTo,
    global: {
      stubs: {
        AppLayout: AppLayoutStub,
        UsageStatsCards: true,
        UsageFilters: UsageFiltersStub,
        UsageTable: true,
        UsageExportDialog: UsageExportDialogStub,
        UsageCleanupDialog: true,
        UserBalanceHistoryModal: true,
        Pagination: true,
        Select: true,
        DateRangePicker: true,
        Icon: true,
        TokenUsageTrend: true,
        ModelDistributionChart: true,
        GroupDistributionChart: true,
        EndpointDistributionChart: true,
        UserTokenRanking: true,
      },
    },
  })

describe('admin UsageView 路由筛选', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    Object.keys(routeQuery).forEach((key) => delete routeQuery[key])
    list.mockReset().mockResolvedValue({ items: [], total: 0, pages: 0 })
    listExportTasks.mockReset().mockResolvedValue({
      items: [],
      total: 0,
      page: 1,
      page_size: 10,
      pages: 0,
    })
    getStats.mockReset().mockResolvedValue({
      total_requests: 0,
      total_input_tokens: 0,
      total_output_tokens: 0,
      total_cache_tokens: 0,
      total_tokens: 0,
      total_cost: 0,
      total_actual_cost: 0,
      average_duration_ms: 0,
    })
    getSnapshotV2.mockReset().mockResolvedValue({ trend: [], models: [], groups: [] })
    getModelStats.mockReset().mockResolvedValue({ models: [] })
    getById.mockReset()
  })

  // Clear the shared route query before restoring the clock so later page mounts cannot
  // inherit another hydration scenario's user or date filters.
  afterEach(() => {
    Object.keys(routeQuery).forEach((key) => delete routeQuery[key])
    vi.useRealTimers()
  })

  // A route-scoped local report remains mounted while the operator inspects an archive.
  // Editing the history dates and model must leave local queries and action filters intact.
  it('切换 raks 历史保留本站日期、筛选及导出和清理的范围', async () => {
    Object.assign(routeQuery, { user_id: '7', start_date: '2026-11-25', end_date: '2026-11-26' })
    getById.mockResolvedValue({ id: 7, email: 'seven@example.test' })
    getImportedCalls
      .mockReset()
      .mockResolvedValue({ source: 'raks', groups: [], items: [], total: 0 })
    const wrapper = mountRouteFilteredUsageView(document.body)
    await flushPromises()
    const localReads = list.mock.calls.length
    const nativeFilters = wrapper.getComponent(UsageFiltersStub)
    expect(getImportedCalls).not.toHaveBeenCalled()
    const [local, history] = wrapper.findAll('button[aria-pressed]')
    await history.trigger('click')
    await flushPromises()
    expect(nativeFilters.isVisible()).toBe(false)
    expect(getImportedCalls.mock.lastCall?.[0]).not.toHaveProperty('start_date')
    const panel = wrapper.getComponent({ name: 'ImportedCallsPanel' })
    await panel.get('button[aria-pressed="false"]').trigger('click')
    await flushPromises()
    expect(getImportedCalls.mock.lastCall?.[0]).toMatchObject({
      view: 'minutes',
      start_date: '2026-11-25',
      end_date: '2026-11-26',
    })
    panel.getComponent({ name: 'DateRangePicker' }).vm.$emit('update:endDate', '2026-11-27')
    await panel.get('input[type="search"]').setValue('history-model')
    await panel.get('form').trigger('submit')
    await flushPromises()
    expect(getImportedCalls.mock.lastCall?.[0]).toMatchObject({
      model: 'history-model',
      end_date: '2026-11-27',
    })
    await local.trigger('click')
    expect(nativeFilters.isVisible()).toBe(true)
    expect(nativeFilters.text()).toContain('seven@example.test')
    for (const name of ['UsageExportDialog', 'UsageCleanupDialog']) {
      const dialog = wrapper.getComponent({ name })
      expect(dialog.props('filters')).toMatchObject({
        user_id: 7,
        start_date: '2026-11-25',
        end_date: '2026-11-26',
      })
      expect(dialog.props('filters').model).toBeUndefined()
    }
    expect(list).toHaveBeenCalledTimes(localReads)
    wrapper.unmount()
  })

  // Endpoint selection changes while an earlier raw-source request is pending. The test
  // protects cancellation, source-specific queries, cache reuse, and rejection of a
  // superseded chart result.
  it('端点图按需加载，切换时取消旧请求且复用已加载结果', async () => {
    const wrapper = mountRouteFilteredUsageView()
    await flushPromises()
    expect(getStats).toHaveBeenCalledTimes(1)
    expect(getStats.mock.calls[0][0].endpoint_source).toBe('inbound')
    const chart = wrapper.findComponent({ name: 'EndpointDistributionChart' })
    chart.vm.$emit('update:source', 'upstream')
    await flushPromises()
    expect(getStats.mock.calls[1][0].endpoint_source).toBe('upstream')
    let resolvePath!: (value: any) => void
    getStats.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolvePath = resolve
        }),
    )
    chart.vm.$emit('update:source', 'path')
    await flushPromises()
    expect(getStats.mock.calls[2][0].endpoint_source).toBe('path')
    const signal = getStats.mock.calls[2][1].signal as AbortSignal
    chart.vm.$emit('update:source', 'upstream')
    await flushPromises()
    expect(signal.aborted).toBe(true)
    expect(getStats).toHaveBeenCalledTimes(3)
    resolvePath({ endpoint_paths: [{ endpoint: 'stale', requests: 999 }] })
    await flushPromises()
    expect(chart.props('endpointPathStats')).toEqual([])
    wrapper.unmount()
  })

  // Filters change while a raw endpoint chart is selected. The test protects independent
  // summary and raw-source loading and canceling the obsolete endpoint request during the
  // refresh.
  it('筛选变更后重新加载当前端点图，摘要独立更新', async () => {
    const wrapper = mountRouteFilteredUsageView()
    await flushPromises()
    const chart = wrapper.findComponent({ name: 'EndpointDistributionChart' })
    chart.vm.$emit('update:source', 'upstream')
    await flushPromises()
    getStats.mockClear()
    getStats.mockImplementation((params) =>
      params.endpoint_source === 'upstream'
        ? new Promise(() => {})
        : Promise.resolve({ total_requests: 17, endpoints: [] }),
    )
    wrapper.findComponent(UsageFiltersStub).vm.$emit('change')
    await flushPromises()
    expect(getStats.mock.calls.map((call) => call[0].endpoint_source)).toEqual([
      'upstream',
      'inbound',
    ])
    expect(wrapper.findComponent({ name: 'UsageStatsCards' }).props('stats').total_requests).toBe(
      17,
    )
    expect(chart.props('loading')).toBe(true)
    const rawCall = getStats.mock.calls.find((call) => call[0].endpoint_source === 'upstream')!
    wrapper.unmount()
    expect(rawCall[1].signal.aborted).toBe(true)
  })

  // Emitting export from UsageFilters snapshots human-readable filter labels from the
  // filter bar before opening UsageExportDialog. The dialog receives administrator mode,
  // current page dates, and the active user filter payload.
  it('opens UsageExportDialog in admin mode with inherited dates and resolved filter chips', async () => {
    routeQuery.user_id = '42'
    getById.mockResolvedValue({ id: 42, email: 'route-user@test.com' })

    const wrapper = mountRouteFilteredUsageView()
    await flushPromises()

    const dialog = wrapper.findComponent(UsageExportDialogStub)
    expect(dialog.props('show')).toBe(false)

    wrapper.findComponent(UsageFiltersStub).vm.$emit('export')
    await flushPromises()

    expect(dialog.props('show')).toBe(true)
    expect(dialog.props('mode')).toBe('admin')
    expect(dialog.props('filters')).toMatchObject({ user_id: 42 })
    expect(dialog.props('filterLabels')).toEqual([
      { key: 'user_id', label: 'User', value: 'route-user@test.com' },
    ])
    wrapper.unmount()
  })

  // Mounting the admin Usage view with an active running export populates UsageFilters'
  // activeExportCount badge immediately. Emitting tasksChanged from UsageExportDialog
  // updates the forwarded badge count in sync.
  it('forwards active background export counts to UsageFilters', async () => {
    listExportTasks.mockResolvedValueOnce({
      items: [{ id: 88, status: 'running' }],
      total: 1,
      page: 1,
      page_size: 10,
      pages: 1,
    })

    const wrapper = mountRouteFilteredUsageView()
    await flushPromises()

    expect(wrapper.find('[data-test="active-export-count"]').text()).toBe('1')

    wrapper.findComponent(UsageExportDialogStub).vm.$emit('tasksChanged', 0)
    await flushPromises()
    expect(wrapper.find('[data-test="active-export-count"]').text()).toBe('0')
    wrapper.unmount()
  })

  // The route supplies a user identifier before the usage page mounts. The test protects
  // using that predicate in list queries and resolving the corresponding readable filter
  // label.
  it('应用 user_id 请求筛选时回显路由用户', async () => {
    routeQuery.user_id = '42'
    getById.mockResolvedValue({ id: 42, email: 'route-user@test.com' })

    const wrapper = mountRouteFilteredUsageView()
    await flushPromises()

    expect(getById).toHaveBeenCalledWith(42, true)
    expect(list).toHaveBeenCalledWith(expect.objectContaining({ user_id: 42 }), expect.anything())
    expect(wrapper.find('[data-test="user-filter-label"]').text()).toBe('route-user@test.com')
  })

  // The selected user changes before route-label retrieval completes. The test protects
  // retaining the newer user label when the earlier user response eventually arrives
  // successfully.
  it('user_id 已变化时忽略过期的路由用户标签', async () => {
    routeQuery.user_id = '42'
    let resolveLookup!: (user: {
      id: number // record numeric identifier
      email: string // user email display text
    }) => void
    getById.mockReturnValue(
      new Promise((resolve) => {
        resolveLookup = resolve
      }),
    )

    const wrapper = mountRouteFilteredUsageView()
    await wrapper.vm.$nextTick()
    ;(wrapper.vm as any).filters.user_id = 84
    ;(wrapper.findComponent(UsageFiltersStub).vm as any).setUserLabel('current-user@test.com')

    resolveLookup({ id: 42, email: 'stale-user@test.com' })
    await flushPromises()

    expect(wrapper.find('[data-test="user-filter-label"]').text()).toBe('current-user@test.com')
  })

  // The same user is selected again after route-label retrieval starts. The test protects
  // the newer selected label from a delayed successful hydration response.
  it('路由用户查询成功时不覆盖之后选定的标签', async () => {
    routeQuery.user_id = '42'
    let resolveLookup!: (user: {
      id: number // record numeric identifier
      email: string // user email display text
    }) => void
    getById.mockReturnValue(
      new Promise((resolve) => {
        resolveLookup = resolve
      }),
    )

    const wrapper = mountRouteFilteredUsageView()
    await wrapper.vm.$nextTick()
    ;(wrapper.findComponent(UsageFiltersStub).vm as any).setUserLabel('picked@test.com')

    resolveLookup({ id: 42, email: 'route-user@test.com' })
    await flushPromises()

    expect((wrapper.vm as any).filters.user_id).toBe(42)
    expect(wrapper.find('[data-test="user-filter-label"]').text()).toBe('picked@test.com')
  })

  // The same user is selected again after route-label retrieval starts and that lookup
  // fails. The test protects the selected label from replacement with numeric identity.
  it('路由用户查询失败时不覆盖之后选定的标签', async () => {
    routeQuery.user_id = '42'
    let rejectLookup!: (error: Error) => void
    getById.mockReturnValue(
      new Promise((_, reject) => {
        rejectLookup = reject
      }),
    )

    const wrapper = mountRouteFilteredUsageView()
    await wrapper.vm.$nextTick()
    ;(wrapper.findComponent(UsageFiltersStub).vm as any).setUserLabel('picked@test.com')

    rejectLookup(new Error('lookup failed'))
    await flushPromises()

    expect((wrapper.vm as any).filters.user_id).toBe(42)
    expect(wrapper.find('[data-test="user-filter-label"]').text()).toBe('picked@test.com')
  })

  // Route-label retrieval fails while its user selection remains current. The test protects
  // keeping the user predicate and displaying the numeric identifier as the readable
  // fallback.
  it('路由用户查询失败时回显用户 ID', async () => {
    routeQuery.user_id = '42'
    getById.mockRejectedValue(new Error('lookup failed'))

    const wrapper = mountRouteFilteredUsageView()
    await flushPromises()

    expect(list).toHaveBeenCalledWith(expect.objectContaining({ user_id: 42 }), expect.anything())
    expect(wrapper.find('[data-test="user-filter-label"]').text()).toBe('42')
  })
})

describe('admin UsageView distribution metric toggles', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    list.mockReset()
    getStats.mockReset()
    getSnapshotV2.mockReset()
    getById.mockReset()
    getModelStats.mockReset()

    list.mockResolvedValue({
      items: [],
      total: 0,
      pages: 0,
    })
    getStats.mockResolvedValue({
      total_requests: 0,
      total_input_tokens: 0,
      total_output_tokens: 0,
      total_cache_tokens: 0,
      total_tokens: 0,
      total_cost: 0,
      total_actual_cost: 0,
      average_duration_ms: 0,
    })
    getSnapshotV2.mockResolvedValue({
      trend: [],
      models: [],
      groups: [],
    })
    getModelStats.mockResolvedValue({ models: [] })
  })

  // Restore the real clock after each fake-timer scenario so later cases do not inherit
  // its pending debounce or refresh schedule.
  afterEach(() => {
    vi.useRealTimers()
  })

  // A model-statistics refresh remains pending after earlier rows were displayed. The test
  // protects retaining those rows during refresh and replacing them when the new response
  // arrives.
  it('keeps previous model stats visible during refresh until new data arrives', async () => {
    // The initial response supplies model A before the delayed refresh begins.
    getModelStats.mockResolvedValueOnce({ models: [{ model: 'A', total_tokens: 10 }] })

    const wrapper = mount(UsageView, {
      global: {
        stubs: {
          AppLayout: AppLayoutStub,
          UsageStatsCards: true,
          UsageFilters: UsageFiltersStub,
          UsageTable: true,
          UsageExportDialog: UsageExportDialogStub,
          UsageCleanupDialog: true,
          UserBalanceHistoryModal: true,
          AuditLogModal: true,
          Pagination: true,
          Select: true,
          DateRangePicker: true,
          Icon: true,
          TokenUsageTrend: true,
          ModelDistributionChart: ModelDistributionChartStub,
          GroupDistributionChart: GroupDistributionChartStub,
          EndpointDistributionChart: true,
          UserTokenRanking: true,
        },
      },
    })
    vi.advanceTimersByTime(120)
    await flushPromises()
    expect((wrapper.vm as any).requestedModelStats).toEqual([{ model: 'A', total_tokens: 10 }])

    // resolveSecond supplies the replaceable resolver for a pending model-statistics
    // refresh.
    let resolveSecond: (v: any) => void = () => {}
    getModelStats.mockReturnValueOnce(
      new Promise((res) => {
        resolveSecond = res
      }),
    )
    ;(wrapper.vm as any).refreshData()
    await flushPromises()
    expect((wrapper.vm as any).requestedModelStats).toEqual([{ model: 'A', total_tokens: 10 }])

    // The completed refresh replaces the displayed model with B.
    resolveSecond({ models: [{ model: 'B', total_tokens: 20 }] })
    await flushPromises()
    expect((wrapper.vm as any).requestedModelStats).toEqual([{ model: 'B', total_tokens: 20 }])
  })

  // The usage page renders both filters and a records table. The test protects separate
  // card sections so the filter container does not also own the table.
  it('renders filters and the usage table in separate sections', async () => {
    const wrapper = mount(UsageView, {
      global: {
        stubs: {
          AppLayout: AppLayoutStub,
          UsageStatsCards: true,
          UsageFilters: UsageFiltersStub,
          UsageTable: UsageTableStub,
          UsageExportDialog: UsageExportDialogStub,
          UsageCleanupDialog: true,
          UserBalanceHistoryModal: true,
          Pagination: true,
          Select: true,
          DateRangePicker: true,
          Icon: true,
          TokenUsageTrend: true,
          ModelDistributionChart: true,
          GroupDistributionChart: true,
          EndpointDistributionChart: true,
          UserTokenRanking: true,
        },
      },
    })
    vi.advanceTimersByTime(120)
    await flushPromises()

    const filtersCard = wrapper.get('[data-testid="admin-usage-filters-card"]')
    const tableSection = wrapper.get('[data-testid="admin-usage-table-section"]')
    expect(filtersCard.classes()).toContain('card')
    expect(filtersCard.find('[data-test="usage-table"]').exists()).toBe(false)
    expect(tableSection.find('[data-test="usage-table"]').exists()).toBe(true)
  })

  // Model and group charts change their display metric independently. The test protects
  // their separate selections and reuse of existing snapshot data
  // without another fetch.
  it('keeps model and group metric toggles independent without refetching chart data', async () => {
    const wrapper = mount(UsageView, {
      global: {
        stubs: {
          AppLayout: AppLayoutStub,
          UsageStatsCards: true,
          UsageFilters: UsageFiltersStub,
          UsageTable: true,
          UsageExportDialog: UsageExportDialogStub,
          UsageCleanupDialog: true,
          UserBalanceHistoryModal: true,
          Pagination: true,
          Select: true,
          DateRangePicker: true,
          Icon: true,
          TokenUsageTrend: true,
          ModelDistributionChart: ModelDistributionChartStub,
          GroupDistributionChart: GroupDistributionChartStub,
          EndpointDistributionChart: EndpointDistributionChartStub,
          UserTokenRanking: true,
        },
      },
    })

    vi.advanceTimersByTime(120)
    await flushPromises()

    expect(getSnapshotV2).toHaveBeenCalledTimes(1)
    const now = new Date()
    const yesterday = new Date(now.getTime() - 24 * 60 * 60 * 1000)
    expect(getSnapshotV2).toHaveBeenCalledWith(
      expect.objectContaining({
        start_date: formatLocalDate(yesterday),
        end_date: formatLocalDate(now),
        granularity: 'hour',
      }),
    )

    const modelChart = wrapper.find('[data-test="model-chart"]')
    const groupChart = wrapper.find('[data-test="group-chart"]')

    expect(modelChart.find('.metric').text()).toBe('tokens')
    expect(groupChart.find('.metric').text()).toBe('tokens')

    await modelChart.find('.switch-metric').trigger('click')
    await flushPromises()

    expect(modelChart.find('.metric').text()).toBe('actual_cost')
    expect(groupChart.find('.metric').text()).toBe('tokens')
    expect(getSnapshotV2).toHaveBeenCalledTimes(1)

    await groupChart.find('.switch-metric').trigger('click')
    await flushPromises()

    expect(modelChart.find('.metric').text()).toBe('actual_cost')
    expect(groupChart.find('.metric').text()).toBe('actual_cost')
    expect(getSnapshotV2).toHaveBeenCalledTimes(1)
  })
})

describe('admin UsageView compact columns', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.mocked(localStorage.getItem).mockReset().mockReturnValue(null)
    vi.mocked(localStorage.setItem).mockReset()
    list.mockReset().mockResolvedValue({ items: [], total: 0, pages: 0 })
    listExportTasks.mockReset().mockResolvedValue({
      items: [],
      total: 0,
      page: 1,
      page_size: 10,
      pages: 0,
    })
    getStats.mockReset().mockResolvedValue({
      total_requests: 0,
      total_input_tokens: 0,
      total_output_tokens: 0,
      total_cache_tokens: 0,
      total_tokens: 0,
      total_cost: 0,
      total_actual_cost: 0,
      average_duration_ms: 0,
    })
    getSnapshotV2.mockReset().mockResolvedValue({ trend: [], models: [], groups: [] })
    getModelStats.mockReset().mockResolvedValue({ models: [] })
  })

  // Restore the real clock after each fake-timer scenario so later cases do not inherit
  // its pending debounce or refresh schedule.
  afterEach(() => {
    vi.useRealTimers()
  })

  // mountColumnView renders administrator usage with the column-aware table stub. Storage
  // substitutes and the real settings actions remain active so tests can compare visible
  // definitions across preference changes.
  const mountColumnView = () =>
    mount(UsageView, {
      global: {
        stubs: {
          AppLayout: AppLayoutStub,
          UsageStatsCards: true,
          UsageFilters: UsageFiltersStub,
          UsageTable: UsageTableStub,
          UsageExportDialog: UsageExportDialogStub,
          UsageCleanupDialog: true,
          UserBalanceHistoryModal: true,
          AuditLogModal: true,
          Pagination: true,
          Select: true,
          DateRangePicker: true,
          Icon: true,
          TokenUsageTrend: true,
          ModelDistributionChart: true,
          GroupDistributionChart: true,
          EndpointDistributionChart: true,
          UserTokenRanking: true,
        },
      },
    })

  // A new administrator gets all nine columns, with real service keys on the two
  // sortable headers and the baseline width expected by the desktop table.
  it('exposes nine columns and binds time and model to the server sort keys', async () => {
    const wrapper = mountColumnView()
    await flushPromises()

    const table = wrapper.findComponent(UsageTableStub)
    const columns = table.props('columns') as Column[]
    expect(columns.map(({ key }) => key)).toEqual([
      'time',
      'caller',
      'route',
      'model_spec',
      'tokens',
      'cost',
      'performance',
      'request_trace',
      'client_network',
    ])
    expect(columns.flatMap(({ sortOptions }) => sortOptions?.map(({ key }) => key) ?? [])).toEqual([
      'created_at',
      'model',
    ])
    const model = columns.find(({ key }) => key === 'model_spec')
    expect(model?.sortOptions?.[0]?.label).toBe(model?.label)
    expect(columns.reduce((width, column) => width + Number.parseInt(column.width!, 10), 0)).toBe(
      1510,
    )
    expect(table.props('columnOrderStorageKey')).toBe('admin-usage-column-order-v2')
    wrapper.unmount()
  })

  // Older preferences hide the whole route, only IP, and the local request ID. The
  // administrator's previous migration already hid upstream IDs for this profile.
  it('restores fully hidden groups and preserves the source preferences', async () => {
    const hidden = JSON.stringify([
      'request_id',
      'ip_address',
      'created_at',
      'provider',
      'endpoint',
    ])
    const order = JSON.stringify(['model', 'created_at', 'reasoning_effort', 'provider'])
    vi.mocked(localStorage.getItem).mockImplementation((key) => {
      if (key === 'usage-hidden-columns') return hidden
      if (key === 'admin-usage-column-order') return order
      return null
    })

    const wrapper = mountColumnView()
    await flushPromises()
    const columns = wrapper.findComponent(UsageTableStub).props('columns') as Column[]
    expect(columns.map(({ key }) => key)).toEqual([
      'time',
      'caller',
      'model_spec',
      'tokens',
      'cost',
      'performance',
      'client_network',
    ])
    expect(localStorage.setItem).toHaveBeenCalledWith(
      'usage-hidden-columns-dense-layout-v1',
      JSON.stringify(['route', 'request_trace']),
    )
    expect(localStorage.setItem).toHaveBeenCalledWith(
      'admin-usage-column-order-v2',
      JSON.stringify([
        'model_spec',
        'time',
        'route',
        'caller',
        'tokens',
        'cost',
        'performance',
        'request_trace',
        'client_network',
      ]),
    )
    expect(localStorage.setItem).not.toHaveBeenCalledWith('usage-hidden-columns', expect.anything())
    wrapper.unmount()
  })

  // An explicit visibility change survives remount and uses the composite key.
  it('persists hiding the performance column', async () => {
    const saved = new Map<string, string>()
    vi.mocked(localStorage.getItem).mockImplementation((key) => saved.get(key) ?? null)
    vi.mocked(localStorage.setItem).mockImplementation((key, value) => {
      saved.set(key, value)
    })

    const wrapper = mountColumnView()
    await wrapper.get('button[title="admin.users.columnSettings"]').trigger('click')
    await wrapper
      .findAll('button')
      .find((button) => button.text() === 'usage.compactTable.performance')!
      .trigger('click')
    expect(saved.get('usage-hidden-columns-dense-layout-v1')).toBe(JSON.stringify(['performance']))
    wrapper.unmount()
    const restored = mountColumnView()
    const columns = restored.findComponent(UsageTableStub).props('columns') as Column[]
    expect(columns.map(({ key }) => key)).not.toContain('performance')
    restored.unmount()
  })

  // Hiding an unrelated column retains the order; hiding the active model column
  // replaces it with descending time once, even if DataTable publishes the fallback.
  it('reloads page one once when the active sort column is hidden', async () => {
    const wrapper = mountColumnView()
    await flushPromises()
    const table = wrapper.findComponent(UsageTableStub)
    table.vm.$emit('sort', 'model', 'asc')
    await flushPromises()
    expect(list).toHaveBeenLastCalledWith(
      expect.objectContaining({ page: 1, sort_by: 'model', sort_order: 'asc' }),
      expect.anything(),
    )
    list.mockClear()

    await wrapper.get('button[title="admin.users.columnSettings"]').trigger('click')
    await wrapper
      .findAll('button')
      .find((button) => button.text() === 'usage.compactTable.performance')!
      .trigger('click')
    expect(list).not.toHaveBeenCalled()

    const model = wrapper
      .findAll('button')
      .find((button) => button.text() === 'usage.compactTable.modelSpec')!
    await model.trigger('click')
    table.vm.$emit('sort', 'created_at', 'desc')
    await flushPromises()
    expect(list).toHaveBeenCalledOnce()
    expect(list).toHaveBeenLastCalledWith(
      expect.objectContaining({ page: 1, sort_by: 'created_at', sort_order: 'desc' }),
      expect.anything(),
    )
    await model.trigger('click')
    expect(list).toHaveBeenCalledOnce()
    wrapper.unmount()
  })
})

describe('admin UsageView handleUserClick', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    list.mockReset()
    getStats.mockReset()
    getSnapshotV2.mockReset()
    getById.mockReset()

    list.mockResolvedValue({ items: [], total: 0, pages: 0 })
    getStats.mockResolvedValue({
      total_requests: 0,
      total_input_tokens: 0,
      total_output_tokens: 0,
      total_cache_tokens: 0,
      total_tokens: 0,
      total_cost: 0,
      total_actual_cost: 0,
      average_duration_ms: 0,
    })
    getSnapshotV2.mockResolvedValue({ trend: [], models: [], groups: [] })
  })

  // Restore the real clock after each fake-timer scenario so later cases do not inherit
  // its pending debounce or refresh schedule.
  afterEach(() => {
    vi.useRealTimers()
  })

  // A usage row refers to a user who may have been deleted. The test protects requesting
  // the user with include_deleted when the row emits its navigation event.
  it('opens user via include_deleted when clicking a usage row user', async () => {
    getById.mockResolvedValue({ id: 2, email: 'd@test.com', deleted_at: '2026-05-28T00:00:00Z' })

    const wrapper = mount(UsageView, {
      global: {
        stubs: {
          AppLayout: AppLayoutStub,
          UsageStatsCards: true,
          UsageFilters: UsageFiltersStub,
          UsageTable: UsageTableStub,
          UsageExportDialog: UsageExportDialogStub,
          UsageCleanupDialog: true,
          UserBalanceHistoryModal: true,
          AuditLogModal: true,
          Pagination: true,
          Select: true,
          DateRangePicker: true,
          Icon: true,
          TokenUsageTrend: true,
          ModelDistributionChart: true,
          GroupDistributionChart: true,
          EndpointDistributionChart: true,
          UserTokenRanking: true,
        },
      },
    })

    vi.advanceTimersByTime(120)
    await flushPromises()

    await wrapper.find('[data-test="usage-table"] .user-click').trigger('click')
    await flushPromises()

    expect(getById).toHaveBeenCalledWith(2, true)
  })
})

describe('admin UsageView errors tab filter forwarding', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    list.mockReset()
    getStats.mockReset()
    getSnapshotV2.mockReset()
    getModelStats.mockReset()
    listErrorLogs.mockReset()

    list.mockResolvedValue({ items: [], total: 0, pages: 0 })
    getStats.mockResolvedValue({
      total_requests: 0,
      total_input_tokens: 0,
      total_output_tokens: 0,
      total_cache_tokens: 0,
      total_tokens: 0,
      total_cost: 0,
      total_actual_cost: 0,
      average_duration_ms: 0,
    })
    getSnapshotV2.mockResolvedValue({ trend: [], models: [], groups: [] })
    getModelStats.mockResolvedValue({ models: [] })
    listErrorLogs.mockResolvedValue({ items: [], total: 0, pages: 0 })
  })

  // Restore the real clock after each fake-timer scenario so later cases do not inherit
  // its pending debounce or refresh schedule.
  afterEach(() => {
    vi.useRealTimers()
  })

  // Shared model, provider, and group filters are set before opening errors. The test
  // protects forwarding those supported predicates to the administrator error-list query
  // for that tab.
  it('forwards model/provider_id/group_id to listErrorLogs on the errors tab', async () => {
    const wrapper = mount(UsageView, {
      global: {
        stubs: {
          AppLayout: AppLayoutStub,
          UsageStatsCards: true,
          UsageFilters: UsageFiltersStub,
          UsageTable: true,
          UsageExportDialog: UsageExportDialogStub,
          UsageCleanupDialog: true,
          UserBalanceHistoryModal: true,
          AuditLogModal: true,
          Pagination: true,
          Select: true,
          DateRangePicker: true,
          Icon: true,
          TokenUsageTrend: true,
          ModelDistributionChart: true,
          GroupDistributionChart: true,
          EndpointDistributionChart: true,
          UserTokenRanking: true,
          OpsErrorLogTable: true,
          OpsErrorDetailModal: true,
        },
      },
    })
    vi.advanceTimersByTime(120)
    await flushPromises()

    // Set the model, provider, and group filters before opening the error tab.
    const vm = wrapper.vm as any
    vm.filters.model = 'gpt-5.3-codex'
    vm.filters.provider_id = 7
    vm.filters.group_id = 3
    await flushPromises()

    // Opening the second tab triggers its error loader using the current shared filter values.
    const tabs = wrapper.findAll('[data-testid="usage-detail-tab"]')
    await tabs[1].trigger('click')
    await flushPromises()

    expect(listErrorLogs).toHaveBeenCalledWith(
      expect.objectContaining({
        view: 'all',
        model: 'gpt-5.3-codex',
        provider_id: 7,
        group_id: 3,
      }),
    )
  })
})

describe('admin UsageView ranking tab', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    list.mockReset()
    getStats.mockReset()
    getSnapshotV2.mockReset()
    getModelStats.mockReset()

    list.mockResolvedValue({ items: [], total: 0, pages: 0 })
    getStats.mockResolvedValue({
      total_requests: 0,
      total_input_tokens: 0,
      total_output_tokens: 0,
      total_cache_tokens: 0,
      total_tokens: 0,
      total_cost: 0,
      total_actual_cost: 0,
      average_duration_ms: 0,
    })
    getSnapshotV2.mockResolvedValue({ trend: [], models: [], groups: [] })
    getModelStats.mockResolvedValue({ models: [] })
  })

  // Restore the real clock after each fake-timer scenario so later cases do not inherit
  // its pending debounce or refresh schedule.
  afterEach(() => {
    vi.useRealTimers()
  })

  // The ranking tab is unopened until the user selects it. The test protects lazy mounting
  // and row drill-down updating user filters before returning to usage records.
  it('mounts ranking lazily and drill-down sets user filter then jumps back to usage tab', async () => {
    const wrapper = mount(UsageView, {
      global: {
        stubs: {
          AppLayout: AppLayoutStub,
          UsageStatsCards: true,
          UsageFilters: UsageFiltersStub,
          UsageTable: true,
          UsageExportDialog: UsageExportDialogStub,
          UsageCleanupDialog: true,
          UserBalanceHistoryModal: true,
          Pagination: true,
          Select: true,
          DateRangePicker: true,
          Icon: true,
          TokenUsageTrend: true,
          ModelDistributionChart: true,
          GroupDistributionChart: true,
          EndpointDistributionChart: true,
          UserTokenRanking: UserTokenRankingStub,
          OpsErrorLogTable: true,
          OpsErrorDetailModal: true,
        },
      },
    })
    vi.advanceTimersByTime(120)
    await flushPromises()

    // The ranking component stays unmounted before the ranking tab is selected.
    expect(wrapper.find('[data-test="ranking"]').exists()).toBe(false)

    const tabs = wrapper.findAll('[data-testid="usage-detail-tab"]')
    expect(tabs).toHaveLength(3)
    await tabs[2].trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-test="ranking"]').exists()).toBe(true)

    // Drill-down updates user identity, returns to usage records, and retrieves the newly filtered list.
    list.mockClear()
    await wrapper.find('[data-test="ranking"] .pick-user').trigger('click')
    await flushPromises()

    expect((wrapper.vm as any).activeTab).toBe('usage')
    expect((wrapper.vm as any).filters.user_id).toBe(5)
    expect(list).toHaveBeenCalledWith(expect.objectContaining({ user_id: 5 }), expect.anything())
  })
})
