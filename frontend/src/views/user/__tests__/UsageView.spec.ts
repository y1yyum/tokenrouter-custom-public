import type { Column } from '@/components/common/table/types'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'

import UsageView from '../UsageView.vue'

// UsageView.spec.ts tests user usage filters, error visibility, team scope, and background
// CSV export panel integration. API substitutes expose the parameters sent by real page
// loaders. Model options and pagination use the page's real controls. Table and export
// stubs expose the selected records, column permissions, filter chips, and export state.

enableAutoUnmount(afterEach)

// User and team API substitutes record queries and control membership, public error
// visibility, translated options, chart data, background export tasks, and notifications.
const {
  query,
  getStats,
  getDashboardModels,
  getDashboardSnapshotV2,
  listMyErrorRequests,
  listExportTasks,
  publicSettings,
  list,
  getAvailable,
  getCurrentTeam,
  getTeamKeys,
  getTeamMembers,
  getTeamMemberUsage,
  showError,
  showWarning,
  showSuccess,
  showInfo,
} = vi.hoisted(() => ({
  query: vi.fn(),
  getStats: vi.fn(),
  getDashboardModels: vi.fn(),
  getDashboardSnapshotV2: vi.fn(),
  listMyErrorRequests: vi.fn(),
  listExportTasks: vi.fn(),
  publicSettings: { allow_user_view_error_requests: undefined as boolean | undefined },
  list: vi.fn(),
  getAvailable: vi.fn(),
  getCurrentTeam: vi.fn(),
  getTeamKeys: vi.fn(),
  getTeamMembers: vi.fn(),
  getTeamMemberUsage: vi.fn(),
  showError: vi.fn(),
  showWarning: vi.fn(),
  showSuccess: vi.fn(),
  showInfo: vi.fn(),
}))

// messages supplies user CSV headers and column-setting labels so export assertions
// compare the complete user-facing field sequence.
const messages: Record<string, string> = {
  'usage.time': 'Time',
  'usage.compactTable.caller.teamOwner': 'Member / Group',
  'usage.compactTable.caller.personal': 'Key / Group',
  'usage.compactTable.performance': 'Latency / TPS',
  'usage.compactTable.modelSpec': 'Model / Type',
  'usage.reasoningEffort': 'Reasoning Effort',
  'usage.inboundEndpoint': 'Inbound Endpoint',
  'admin.usage.ipAddress': 'IP Address',
  'admin.usage.inputTokens': 'Input Tokens',
  'admin.usage.outputTokens': 'Output Tokens',
  'admin.usage.cacheReadTokens': 'Cache Read Tokens',
  'admin.usage.cacheCreationTokens': 'Cache Creation Tokens',
  'usage.rate': 'Rate Multiplier',
  'usage.userBilled': 'Billed Cost',
  'usage.original': 'Original Cost',
  'usage.firstToken': 'TTFT (ms)',
  'usage.ttftMode': 'TTFT mode',
  'usage.latencyFirstToken': 'First output',
  'usage.duration': 'Duration (ms)',
  'usage.tps': 'TPS',
  'usage.tpsExport': 'TPS (tok/s)',

  'admin.dashboard.timeRange': 'Time range',
  'admin.dashboard.granularity': 'Granularity',
  'admin.dashboard.day': 'Day',
  'admin.dashboard.hour': 'Hour',
  'admin.users.columnSettings': 'Columns',
  'admin.usage.group': 'Group',
  'admin.usage.billingType': 'Billing type',
  'admin.usage.billingMode': 'Billing mode',
  'admin.usage.allTypes': 'All types',
  'admin.usage.allBillingTypes': 'All billing types',
  'admin.usage.billingTypeBalance': 'Balance',
  'admin.usage.billingTypeSubscription': 'Subscription',
  'admin.usage.allBillingModes': 'All billing modes',
  'admin.usage.billingModeToken': 'Token',
  'admin.usage.billingModePerRequest': 'Per request',
  'admin.usage.billingModeImage': 'Image',
  'admin.usage.allGroups': 'All groups',
  'admin.usage.allModels': 'All models',
  'usage.allApiKeys': 'All API Keys',
  'usage.apiKeyFilter': 'API Key',
  'usage.model': 'Model',
  'usage.type': 'Type',
  'usage.ws': 'WS',
  'usage.stream': 'Stream',
  'usage.sync': 'Sync',
  'usage.exportCsv': 'Export',
  'usage.compactionFilter': 'Compaction',
  'usage.nativeCompactionV2': 'Native compaction',
  'usage.legacyCompaction': 'Legacy compaction',
  'requests.id': 'Request ID',
  'usage.failedToLoad': 'Failed to load',
  'common.refresh': 'Refresh',
  'common.reset': 'Reset',
}

