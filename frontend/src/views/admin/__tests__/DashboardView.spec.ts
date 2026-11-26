import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

import type { DashboardStats } from '@/api/admin/dashboard.types'
import DashboardView from '../DashboardView.vue'

// DashboardView.spec.ts checks administrator dashboard initialization and its
// pending-data presentation. Mocked snapshot, trend, and ranking endpoints isolate the
// initial date-range request, while a deferred snapshot lets the test observe the
// layout skeleton before summary cards become available.

// Hoisted dashboard endpoint mocks provide independently controlled summary, user
// trend, and ranking responses for component initialization tests.
const { getSnapshotV2, getUserUsageTrend, getUserSpendingRanking } = vi.hoisted(() => ({
  getSnapshotV2: vi.fn(),
  getUserUsageTrend: vi.fn(),
  getUserSpendingRanking: vi.fn(),
}))

// Imported history has a separate read boundary from the native snapshot and charts.
const { getImportedCalls } = vi.hoisted(() => ({ getImportedCalls: vi.fn() }))
vi.mock('@/api/admin/usage', () => ({ getImportedCalls }))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    dashboard: {
      getSnapshotV2,
      getUserUsageTrend,
      getUserSpendingRanking,
    },
  },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
  }),
}))

vi.mock('vue-router', () => ({
  useRouter: () => ({
    push: vi.fn(),
  }),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key,
    }),
  }
})

// formatLocalDate builds local calendar dates used by expected dashboard query
// parameters.
const formatLocalDate = (date: Date): string => {
  const year = date.getFullYear()
  const month = String(date.getMonth() + 1).padStart(2, '0')
  const day = String(date.getDate()).padStart(2, '0')
  return `${year}-${month}-${day}`
}

// createDashboardStats returns a complete zero-valued summary for dashboard rendering
// tests.
const createDashboardStats = (): DashboardStats => ({
  total_users: 0,
  today_new_users: 0,
  active_users: 0,
  hourly_active_users: 0,
  stats_updated_at: '',
  stats_stale: false,
  total_api_keys: 0,
  active_api_keys: 0,
  total_providers: 0,
  normal_providers: 0,
  error_providers: 0,
  ratelimit_providers: 0,
  overload_providers: 0,
  total_requests: 0,
  total_input_tokens: 0,
  total_output_tokens: 0,
  total_cache_creation_tokens: 0,
  total_cache_read_tokens: 0,
  total_tokens: 0,
  total_cost: 0,
  total_actual_cost: 0,
  today_requests: 0,
  today_input_tokens: 0,
  today_output_tokens: 0,
  today_cache_creation_tokens: 0,
  today_cache_read_tokens: 0,
  today_tokens: 0,
  today_cost: 0,
  today_actual_cost: 0,
  average_duration_ms: 0,
  uptime: 0,
  rpm: 0,
  tpm: 0,
})

// pinia holds the fresh store container installed before each test mounts the dashboard
// component and its composables.
let pinia: ReturnType<typeof createPinia>

// mountDashboard keeps native and imported reads active while replacing the chart canvases.
function mountDashboard() {
  return mount(DashboardView, {
    global: {
      plugins: [pinia],
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        Icon: true,
        DateRangePicker: true,
        Select: true,
        ModelDistributionChart: true,
        TokenUsageTrend: true,
        Line: true,
      },
    },
  })
}

