import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest'
import { i18n, loadLocaleMessages } from '@/i18n'
import { formatDateTimeToMinute, getBrowserTimeZone } from '@/utils/format'
import Select from '@/components/common/Select.vue'
import ImportedCallsPanel from '../ImportedCallsPanel.vue'

// ImportedCallsPanel.spec.ts exercises both archive views against a controlled GET
// transport. It mounts the real panel, table and pagination with the Chinese catalog
// and inspects the parameters produced by the administrator usage API wrapper.

// The HTTP boundary allows tests to delay a specific page independently of later selections.
const { get } = vi.hoisted(() => ({ get: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: { get } }))
enableAutoUnmount(afterEach)

// Mixed request outcomes and unequal latency sample counts distinguish the measured
// means from averages calculated using total requests or only successful requests.
const group = {
  group_id: 7,
  group_name: 'group A',
  success_requests: 8,
  error_requests: 3,
  input_tokens: 11,
  output_tokens: 13,
  cache_creation_tokens: 17,
  cache_read_tokens: 19,
  duration_sum_ms: 555,
  duration_count: 5,
  ttft_sum_ms: 0,
  ttft_count: 0,
}
const minute = {
  ...group,
  bucket_start: '2026-11-19T22:04:00Z',
  model: 'alpha.v1',
  source_platform: 'openai',
  duration_sum_ms: 0,
  duration_count: 1,
  ttft_sum_ms: 42,
  ttft_count: 3,
}
const minuteReport = {
  source: 'raks',
  groups: [group, { ...group, group_id: 9, group_name: 'group B' }],
  items: [minute],
  total: 21,
  page: 1,
  page_size: 20,
}
const dateProps = { startDate: '2026-11-25', endDate: '2026-11-25' }

// mountPanel opens the chosen projection with a calendar range after the fixture archive.
function mountPanel(view: 'groups' | 'minutes') {
  return mount(ImportedCallsPanel, { props: { view, ...dateProps }, global: { plugins: [i18n] } })
}