vi.mock('@/api', () => ({
  usageAPI: {
    query,
    getStats,
    getDashboardModels,
    getDashboardSnapshotV2,
    listMyErrorRequests,
    listExportTasks,
  },
  keysAPI: {
    list,
  },
  userGroupsAPI: {
    getAvailable,
  },
}))

vi.mock('@/api/usage', () => ({
  usageAPI: {
    query,
    getStats,
    getDashboardModels,
    getDashboardSnapshotV2,
    listMyErrorRequests,
    listExportTasks,
  },
}))

vi.mock('@/api/team', () => ({
  teamAPI: {
    current: getCurrentTeam,
    keys: getTeamKeys,
    members: getTeamMembers,
    memberUsage: getTeamMemberUsage,
  },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError,
    showWarning,
    showSuccess,
    showInfo,
    cachedPublicSettings: publicSettings,
  }),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) => {
        if (key === 'usage.exportPanel.activeCountLabel') {
          return `Export (${params?.count ?? 0})`
        }
        return messages[key] ?? key
      },
    }),
  }
})

// simpleStub exposes the child slots or props exercised by this suite while leaving the
// owning component logic active.
const simpleStub = { template: '<div><slot /></div>' }
// chartStub omits chart rendering while leaving the usage page loaders active
// for API request and filter assertions.
const chartStub = { template: '<div />' }
// UsageTableStub accepts visible columns and member presentation flags so tests inspect
// user permissions and team attribution configuration.
const UsageTableStub = {
  props: [
    'data', // records on the selected usage page
    'columns', // visible table column definitions
    'userClickable', // administrator navigation permission flag
    'compactRole', // administrator, team owner, or personal presentation
    'columnOrderStorageKey', // compact drag order preference
    'showProviderBilling', // visibility for provider charges
    'showStandardCost', // visibility for standard charges
  ],
  emits: ['sort'],
  template:
    '<div data-test="usage-table"><span v-for="row in data" :key="row.id">{{ row.request_id }}</span></div>',
}
// UsageExportDialogStub captures inherited date, filter, and chip props and exposes events
// used to drive active background export badge transitions.
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

// usageLog supplies representative usage measurements reused by rendering cases, with each
// test overriding the fields relevant to its scenario.
const usageLog = {
  id: 1,
  request_id: 'req-user-export',
  actual_cost: 0.092883,
  total_cost: 0.092883,
  rate_multiplier: 1,
  service_tier: 'priority',
  input_cost: 0.020285,
  output_cost: 0.00303,
  cache_creation_cost: 0.000001,
  cache_read_cost: 0.069568,
  input_tokens: 4057,
  output_tokens: 101,
  cache_creation_tokens: 4,
  cache_read_tokens: 278272,
  cache_creation_5m_tokens: 0,
  cache_creation_1h_tokens: 0,
  image_count: 0,
  image_size: null,
  ttft_ms: 12,
  ttft_mode: 'first_output',
  tps: 303.3,
  duration_ms: 345,
  created_at: '2026-03-08T00:00:00Z',
  model: 'gpt-5.4',
  reasoning_effort: null,
  ip_address: '203.0.113.10',
  api_key: { name: 'demo-key' },
  billing_mode: 'token',
  request_type: 'sync',
  stream: false,
}