describe('admin DashboardView', () => {
  beforeEach(() => {
    pinia = createPinia()
    setActivePinia(pinia)

    getSnapshotV2.mockReset()
    getUserUsageTrend.mockReset()
    getUserSpendingRanking.mockReset()
    getImportedCalls.mockReset().mockResolvedValue({ source: 'raks', groups: [] })

    getSnapshotV2.mockResolvedValue({
      stats: createDashboardStats(),
      trend: [],
      models: [],
    })
    getUserUsageTrend.mockResolvedValue({
      trend: [],
      start_date: '',
      end_date: '',
      granularity: 'hour',
    })
    getUserSpendingRanking.mockResolvedValue({
      ranking: [],
      total_actual_cost: 0,
      total_requests: 0,
      total_tokens: 0,
      start_date: '',
      end_date: '',
    })
  })

  // The snapshot request remains pending while the mounted dashboard initializes. The
  // layout skeleton must stay visible until the deferred response resolves, then give
  // way to summary cards.
  it('数据未返回时显示布局骨架，返回后渲染统计卡', async () => {
    let resolveSnapshot!: (value: unknown) => void
    getSnapshotV2.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolveSnapshot = resolve
        }),
    )
    const wrapper = mountDashboard()
    await flushPromises()
    expect(wrapper.find('[data-testid="dashboard-skeleton"]').exists()).toBe(true)
    expect(wrapper.text()).not.toContain('admin.dashboard.apiKeys')

    resolveSnapshot({ stats: createDashboardStats(), trend: [], models: [] })
    await flushPromises()
    expect(wrapper.find('[data-testid="dashboard-skeleton"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('admin.dashboard.apiKeys')
    wrapper.unmount()
  })

  // The dashboard mounts without a selected custom date range. Its first snapshot
  // request must use local dates spanning the previous 24 hours and hourly granularity.
  it('uses last 24 hours as default dashboard range', async () => {
    const wrapper = mountDashboard()

    await flushPromises()

    const now = new Date()
    const yesterday = new Date(now.getTime() - 24 * 60 * 60 * 1000)

    expect(getSnapshotV2).toHaveBeenCalledTimes(1)
    expect(getSnapshotV2).toHaveBeenCalledWith(
      expect.objectContaining({
        start_date: formatLocalDate(yesterday),
        end_date: formatLocalDate(now),
        granularity: 'hour',
      }),
    )
    expect(getImportedCalls.mock.lastCall?.[0]).not.toHaveProperty('start_date')
    await wrapper.get('button[aria-pressed="false"]').trigger('click')
    expect(getImportedCalls.mock.lastCall?.[0]).toMatchObject({
      view: 'groups',
      start_date: formatLocalDate(yesterday),
      end_date: formatLocalDate(now),
    })
    const picker = wrapper.getComponent({ name: 'DateRangePicker' })
    picker.vm.$emit('update:startDate', '2026-11-18')
    picker.vm.$emit('update:endDate', '2026-11-19')
    await flushPromises()
    expect(getImportedCalls.mock.lastCall?.[0]).toMatchObject({
      start_date: '2026-11-18',
      end_date: '2026-11-19',
    })
    expect(wrapper.text()).toContain('admin.dashboard.todayRequests')
    wrapper.unmount()
  })

  // A native snapshot failure leaves a successful unbounded group read visible.
  // Retrying the native section must neither discard the group nor repeat its read.
  it('原生统计失败时仍展示全量历史，原生重试不刷新历史', async () => {
    getSnapshotV2.mockRejectedValueOnce(new Error('snapshot unavailable'))
    getImportedCalls.mockResolvedValue({
      source: 'raks',
      groups: [
        {
          group_id: 9,
          group_name: 'Archive group',
          success_requests: 8,
          error_requests: 3,
          input_tokens: 25,
          output_tokens: 5,
          cache_creation_tokens: 0,
          cache_read_tokens: 0,
          duration_sum_ms: 0,
          duration_count: 0,
          ttft_sum_ms: 0,
          ttft_count: 0,
        },
      ],
    })
    const wrapper = mountDashboard()
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('admin.dashboard.failedToLoad')
    expect(wrapper.get('td[data-column-key="requests"]').text()).toBe('11')
    expect(wrapper.text()).toContain('Archive group')
    expect(getImportedCalls.mock.lastCall?.[0]).not.toHaveProperty('start_date')
    await wrapper.get('[role="alert"] button').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('admin.dashboard.todayRequests')
    expect(getImportedCalls).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })

  // An unavailable archive must leave the native cards visible. Retrying the
  // archive reads only imported groups and preserves the native snapshot.
  it('历史读取失败与重试独立于原生统计', async () => {
    getImportedCalls.mockRejectedValueOnce(new Error('archive unavailable'))
    const wrapper = mountDashboard()
    await flushPromises()
    expect(wrapper.text()).toContain('admin.dashboard.todayRequests')
    expect(wrapper.get('[role="alert"]').text()).toContain('admin.usage.imported.failedToLoad')
    await wrapper.get('[role="alert"] button').trigger('click')
    await flushPromises()
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    expect(getSnapshotV2).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })
})