describe('raks 历史流量', () => {
  beforeAll(async () => {
    await loadLocaleMessages('zh-Hans')
    i18n.global.locale.value = 'zh-Hans'
  })
  beforeEach(() => {
    get.mockReset()
  })

  // An old group still appears under the initial unbounded scope. Calendar changes
  // affect the group query only after selecting dates, including empty intervals.
  it('默认展示全部分组，跟随所选日期，空范围可切回全部历史', async () => {
    get.mockImplementation(async (_, { params }) => ({
      data: { source: 'raks', groups: params.start_date ? [] : [group] },
    }))
    const wrapper = mountPanel('groups')
    await flushPromises()
    const [all, selected] = wrapper.findAll('button[aria-pressed]')
    expect(all.text()).toBe('全部历史')
    expect(all.attributes('aria-pressed')).toBe('true')
    expect(get.mock.lastCall).toEqual([
      '/admin/usage/imported-calls',
      {
        params: { view: 'groups', timezone: getBrowserTimeZone() },
        signal: expect.any(AbortSignal),
      },
    ])
    expect(wrapper.text()).toContain('与本站账单独立')
    expect(
      wrapper
        .get('tbody tr')
        .findAll('td')
        .map((cell) => cell.text()),
    ).toEqual(['group A', '11', '8', '3', '60', '111', '—'])
    expect(wrapper.get('th[data-column-key="requests"]').text()).toBe('总请求数')
    await wrapper.setProps({ endDate: '2026-11-26' })
    expect(get).toHaveBeenCalledTimes(1)
    await selected.trigger('click')
    await flushPromises()
    expect(get.mock.lastCall?.[1].params).toEqual({
      view: 'groups',
      timezone: getBrowserTimeZone(),
      start_date: '2026-11-25',
      end_date: '2026-11-26',
    })
    expect(wrapper.text()).toContain('没有匹配的 raks 历史记录')
    await wrapper.setProps({ startDate: '2026-11-25T19:04:02' })
    expect(get.mock.lastCall?.[1].params.start_date).toBe('2026-11-25T19:04:02')
    await all.trigger('click')
    await flushPromises()
    expect(get.mock.lastCall?.[1].params).not.toHaveProperty('start_date')
    expect(wrapper.text()).toContain('group A')
  })

  // A filtered minute page carries the group and exact model. Changing filters
  // from page two removes old navigation immediately and rejects its late response.
  it('分页、精确模型与分组筛选独立，迟到的旧页不覆盖空结果', async () => {
    get.mockResolvedValue({ data: minuteReport })
    const wrapper = mountPanel('minutes')
    await flushPromises()
    expect(get.mock.lastCall?.[1].params).toMatchObject({ view: 'minutes', page: 1, page_size: 20 })
    expect(get.mock.lastCall?.[1].params).not.toHaveProperty('end_date')
    expect(
      wrapper
        .get('tbody tr')
        .findAll('td')
        .map((cell) => cell.text()),
    ).toEqual([
      formatDateTimeToMinute(minute.bucket_start),
      'group A',
      'alpha.v1',
      'openai',
      '11',
      '8',
      '3',
      '60',
      '0',
      '14',
    ])
    wrapper.getComponent(Select).vm.$emit('update:modelValue', 7)
    await flushPromises()
    let resolveOld!: (result: unknown) => void
    get.mockImplementationOnce(
      () =>
        new Promise((resolve) => {
          resolveOld = resolve
        }),
    )
    await wrapper.get('.pagination-mobile button:last-child').trigger('click')
    const old = get.mock.lastCall?.[1]
    expect(old.params).toMatchObject({ page: 2, group_id: 7 })
    expect(wrapper.find('.pagination-root').exists()).toBe(false)
    get.mockResolvedValue({ data: { ...minuteReport, items: [], total: 0 } })
    await wrapper.get('input[type="search"]').setValue('Model.V2+')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(get.mock.lastCall?.[1].params).toMatchObject({
      group_id: 7,
      model: 'Model.V2+',
      page: 1,
    })
    expect(old.signal.aborted).toBe(true)
    resolveOld({ data: minuteReport })
    await flushPromises()
    expect(wrapper.text()).toContain('没有匹配的 raks 历史记录')
    expect(wrapper.getComponent(Select).props('options')).toHaveLength(3)
    expect(wrapper.find('.pagination-root').exists()).toBe(false)
    await wrapper.get('button[aria-pressed="false"]').trigger('click')
    await flushPromises()
    expect(get.mock.lastCall?.[1].params).toMatchObject({
      start_date: dateProps.startDate,
      end_date: dateProps.endDate,
      model: 'Model.V2+',
      page: 1,
    })
    expect(get.mock.lastCall?.[1].params.group_id).toBeUndefined()
  })

  // Two platforms reported the same group and model in one minute. Both platform
  // names remain visible and a successful page change replaces them with older rows.
  it('同分钟同组同模型按平台区分，第二页展示对应分钟', async () => {
    get.mockResolvedValueOnce({
      data: {
        ...minuteReport,
        items: [
          minute,
          { ...minute, source_platform: 'anthropic', success_requests: 2, error_requests: 1 },
        ],
      },
    })
    const wrapper = mountPanel('minutes')
    await flushPromises()
    const rows = wrapper.findAll('tbody tr')
    expect(rows).toHaveLength(2)
    expect(rows.map((row) => row.get('[data-column-key="platform"]').text())).toEqual([
      'openai',
      'anthropic',
    ])
    expect(rows.map((row) => row.get('[data-column-key="requests"]').text())).toEqual(['11', '3'])
    const older = { ...minute, bucket_start: '2026-11-18T23:59:00Z', model: 'alpha.v0' }
    get.mockResolvedValueOnce({ data: { ...minuteReport, page: 2, items: [older] } })
    await wrapper.get('.pagination-mobile button:last-child').trigger('click')
    await flushPromises()
    expect(get.mock.lastCall?.[1].params.page).toBe(2)
    expect(wrapper.get('tbody').text()).toContain('alpha.v0')
    expect(wrapper.get('tbody').text()).not.toContain('alpha.v1')
    expect(wrapper.get('button[aria-current="page"]').text()).toBe('2')
  })

  // A failed read gets a distinct retry action. Retrying keeps the active date
  // bounds and restores rows without asking the operator to reselect the range.
  it('所选日期失败时可以用相同条件重试', async () => {
    get.mockResolvedValue({ data: minuteReport })
    const wrapper = mountPanel('minutes')
    await flushPromises()
    get.mockRejectedValueOnce(new Error('offline'))
    await wrapper.get('button[aria-pressed="false"]').trigger('click')
    await flushPromises()
    const params = get.mock.lastCall?.[1].params
    expect(wrapper.get('[role="alert"]').text()).toContain('无法加载 raks 历史流量')
    await wrapper.get('[role="alert"] button').trigger('click')
    await flushPromises()
    expect(get.mock.lastCall?.[1].params).toEqual(params)
    expect(wrapper.text()).toContain('alpha.v1')
  })
})