// mountUsageView renders the user page with controlled child widgets and API substitutes.
// The actual page loaders, preference actions, permission checks, and export panel wiring
// stay active for these assertions.
function mountUsageView() {
  return mount(UsageView, {
    global: {
      stubs: {
        AppLayout: simpleStub,
        Select: true,
        DateRangePicker: true,
        Icon: true,
        UsageStatsCards: true,
        UsageTable: UsageTableStub,
        UsageExportDialog: UsageExportDialogStub,
        ModelDistributionChart: chartStub,
        GroupDistributionChart: chartStub,
        EndpointDistributionChart: chartStub,
        TokenUsageTrend: chartStub,
        TeamMemberUsageCharts: chartStub,
        UserErrorRequestsTable: true,
      },
    },
  })
}

describe('user UsageView', () => {
  beforeEach(() => {
    localStorage.clear()
    query.mockReset()
    getStats.mockReset()
    getDashboardModels.mockReset()
    getDashboardSnapshotV2.mockReset()
    listMyErrorRequests.mockReset().mockResolvedValue({ items: [], total: 0, pages: 0 })
    listExportTasks.mockReset().mockResolvedValue({
      items: [],
      total: 0,
      page: 1,
      page_size: 10,
      pages: 0,
    })
    publicSettings.allow_user_view_error_requests = undefined
    list.mockReset()
    getAvailable.mockReset()
    getCurrentTeam.mockReset()
    getTeamKeys.mockReset()
    getTeamMembers.mockReset()
    getTeamMemberUsage.mockReset()
    showError.mockReset()
    showWarning.mockReset()
    showSuccess.mockReset()
    showInfo.mockReset()

    query.mockResolvedValue({ items: [usageLog], total: 1, pages: 1 })
    getStats.mockResolvedValue({
      total_requests: 1,
      total_input_tokens: 10,
      total_output_tokens: 20,
      total_cache_tokens: 0,
      total_tokens: 30,
      total_cost: 0.1,
      total_actual_cost: 0.08,
      average_duration_ms: 12,
      endpoints: [],
      upstream_endpoints: [],
      endpoint_paths: [],
    })
    getDashboardModels.mockResolvedValue({
      models: [
        {
          model: 'gpt-5.4',
          requests: 1,
          input_tokens: 10,
          output_tokens: 20,
          cache_creation_tokens: 0,
          cache_read_tokens: 0,
          total_tokens: 30,
          cost: 0.1,
          actual_cost: 0.08,
        },
      ],
      start_date: '2026-03-08',
      end_date: '2026-03-08',
    })
    getDashboardSnapshotV2.mockResolvedValue({
      generated_at: '2026-03-08T00:00:00Z',
      start_date: '2026-03-08',
      end_date: '2026-03-08',
      granularity: 'hour',
      trend: [],
      groups: [],
    })
    list.mockResolvedValue({ items: [{ id: 1, name: 'demo-key' }] })
    getAvailable.mockResolvedValue([{ id: 1, name: 'default' }])
    getCurrentTeam.mockRejectedValue({ response: { status: 404 } })
    getTeamKeys.mockResolvedValue([])
    getTeamMembers.mockResolvedValue([])
    getTeamMemberUsage.mockResolvedValue([])
  })

  // The usage page mounts with personal scope and no selected filters. The test protects
  // initial record and statistics requests together with personal key and group lookups.
  it('loads logs, stats, model stats, and snapshot on first render', async () => {
    mountUsageView()
    await flushPromises()

    expect(query).toHaveBeenCalled()
    expect(getStats).toHaveBeenCalled()
    expect(getDashboardModels).toHaveBeenCalled()
    expect(getDashboardSnapshotV2).toHaveBeenCalledWith(
      expect.objectContaining({
        include_trend: true,
        include_model_stats: false,
        include_group_stats: true,
      }),
    )
    expect(list).toHaveBeenCalledWith(1, 100, { scope: 'personal' })
    expect(getAvailable).toHaveBeenCalledWith('personal', undefined, false)
  })

  // A second page fails after the first page has rendered. Retrying must read
  // the selected page and keep the summary for the entire date range.
  it('retries a failed page without showing rows from the previous page', async () => {
    query.mockResolvedValueOnce({ items: [usageLog], total: 51 })
    const wrapper = mountUsageView()
    await flushPromises()
    expect(wrapper.get('[data-test="usage-table"]').text()).toBe(usageLog.request_id)

    query.mockRejectedValueOnce(new Error('unavailable'))
    wrapper.getComponent({ name: 'Pagination' }).vm.$emit('update:page', 2)
    await flushPromises()
    expect(wrapper.find('[data-test="usage-table"]').exists()).toBe(false)
    expect(wrapper.findComponent({ name: 'Pagination' }).exists()).toBe(false)
    const failure = wrapper.get('[role="alert"]')
    expect(failure.text()).toContain('Failed to load')

    query.mockResolvedValueOnce({
      items: [{ ...usageLog, id: 2, request_id: 'page-two' }],
      total: 51,
    })
    await failure.get('button').trigger('click')
    await flushPromises()
    expect(query.mock.lastCall?.[0]).toMatchObject({ page: 2 })
    expect(wrapper.get('[data-test="usage-table"]').text()).toBe('page-two')
    expect(getStats).toHaveBeenCalledTimes(1)
  })

  // A user changes the model from page two while the new first page is slow.
  // Neither a fast summary nor a late refresh of the previous model can reopen
  // pagination before the new list supplies its count.
  it.each([
    { action: 'select', previous: null, next: 'f04-alpha', total: 22 },
    { action: 'switch', previous: 'f04-alpha', next: 'f04-beta', total: 1 },
    { action: 'clear', previous: 'f04-alpha', next: null, total: 23 },
  ])(
    'hides stale pagination on model $action until page one arrives',
    async ({ action, previous, next, total }) => {
      getDashboardModels.mockResolvedValue({
        models: [{ model: 'f04-alpha' }, { model: 'f04-beta' }],
      })
      const alphaLog = { ...usageLog, model: 'f04-alpha', request_id: 'alpha-first' }
      const betaLog = { ...usageLog, id: 2, model: 'f04-beta', request_id: 'beta-first' }
      query.mockResolvedValue({ items: [alphaLog, betaLog], total: 23 })
      const wrapper = mountUsageView()
      await flushPromises()

      const filterButton = wrapper.get('button[aria-label="common.filter"]')
      const modelField = wrapper
        .findAllComponents({ name: 'FilterField' })
        .find((field) => field.props('label') === 'Model')!
      if (previous) {
        await filterButton.trigger('click')
        await modelField.get('button[aria-controls]').trigger('click')
        query.mockResolvedValueOnce({ items: [alphaLog], total: 22 })
        await modelField.get(`[role="option"][data-value="${previous}"]`).trigger('click')
        await flushPromises()
        await filterButton.trigger('click')
      }

      const oldResult = {
        items: [{ ...alphaLog, request_id: 'old-page-two' }],
        total: previous ? 22 : 23,
      }
      query.mockResolvedValueOnce(oldResult)
      await wrapper.get('[aria-label="pagination.next"]').trigger('click')
      await flushPromises()
      expect(query.mock.lastCall?.[0]).toMatchObject({ page: 2, model: previous ?? undefined })
      expect(wrapper.get('[data-test="usage-table"]').text()).toBe('old-page-two')
      const oldPage = Promise.withResolvers<unknown>()
      if (action === 'switch') {
        query.mockReturnValueOnce(oldPage.promise)
        await wrapper.get('button[title="Refresh"]').trigger('click')
      }

      await filterButton.trigger('click')
      if (!previous) await modelField.get('button[aria-controls]').trigger('click')
      const firstPage = Promise.withResolvers<unknown>()
      query.mockReturnValueOnce(firstPage.promise)
      getStats.mockResolvedValueOnce({ total_requests: total })
      await modelField.get(`[role="option"][data-value="${next ?? previous}"]`).trigger('click')
      await flushPromises()

      expect(query.mock.lastCall?.[0]).toMatchObject({ page: 1, model: next ?? undefined })
      expect(getStats.mock.lastCall?.[0].model).toBe(next ?? undefined)
      expect(wrapper.getComponent({ name: 'UsageStatsCards' }).props('stats').total_requests).toBe(
        total,
      )
      expect(wrapper.findComponent({ name: 'Pagination' }).exists()).toBe(false)

      if (action === 'switch') {
        oldPage.resolve(oldResult)
        await flushPromises()
        expect(wrapper.findComponent({ name: 'Pagination' }).exists()).toBe(false)
      }

      const items = [alphaLog, betaLog].filter((row) => !next || row.model === next)
      firstPage.resolve({ items, total })
      await flushPromises()
      expect(wrapper.findAll('[data-test="usage-table"] span').map((row) => row.text())).toEqual(
        items.map((row) => row.request_id),
      )
      expect(wrapper.getComponent({ name: 'Pagination' }).props()).toMatchObject({ page: 1, total })
      expect(
        wrapper.get<HTMLButtonElement>('[aria-label="pagination.next"]').element.disabled,
      ).toBe(total === 1)
    },
  )

  // A new date range has no matching records while an older request is still
  // pending. Its late failure must leave the successful empty result visible.
  it('resets the page on date changes and ignores superseded list failures', async () => {
    let rejectOld!: (error: Error) => void
    query.mockResolvedValueOnce({ items: [usageLog], total: 51 })
    const wrapper = mountUsageView()
    await flushPromises()
    query.mockImplementationOnce(
      () =>
        new Promise((_resolve, reject) => {
          rejectOld = reject
        }),
    )
    wrapper.getComponent({ name: 'Pagination' }).vm.$emit('update:page', 2)
    query.mockResolvedValueOnce({ items: [], total: 0 })
    wrapper.getComponent({ name: 'DateRangePicker' }).vm.$emit('change', {
      startDate: '2026-11-20',
      endDate: '2026-11-20',
      preset: null,
    })
    await flushPromises()
    rejectOld(new Error('superseded'))
    await flushPromises()
    expect(query.mock.lastCall?.[0]).toMatchObject({
      page: 1,
      start_date: '2026-11-20',
      end_date: '2026-11-20',
    })
    expect(wrapper.get('[data-test="usage-table"]').text()).toBe('')
    expect(wrapper.findComponent({ name: 'Pagination' }).exists()).toBe(false)
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    expect(showError).not.toHaveBeenCalled()
  })

  // Group options contain translated and legacy names before a locale change. The test
  // protects display-name precedence and refreshing the selected language's labels after
  // language restoration.
  it('分组筛选使用展示名称，并在切换语言后刷新', async () => {
    getAvailable.mockResolvedValue([
      { id: 1, name: 'business-group', display_name: 'English group' },
      { id: 2, name: 'Legacy group' },
    ])
    const wrapper = mountUsageView()
    await flushPromises()
    // options collects option objects exposed by every mounted filter option list stub.
    const options = () =>
      wrapper
        .findAllComponents({ name: 'FilterOptions' })
        .flatMap((item) => item.props('options') || [])
    expect(options()).toContainEqual(expect.objectContaining({ value: 1, label: 'English group' }))
    expect(options()).toContainEqual(expect.objectContaining({ value: 2, label: 'Legacy group' }))
    getAvailable.mockResolvedValue([{ id: 1, name: 'business-group', display_name: '中文分组' }])
    window.dispatchEvent(new CustomEvent('locale-changed', { detail: 'zh-Hans' }))
    await flushPromises()
    expect(options()).toContainEqual(expect.objectContaining({ value: 1, label: '中文分组' }))
    wrapper.unmount()
  })

  // The public error-visibility setting is absent or false on first render. The test
  // protects loading ordinary usage without querying errors, displaying error
  // notifications, or hiding the usage table.
  it.each([undefined, false])(
    'skips error queries on locale refresh when visibility is %s',
    async (enabled) => {
      publicSettings.allow_user_view_error_requests = enabled
      listMyErrorRequests.mockRejectedValue({ status: 403 })
      const wrapper = mountUsageView()
      await flushPromises()
      query.mockClear()

      window.dispatchEvent(new CustomEvent('locale-changed', { detail: 'zh-Hans' }))
      await flushPromises()

      expect(query).toHaveBeenCalledOnce()
      expect(listMyErrorRequests).not.toHaveBeenCalled()
      expect(showError).not.toHaveBeenCalled()
      expect(wrapper.findComponent(UsageTableStub).exists()).toBe(true)
    },
  )

  // The error loader is invoked directly while visibility is disabled. The test protects
  // returning before any API request, loading state change, or error notification is
  // produced.
  it('guards the error loader when error visibility is disabled', async () => {
    publicSettings.allow_user_view_error_requests = false
    const wrapper = mountUsageView()
    await flushPromises()

    await (wrapper.vm as any).loadErrors()

    expect(listMyErrorRequests).not.toHaveBeenCalled()
    expect((wrapper.vm as any).errorLoading).toBe(false)
    expect(showError).not.toHaveBeenCalled()
  })

  // Error visibility is enabled but the error tab remains unopened. The test protects
  // deferring its API request until the user selects the error view.
  it('defers enabled error queries until the errors tab is opened', async () => {
    publicSettings.allow_user_view_error_requests = true
    const wrapper = mountUsageView()
    await flushPromises()

    window.dispatchEvent(new CustomEvent('locale-changed', { detail: 'zh-Hans' }))
    await flushPromises()
    expect(listMyErrorRequests).not.toHaveBeenCalled()

    await wrapper
      .findAll('button')
      .find((button) => button.text() === 'usage.tabs.errors')!
      .trigger('click')
    await flushPromises()
    expect(listMyErrorRequests).toHaveBeenCalledOnce()
  })

  // Locale changes occur with errors visible and then with their tab inactive. The test
  // protects immediate visible refresh and invalidating inactive records for retrieval when
  // reopened.
  it('refreshes visible errors and invalidates inactive error rows on locale changes', async () => {
    publicSettings.allow_user_view_error_requests = true
    listMyErrorRequests.mockResolvedValue({ items: [{ id: 1 }], total: 1, pages: 1 })
    const wrapper = mountUsageView()
    await flushPromises()
    await wrapper
      .findAll('button')
      .find((button) => button.text() === 'usage.tabs.errors')!
      .trigger('click')
    await flushPromises()
    listMyErrorRequests.mockClear()

    window.dispatchEvent(new CustomEvent('locale-changed', { detail: 'en' }))
    await flushPromises()
    expect(listMyErrorRequests).toHaveBeenCalledOnce()

    await wrapper
      .findAll('button')
      .find((button) => button.text() === 'usage.tabs.usage')!
      .trigger('click')
    listMyErrorRequests.mockClear()
    window.dispatchEvent(new CustomEvent('locale-changed', { detail: 'zh-Hans' }))
    await flushPromises()
    expect(listMyErrorRequests).not.toHaveBeenCalled()

    await wrapper
      .findAll('button')
      .find((button) => button.text() === 'usage.tabs.errors')!
      .trigger('click')
    await flushPromises()
    expect(listMyErrorRequests).toHaveBeenCalledOnce()
    expect(showError).not.toHaveBeenCalled()
  })

  // Main filter lists start with unselected null values in the UI. The test protects their
  // leading all-option and omitting those predicates from the normalized usage query.
  it('主筛选项初始值为 null，首项是“全部”，请求参数不带未选条件', async () => {
    const wrapper = mountUsageView()
    await flushPromises()

    const labels = [
      'usage.allApiKeys',
      'admin.usage.allModels',
      'admin.usage.allGroups',
      'admin.usage.allTypes',
    ]
    for (const label of labels) {
      const filterOptions = wrapper
        .findAllComponents({ name: 'FilterOptions' })
        .find((item) => item.props('options')?.[0]?.label === (messages[label] ?? label))
      expect(filterOptions?.props('modelValue')).toBeNull()
    }
    const params = query.mock.calls[0][0]
    for (const key of ['api_key_id', 'group_id', 'model', 'request_type']) {
      expect(params[key]).toBeUndefined()
    }
  })

  // A team owner loads team keys and aggregate member usage. The test protects owner
  // requests, member column presentation, compact attribution, and disabling administrator
  // navigation for user rows.
  it('loads team keys and aggregated member charts for a team owner', async () => {
    getCurrentTeam.mockResolvedValue({
      team: { id: 7, name: 'Demo team' },
      membership: { user_id: 42, role: 'owner' },
      owner: { user_id: 42, role: 'owner' },
    })
    getTeamKeys.mockResolvedValue([{ id: 9, name: 'Team key' }])
    getTeamMembers.mockResolvedValue([
      { user_id: 42, username: 'Owner', email: 'owner@example.com', role: 'owner' },
      { user_id: 43, username: 'Member', email: 'member@example.com', role: 'member' },
    ])
    getTeamMemberUsage.mockResolvedValue([
      {
        actor_user_id: 42,
        display_name: 'Owner',
        status: 'active',
        summary: {
          actual_cost: 1.2,
          request_count: 1,
          input_tokens: 10,
          output_tokens: 5,
          daily: [],
        },
      },
      {
        actor_user_id: 43,
        display_name: 'Former member',
        status: 'left',
        summary: {
          actual_cost: 0.8,
          request_count: 1,
          input_tokens: 10,
          output_tokens: 5,
          daily: [],
        },
      },
    ])

    const wrapper = mountUsageView()
    await flushPromises()

    expect(getTeamKeys).toHaveBeenCalledOnce()
    expect(getTeamMembers).toHaveBeenCalledOnce()
    expect(getTeamMemberUsage).toHaveBeenCalledOnce()
    expect(getTeamMemberUsage).toHaveBeenCalledWith(
      expect.objectContaining({
        from: expect.any(String),
        to: expect.any(String),
      }),
    )
    const usageTable = wrapper.findComponent(UsageTableStub)
    const columns = usageTable.props('columns') as Column[]
    expect(columns.find(({ key }) => key === 'caller')?.label).toBe('Member / Group')
    expect(usageTable.props('userClickable')).toBe(false)
    expect(usageTable.props('compactRole')).toBe('teamOwner')
  })

  // Personal usage starts with the full composite layout and copy-only attribution,
  // even if the old default hid the standalone user-agent field.
  it('shows all nine composite columns for a personal user', async () => {
    localStorage.setItem('user-usage-hidden-columns', JSON.stringify(['user_agent']))
    const wrapper = mountUsageView()
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
    expect(columns.find(({ key }) => key === 'caller')?.label).toBe('Key / Group')
    expect(columns.reduce((width, column) => width + Number.parseInt(column.width!, 10), 0)).toBe(
      1485,
    )
    expect(table.props('compactRole')).toBe('personal')
    expect(table.props('showProviderBilling')).toBe(false)
    expect(table.props('showStandardCost')).toBe(false)
    expect(table.props('columnOrderStorageKey')).toBe('user-usage-column-order-v2')
  })

  // The personal and team layouts share caller preferences. An older personal profile
  // must also hide the member key to hide the composite caller after migration.
  it.each([
    { hidden: ['api_key', 'group'], callerVisible: true },
    { hidden: ['user', 'api_key', 'group'], callerVisible: false },
  ])(
    'migrates caller visibility when the old profile hid $hidden',
    async ({ hidden, callerVisible }) => {
      localStorage.setItem(
        'user-usage-hidden-columns',
        JSON.stringify([...hidden, 'ip_address', 'user_agent']),
      )
      const wrapper = mountUsageView()
      await flushPromises()
      const columns = wrapper.findComponent(UsageTableStub).props('columns') as Column[]
      expect(columns.some(({ key }) => key === 'caller')).toBe(callerVisible)
      expect(columns.some(({ key }) => key === 'client_network')).toBe(false)
      expect(localStorage.getItem('user-usage-hidden-columns-dense-layout-v1')).toBe(
        JSON.stringify(callerVisible ? ['client_network'] : ['caller', 'client_network']),
      )
    },
  )

  // The model sort uses the API key and falls back to time when its cell is hidden.
  // A later mount restores the hidden column and starts with descending time.
  it('persists a hidden model group and reloads page one only once', async () => {
    const wrapper = mountUsageView()
    await flushPromises()
    const table = wrapper.findComponent(UsageTableStub)
    table.vm.$emit('sort', 'model', 'asc')
    await flushPromises()
    expect(query).toHaveBeenLastCalledWith(
      expect.objectContaining({ page: 1, sort_by: 'model', sort_order: 'asc' }),
      expect.anything(),
    )
    query.mockClear()

    await wrapper.get('button[title="Columns"]').trigger('click')
    const model = wrapper.findAll('button').find((button) => button.text() === 'Model / Type')!
    await model.trigger('click')
    table.vm.$emit('sort', 'created_at', 'desc')
    await flushPromises()
    expect(query).toHaveBeenCalledOnce()
    expect(query).toHaveBeenLastCalledWith(
      expect.objectContaining({ page: 1, sort_by: 'created_at', sort_order: 'desc' }),
      expect.anything(),
    )
    expect(localStorage.getItem('user-usage-hidden-columns-dense-layout-v1')).toBe(
      JSON.stringify(['model_spec']),
    )
    wrapper.unmount()
    const restored = mountUsageView()
    await flushPromises()
    const columns = restored.findComponent(UsageTableStub).props('columns') as Column[]
    expect(columns.some(({ key }) => key === 'model_spec')).toBe(false)
    expect(query).toHaveBeenLastCalledWith(
      expect.objectContaining({ sort_by: 'created_at', sort_order: 'desc' }),
      expect.anything(),
    )
  })

  // Clicking the toolbar export button on the user Usage view opens UsageExportDialog
  // with the current date range. Resolved filter chips include API key, group, zero-valued
  // balance billing, and false-valued legacy compaction selections.
  it('opens UsageExportDialog with inherited date range and resolved filter labels', async () => {
    getAvailable.mockResolvedValue([{ id: 3, name: 'vip', display_name: 'VIP Group' }])
    const wrapper = mountUsageView()
    await flushPromises()

    const vm = wrapper.vm as any
    vm.startDate = '2026-03-01'
    vm.endDate = '2026-03-08'
    vm.filters.api_key_id = 1
    vm.filters.group_id = 3
    vm.filters.model = 'gpt-5.4'
    vm.filters.billing_type = 0
    vm.filters.native_compaction_v2 = false
    await flushPromises()

    const dialog = wrapper.findComponent(UsageExportDialogStub)
    expect(dialog.props('show')).toBe(false)

    await wrapper.get('[data-testid="user-usage-export-btn"]').trigger('click')
    expect(dialog.props('show')).toBe(true)
    expect(dialog.props('mode')).toBe('user')
    expect(dialog.props('startDate')).toBe('2026-03-01')
    expect(dialog.props('endDate')).toBe('2026-03-08')
    expect(dialog.props('filterLabels')).toEqual([
      { key: 'api_key_id', label: 'API Key', value: 'demo-key' },
      { key: 'model', label: 'Model', value: 'gpt-5.4' },
      { key: 'group_id', label: 'Group', value: 'VIP Group' },
      { key: 'billing_type', label: 'Billing type', value: 'Balance' },
      { key: 'native_compaction_v2', label: 'Compaction', value: 'Legacy compaction' },
    ])
  })

  // Mounting the user Usage view with pending and running background exports populates the
  // active count on the toolbar button. The button stays enabled so users can reopen
  // UsageExportDialog to monitor progress or cancel tasks.
  it('displays active background export count on the toolbar button without disabling it', async () => {
    listExportTasks.mockResolvedValueOnce({
      items: [
        { id: 21, status: 'running' },
        { id: 20, status: 'pending' },
      ],
      total: 2,
      page: 1,
      page_size: 10,
      pages: 1,
    })

    const wrapper = mountUsageView()
    await flushPromises()

    const exportBtn = wrapper.get('[data-testid="user-usage-export-btn"]')
    expect(exportBtn.attributes('disabled')).toBeUndefined()
    expect(exportBtn.text()).toBe('Export (2)')

    const dialog = wrapper.findComponent(UsageExportDialogStub)
    dialog.vm.$emit('tasksChanged', 0)
    await flushPromises()
    expect(exportBtn.text()).toBe('Export')
  })
})
