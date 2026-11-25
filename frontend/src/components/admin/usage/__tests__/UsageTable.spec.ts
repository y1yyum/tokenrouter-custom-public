import { nextMotionFrame } from '@/__tests__/helpers/motion'
// ipGeoMocks controls per-address cache state and batch results so table cases can
// assert lookup payloads and failure events.
const ipGeoMocks = vi.hoisted(() => ({
  getEntry: vi.fn(() => ({ status: 'idle' as const })),
  fetchOne: vi.fn(),
  fetchBatch: vi.fn(),
}))

// clipboardMocks records copied identifiers and returns controlled success values for
// request and upstream-ID feedback assertions.
const clipboardMocks = vi.hoisted(() => ({
  copyToClipboard: vi.fn(),
}))

vi.mock('@/utils/ipGeoLookup', () => ipGeoMocks)
vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => clipboardMocks,
}))

vi.mock('@/composables/useBalanceDisplay', () => ({
  useBalanceDisplay: () => ({
    balanceUnitSymbol: { value: '$' },
    usdUnitSymbol: '$',
    formatBalanceAmount: (value: number | null | undefined) => `$${(value ?? 0).toFixed(6)}`,
    formatUsdAmount: (value: number | null | undefined) => `$${(value ?? 0).toFixed(6)}`,
  }),
}))

import { describe, expect, it, vi, beforeEach } from 'vitest'
import { mount, RouterLinkStub } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { nextTick } from 'vue'

import UsageTable from '../UsageTable.vue'
import { getUsageTableColumns, type UsageTableRole } from '../usageTableColumns'
import type { AdminUsageLog } from '@/api/usage.types'

// UsageTable.spec.ts tests billing, identity, tooltip positioning, and IP lookup
// behavior in usage rows. Table-slot stubs expose actual cell content, while clipboard
// and location substitutes record effects for assertions against representative
// historical and current records.

// messages supplies billing, image provenance, identity, and copy labels inspected
// inside actual usage-cell slots and floating panels.
const messages: Record<string, string> = {
  'admin.usage.userDeletedBadge': 'Deleted',
  'usage.costDetails': 'Cost Breakdown',
  'usage.costUnknown': 'Unknown',
  'usage.costPartial': 'Partial',
  'usage.providerCost': 'Provider cost',
  'usage.imageFrameUnit': ' frames',
  'usage.compactTable.modelMapping': 'Model mapping',
  'usage.compactTable.modelDetails': 'Model details',
  'usage.compactTable.copyUpstreamId': 'Copy upstream request ID',
  'usage.compactTable.copyUserAgent': 'Copy user agent',
  'admin.usage.billingTypeBalance': 'Balance',
  'admin.usage.billingTypeSubscription': 'Subscription',
  'admin.usage.requestSources.provider_test': 'Manual provider test',
  'admin.usage.inputCost': 'Input Cost',
  'admin.usage.outputCost': 'Output Cost',
  'admin.usage.cacheCreationCost': 'Cache Creation Cost',
  'admin.usage.cacheReadCost': 'Cache Read Cost',
  'usage.inputTokenPrice': 'Input price',
  'usage.outputTokenPrice': 'Output price',
  'usage.perMillionTokens': '/ 1M tokens',
  'usage.serviceTier': 'Service tier',
  'usage.serviceTierPriority': 'Fast',
  'usage.serviceTierUltrafast': 'Ultrafast',
  'usage.serviceTierFlex': 'Flex',
  'usage.serviceTierStandard': 'Standard',
  'usage.rate': 'Rate',
  'usage.providerMultiplier': 'Provider rate',
  'usage.original': 'Original',
  'usage.userBilled': 'User billed',
  'usage.providerBilled': 'Provider billed',
  'usage.imageUnit': ' images',
  'usage.imageCount': 'Image count',
  'usage.imageBillingSize': 'Billing size',
  'usage.imageInputSize': 'Input size',
  'usage.imageOutputSize': 'Output size',
  'usage.imageSizeSource': 'Size source',
  'usage.imageSizeBreakdown': 'Size breakdown',
  'usage.imageSizeSourceOutput': 'Upstream output',
  'usage.imageSizeSourceInput': 'Request input',
  'usage.imageSizeSourceDefault': 'Default billing tier',
  'usage.imageSizeSourceLegacy': 'Legacy record',
  'usage.imageSizeSourceMissing': 'Not recorded',
  'usage.imageSizeNotRecorded': 'not recorded',
  'usage.imageSizeLegacyUnstandardized': 'legacy unstandardized',
  'usage.imageSizeUnknown': 'unknown',
  'usage.imageUnitPrice': 'Per-image price',
  'usage.imageTotalPrice': 'Image total price',
  'admin.usage.billingModeToken': 'Token',
  'admin.usage.billingModePerRequest': 'Per request',
  'admin.usage.billingModeImage': 'Image',
  'admin.usage.upstreamRequestIdCopied': 'Upstream ID copied',
  'keys.copied': 'Copied',
  'keys.copyToClipboard': 'Copy to clipboard',
  'usage.detailedTiming': 'Detailed Timing',
  'usage.timingRequestSize': 'Request size',
  'usage.timingAttempts': 'Attempts',
  'usage.timingReused': 'Reused conn',
  'usage.timingWriteError': 'Write error',
  'common.copyFailed': 'Copy failed',
  'common.copied': 'Copied',
  'requests.copy': 'Copy request ID',
  'requests.copied': 'Request ID copied',
  'admin.usage.cacheWriteAnomalyBadge': 'Anomaly',
  'admin.usage.cacheWriteAnomalyLabel': 'Cache Write Anomaly',
  'admin.usage.cacheWriteAnomalyKindPartial': 'Partial split',
  'admin.usage.cacheWriteAnomalyKindExceeds': 'Exceeds total',
  'admin.usage.cacheWriteAnomalyKindMissingTotal': 'Missing total',
  'admin.usage.cacheWriteAnomalyRawTotal': 'Reported total',
  'admin.usage.cacheWriteAnomalyRawSplit': 'Reported {short} / {long}',
  'admin.usage.cacheWriteAnomalyPartial': 'Partial breakdown (raw {raw})',
  'admin.usage.cacheWriteAnomalyMissingTotal': 'Missing total (raw {raw})',
  'admin.usage.cacheWriteAnomalyExceeds': 'Exceeds total (raw {raw})',
}

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, string>) => {
        let rendered = messages[key] ?? key
        if (params) {
          for (const [k, v] of Object.entries(params)) {
            rendered = rendered.replace(`{${k}}`, v)
          }
        }
        return rendered
      },
    }),
  }
})

// DataTableStub renders the usage cell slots for each fixture row so tests inspect the
// real cell and tooltip implementations.
const DataTableStub = {
  props: [
    'data', // supplied usage row records
  ],
  template: `
    <div>
      <div v-for="row in data" :key="row.request_id">
        <slot name="cell-model" :row="row" :value="row.model" />
        <slot name="cell-billing_mode" :row="row" />
        <slot name="cell-tokens" :row="row" />
        <slot name="cell-cost" :row="row" />
        <slot name="cell-latency" :row="row" />
        <div class="tps-cell"><slot name="cell-tps" :row="row" /></div>
        <slot name="cell-request_id" :row="row" />
        <slot name="cell-upstream_request_id" :row="row" />
      </div>
    </div>
  `,
}

// baseImageRow supplies representative usage measurements reused by rendering cases, with
// each test overriding the fields relevant to its scenario.
const baseImageRow = {
  request_id: 'req-admin-image',
  model: 'gpt-image-2',
  actual_cost: 0.4,
  total_cost: 0.4,
  provider_rate_multiplier: 1,
  rate_multiplier: 1,
  service_tier: null,
  input_cost: 0,
  output_cost: 0,
  cache_creation_cost: 0,
  cache_read_cost: 0,
  input_tokens: 0,
  output_tokens: 0,
  cache_creation_tokens: 0,
  cache_read_tokens: 0,
  cache_creation_5m_tokens: 0,
  cache_creation_1h_tokens: 0,
  cache_ttl_overridden: false,
  billing_mode: 'image',
  image_count: 2,
  image_size: '2K',
  image_input_size: null,
  image_output_size: null,
  image_size_source: null,
  image_size_breakdown: null,
}

// Usage rows include translated and legacy group names. The test protects display-name
// precedence, business-name recovery, and updating the group badge when its display name
// changes.
it('用量分组徽章优先显示译文，缺少展示名称时使用业务名称', async () => {
  const wrapper = mount(UsageTable, {
    props: {
      data: [
        {
          ...baseImageRow,
          request_id: 'translated',
          group: { id: 1, name: 'business-name', display_name: 'English group' },
        },
        { ...baseImageRow, request_id: 'original', group: { id: 2, name: 'Original group' } },
      ],
      loading: false,
      columns: [{ key: 'group', label: 'Group' }],
      showProviderBilling: false,
    },
    global: {
      stubs: {
        DataTable: {
          props: [
            'data', // supplied usage row records
          ],
          template: '<div><slot v-for="row in data" name="cell-group" :row="row" /></div>',
        },
        GroupBadge: {
          props: [
            'name', // displayed usage group name
          ],
          template: '<span data-test="group-name">{{ name }}</span>',
        },
        EmptyState: true,
        Icon: true,
        Teleport: true,
      },
    },
  })
  expect(wrapper.findAll('[data-test="group-name"]').map((item) => item.text())).toEqual([
    'English group',
    'Original group',
  ])
  await wrapper.setProps({
    data: [{ ...baseImageRow, group: { id: 1, name: 'business-name', display_name: '中文分组' } }],
  })
  expect(wrapper.get('[data-test="group-name"]').text()).toBe('中文分组')
  wrapper.unmount()
})

describe('admin UsageTable request ID column', () => {
  beforeEach(() => {
    clipboardMocks.copyToClipboard.mockReset().mockResolvedValue(true)
  })

  // A complete request identifier appears in a rendered usage row. The test protects the
  // clipboard payload and the copied feedback shown after clicking its action.
  // A system probe has no customer identity and no reliable price. The row must
  // identify its source and retain unknown expense instead of displaying free usage.
  it('shows system attribution and unknown provider cost', () => {
    const wrapper = mount(UsageTable, {
      props: {
        data: [
          {
            ...baseImageRow,
            actual_cost: 0,
            total_cost: 0,
            user_id: null,
            api_key_id: null,
            request_source: 'group_probe',
            provider_rate_multiplier: 1,
            system_usage: {
              parent_request_id: 'run',
              outcome: 'success',
              usage_state: 'missing',
              cost_state: 'unknown',
            },
          },
        ],
        loading: false,
        columns: [
          { key: 'user', label: 'User' },
          { key: 'cost', label: 'Cost' },
        ],
        showProviderBilling: true,
      },
      global: {
        stubs: {
          DataTable: {
            props: ['data'],
            template:
              '<div v-for="row in data" :key="row.request_id"><slot name="cell-user" :row="row" /><slot name="cell-cost" :row="row" /></div>',
          },
          EmptyState: true,
          Icon: true,
          Teleport: true,
        },
      },
    })
    expect(wrapper.text()).toContain('admin.usage.requestSources.group_probe')
    expect(wrapper.text()).toContain('Unknown')
    expect(wrapper.text()).not.toContain('#null')
    expect(wrapper.text()).not.toContain('A $0.000000')
    wrapper.unmount()
  })

  it('renders and copies the complete request ID', async () => {
    const wrapper = mount(UsageTable, {
      props: {
        data: [{ ...baseImageRow, request_id: 'req-admin-visible-id' }],
        loading: false,
        columns: [{ key: 'request_id', label: 'Request ID' }],
      },
      global: {
        stubs: { DataTable: DataTableStub, EmptyState: true, Icon: true, Teleport: true },
      },
    })

    expect(wrapper.text()).toContain('req-admin-visible-id')
    const copyButton = wrapper.get('button[title="Copy request ID"]')
    await copyButton.trigger('click')

    expect(clipboardMocks.copyToClipboard).toHaveBeenCalledWith(
      'req-admin-visible-id',
      'Request ID copied',
    )
    expect(copyButton.attributes('title')).toBe('Copied')
  })
})

describe('使用记录 TPS 列', () => {
  // Usage rows include server TPS estimates and an unavailable estimate. The test protects
  // numeric text, the shared formula hint, and the placeholder for missing estimates.
  it('展示 TPS 和公式提示，没有服务端估算时显示占位符', () => {
    const wrapper = mount(UsageTable, {
      props: {
        data: [
          {
            ...baseImageRow,
            request_id: 'valid',
            output_tokens: 100,
            duration_ms: 5000,
            ttft_ms: 1000,
            tps: 25,
          },
          {
            ...baseImageRow,
            request_id: 'missing',
            output_tokens: 100,
            duration_ms: 5000,
            ttft_ms: null,
          },
          {
            ...baseImageRow,
            request_id: 'rounding',
            output_tokens: 23,
            duration_ms: 21000,
            ttft_ms: 1000,
            tps: 1.2,
          },
        ],
        columns: [{ key: 'tps', label: 'TPS' }],
      },
      global: {
        stubs: { DataTable: DataTableStub, EmptyState: true, Icon: true, Teleport: true },
      },
    })
    expect(wrapper.findAll('.tps-cell').map((cell) => cell.text())).toEqual([
      '25.0 tok/s',
      '-',
      '1.2 tok/s',
    ])
    expect(wrapper.get('.tps-cell span').attributes('title')).toBe('usage.tpsHint')
    wrapper.unmount()
  })
})

describe('admin UsageTable detailed timing tooltip', () => {
  // A usage row carries detailed request stages and request-size observations. The test
  // protects opening timing details and rendering those recorded values with their
  // corresponding units.
  it('renders the request stage timings from the latency info button', async () => {
    const wrapper = mount(UsageTable, {
      props: {
        data: [
          {
            ...baseImageRow,
            detailed_timing: {
              request_content_length: 8360000,
              request_raw_bytes: 2097152,
              request_decoded_bytes: 8360000,
              request_compressed: 1,
              upstream_request_bytes: 8388608,
              request_body_decode_ms: 5,
              bootstrap_ms: 14,
              provider_prepare_ms: 21,
              provider_slot_acquired_ms: 120,
              upstream_got_conn_ms: 200,
              upstream_get_conn_ms: 180,
              upstream_wrote_request_ms: 420,
              upstream_first_response_byte_ms: 980,
              upstream_first_event_ms: 1020,
              first_visible_output_ms: 1060,
              first_downstream_flush_ms: 1080,
              upstream_attempt_count: 2,
            },
          },
        ],
        loading: false,
        columns: [{ key: 'latency', label: 'Latency' }],
      },
      global: {
        stubs: { DataTable: DataTableStub, EmptyState: true, Icon: true, Teleport: true },
      },
    })

    const timingButton = wrapper.get('button[title="Detailed Timing"]')
    await timingButton.trigger('mouseenter')
    await nextTick()

    expect(wrapper.text()).toContain('requests.timingNames.provider_slot_acquired_ms')
    expect(wrapper.text()).toContain('2.00MB')
    expect(wrapper.text()).toContain('7.97MB')
    expect(wrapper.text()).toContain('8.00MB')
    expect(wrapper.text()).toContain('requests.timing.compressedYes')
    expect(wrapper.text()).toContain('requests.timing.preprocessSteps')
    expect(wrapper.text()).toContain('requests.timing.afterSlot')
    expect(wrapper.text()).toContain('requests.timingNames.bootstrap_ms')
    expect(wrapper.text()).toContain('requests.timingNames.provider_prepare_ms')
    expect(wrapper.text()).toContain('120ms')
    expect(wrapper.text()).toContain('requests.timingNames.upstream_first_event_ms')
    expect(wrapper.text()).toContain('1.02s')
    expect(wrapper.text()).not.toContain('requests.timing.durations')
    expect(wrapper.text()).toContain('2')
  })

  // A timing panel is opened by a touch-style click. The test protects keeping it visible
  // after pointer exit and closing it on the next toggle.
  it('keeps the timing tooltip open after a mobile-style click until toggled again', async () => {
    const wrapper = mount(UsageTable, {
      props: {
        data: [
          {
            ...baseImageRow,
            detailed_timing: { provider_slot_acquired_ms: 120 },
          },
        ],
        loading: false,
        columns: [{ key: 'latency', label: 'Latency' }],
      },
      global: {
        stubs: { DataTable: DataTableStub, EmptyState: true, Icon: true, Teleport: true },
      },
    })

    const timingButton = wrapper.get('button[title="Detailed Timing"]')
    await timingButton.trigger('click')
    await nextTick()
    expect(wrapper.find('[data-testid="timing-detail-tooltip"]').exists()).toBe(true)

    await timingButton.trigger('mouseleave')
    expect(wrapper.find('[data-testid="timing-detail-tooltip"]').exists()).toBe(true)

    await timingButton.trigger('click')
    await nextTick()
    await nextMotionFrame()
    expect(wrapper.find('[data-testid="timing-detail-tooltip"]').exists()).toBe(false)
  })
})

describe('admin UsageTable tooltip', () => {
  beforeEach(() => {
    vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockReturnValue({
      x: 0,
      y: 0,
      top: 20,
      left: 20,
      right: 120,
      bottom: 40,
      width: 100,
      height: 20,
      toJSON: () => ({}),
    } as DOMRect)
  })

  // Only one fixture row reports that long-context billing was applied. The test protects
  // rendering the long-context marker on that row alone within the table.
  it('marks only usage rows that actually applied long-context billing', () => {
    const wrapper = mount(UsageTable, {
      props: {
        data: [
          {
            ...baseImageRow,
            request_id: 'req-long-context-enabled',
            long_context_billing_applied: true,
          },
          {
            ...baseImageRow,
            request_id: 'req-long-context-disabled',
            long_context_billing_applied: false,
          },
        ],
        loading: false,
        columns: [],
      },
      global: {
        stubs: {
          DataTable: DataTableStub,
          EmptyState: true,
          Icon: true,
          Teleport: true,
        },
      },
    })

    expect(wrapper.findAll('[data-testid="long-context-billing-marker"]')).toHaveLength(1)
    expect(wrapper.get('[data-testid="long-context-billing-marker"]').text()).toBe('L')
  })

  // A usage record contains service tier, user costs, and provider pricing. The test
  // protects the cost tooltip labels and separately formatted user and provider amounts.
  it('shows service tier and billing breakdown in cost tooltip', async () => {
    const row = {
      request_id: 'req-admin-1',
      actual_cost: 0.092883,
      total_cost: 0.092883,
      provider_rate_multiplier: 1,
      rate_multiplier: 1,
      service_tier: 'priority',
      input_cost: 0.020285,
      output_cost: 0.00303,
      cache_creation_cost: 0,
      cache_read_cost: 0.069568,
      input_tokens: 4057,
      output_tokens: 101,
    }

    const wrapper = mount(UsageTable, {
      props: {
        data: [row],
        loading: false,
        columns: [],
      },
      global: {
        stubs: {
          DataTable: DataTableStub,
          EmptyState: true,
          Icon: true,
          Teleport: true,
        },
      },
    })

    const tooltipTriggers = wrapper.findAll('.group.relative')
    await tooltipTriggers[tooltipTriggers.length - 1].trigger('mouseenter')
    await nextTick()

    const text = wrapper.text()
    expect(text).toContain('Service tier')
    expect(text).toContain('Fast')
    expect(text).toContain('Rate')
    expect(text).toContain('1.00x')
    expect(text).toContain('Provider rate')
    expect(text).toContain('User billed')
    expect(text).toContain('Provider billed')
    expect(text).toContain('$0.092883')
    expect(text).toContain('$5.0000 / 1M tokens')
    expect(text).toContain('$30.0000 / 1M tokens')
    expect(text).toContain('$0.069568')
  })

  // A personal usage row has cache traffic and a discounted charge. Keyboard
  // focus exposes its full token count and the amount deducted from the user.
  it('exposes token and charge details to keyboard users', async () => {
    const wrapper = mount(UsageTable, {
      props: {
        data: [
          {
            ...baseImageRow,
            billing_mode: 'token',
            image_count: 0,
            model: 'f04-alpha',
            input_tokens: 60,
            output_tokens: 30,
            cache_creation_tokens: 10,
            cache_read_tokens: 25,
            total_cost: 0.5,
            actual_cost: 0.25,
          },
        ],
        columns: [],
        showProviderBilling: false,
        showStandardCost: false,
      },
      global: {
        stubs: {
          DataTable: DataTableStub,
          Icon: true,
          Teleport: true,
          MotionTransition: { template: '<slot />' },
        },
      },
    })
    const tokens = wrapper.get('button[aria-label="usage.tokenDetails"]')
    await tokens.trigger('focus')
    const breakdown = wrapper.get('[data-testid="token-detail-tooltip"]')
    expect(tokens.attributes('aria-describedby')).toBe(breakdown.attributes('id'))
    expect(breakdown.text()).toContain('125')
    await tokens.trigger('blur')
    expect(wrapper.find('[data-testid="token-detail-tooltip"]').exists()).toBe(false)

    const cost = wrapper.get('button[aria-label="Cost Breakdown"]')
    await cost.trigger('focus')
    const charges = wrapper.get('[data-testid="cost-detail-tooltip"]')
    expect(cost.attributes('aria-describedby')).toBe(charges.attributes('id'))
    expect(charges.text()).toContain('$0.250000')
    expect(charges.text()).not.toContain('$0.500000')
    await cost.trigger('keydown', { key: 'Escape' })
    expect(wrapper.find('[data-testid="cost-detail-tooltip"]').exists()).toBe(false)
  })

  // Floating detail panels open near the edge of a narrow viewport. The test protects their
  // horizontal coordinates staying within padding and their ready state becoming visible.
  it.each([
    {
      triggerIndex: 0,
      tooltipTestId: 'token-detail-tooltip',
      tooltipWidth: 260,
      tooltipHeight: 180,
    },
    {
      triggerIndex: 1,
      tooltipTestId: 'cost-detail-tooltip',
      tooltipWidth: 296,
      tooltipHeight: 340,
    },
  ])(
    'keeps $tooltipTestId inside a narrow viewport',
    async ({ triggerIndex, tooltipTestId, tooltipWidth, tooltipHeight }) => {
      const viewportWidthSpy = vi.spyOn(window, 'innerWidth', 'get').mockReturnValue(320)
      const viewportHeightSpy = vi.spyOn(window, 'innerHeight', 'get').mockReturnValue(640)
      const rectSpy = vi.mocked(HTMLElement.prototype.getBoundingClientRect)
      rectSpy.mockImplementation(function (this: HTMLElement) {
        if (this.dataset.testid === tooltipTestId) {
          return {
            x: 0,
            y: 0,
            top: 0,
            left: 0,
            right: tooltipWidth,
            bottom: tooltipHeight,
            width: tooltipWidth,
            height: tooltipHeight,
            toJSON: () => ({}),
          } as DOMRect
        }

        return {
          x: 280,
          y: 300,
          top: 300,
          left: 280,
          right: 304,
          bottom: 320,
          width: 24,
          height: 20,
          toJSON: () => ({}),
        } as DOMRect
      })

      const wrapper = mount(UsageTable, {
        props: {
          data: [
            {
              request_id: 'req-mobile-tooltip',
              model: 'gpt-5.4',
              actual_cost: 0.092883,
              total_cost: 0.092883,
              rate_multiplier: 1,
              input_cost: 0.020285,
              output_cost: 0.00303,
              cache_creation_cost: 0,
              cache_read_cost: 0.069568,
              input_tokens: 4057,
              output_tokens: 101,
            },
          ],
          loading: false,
          columns: [],
        },
        global: {
          stubs: {
            DataTable: DataTableStub,
            EmptyState: true,
            Icon: true,
            Teleport: true,
          },
        },
      })

      try {
        await wrapper.findAll('.group.relative')[triggerIndex].trigger('mouseenter')
        await nextTick()
        await nextTick()

        const tooltip = wrapper.get(`[data-testid="${tooltipTestId}"]`)
        const left = Number.parseFloat((tooltip.element as HTMLElement).style.left)
        expect(left).toBeGreaterThanOrEqual(12)
        expect(left + tooltipWidth).toBeLessThanOrEqual(308)
        expect(tooltip.classes()).not.toContain('invisible')
      } finally {
        wrapper.unmount()
        viewportWidthSpy.mockRestore()
        viewportHeightSpy.mockRestore()
      }
    },
  )

  // Upstream response-model fixtures cover matching, different, and unrecorded identities.
  // The test protects conditional mismatch text and the relationship shown between
  // forwarded and returned models.
  it.each([
    { mismatch: true, response: 'runtime-model-' + 'v'.repeat(160), visible: true },
    { mismatch: false, response: 'sent-model', visible: false },
    { mismatch: null, response: null, visible: false },
    { mismatch: undefined, response: undefined, visible: false },
    { mismatch: true, response: '', visible: false },
  ])(
    'renders the response model only on a known mismatch: $mismatch/$visible',
    ({ mismatch, response, visible }) => {
      const wrapper = mount(UsageTable, {
        props: {
          data: [
            {
              ...baseImageRow,
              model: 'public-model',
              upstream_model: 'sent-model',
              model_mapping_chain: 'public-model→sent-model',
              upstream_response_model: response,
              upstream_model_mismatch: mismatch,
            },
          ],
          loading: false,
          columns: [],
        },
        global: {
          stubs: { DataTable: DataTableStub, EmptyState: true, Icon: true, Teleport: true },
        },
      })
      const model = wrapper.find('[data-testid="upstream-response-model"]')
      expect(model.exists()).toBe(visible)
      expect(wrapper.text()).toContain('public-model')
      expect(wrapper.text()).toContain('sent-model')
      if (visible) {
        expect(model.text()).toContain('↳')
        expect(model.text()).toContain(response)
        expect(model.attributes('title')).toContain(`sent-model → ${response}`)
        expect(model.classes()).toEqual(
          expect.arrayContaining([
            'text-xs',
            'break-all',
            'text-orange-600',
            'dark:text-orange-400',
          ]),
        )
      }
      wrapper.unmount()
    },
  )

  // An administrator row has different requested and forwarded model names. The test
  // protects displaying both identities so the requested model does not conceal upstream
  // attribution.
  it('shows requested and upstream models separately for admin rows', () => {
    const row = {
      request_id: 'req-admin-model-1',
      model: 'claude-sonnet-4',
      upstream_model: 'claude-sonnet-4-20250514',
      actual_cost: 0,
      total_cost: 0,
      provider_rate_multiplier: 1,
      rate_multiplier: 1,
      input_cost: 0,
      output_cost: 0,
      cache_creation_cost: 0,
      cache_read_cost: 0,
      input_tokens: 0,
      output_tokens: 0,
    }

    const wrapper = mount(UsageTable, {
      props: {
        data: [row],
        loading: false,
        columns: [],
      },
      global: {
        stubs: {
          DataTable: DataTableStub,
          EmptyState: true,
          Icon: true,
          Teleport: true,
        },
      },
    })

    const text = wrapper.text()
    expect(text).toContain('claude-sonnet-4')
    expect(text).toContain('claude-sonnet-4-20250514')
  })

  // Image fixtures cover default, input, output, and historical size sources. The test
  // protects billing size labels, source provenance, raw dimensions, and per-image pricing
  // details.
  it.each([
    {
      name: 'defaulted row',
      row: {
        ...baseImageRow,
        request_id: 'req-admin-default-image',
        image_size: '2K',
        image_input_size: 'auto',
        image_output_size: null,
        image_size_source: 'default',
      },
      expected: ['2K', 'Default billing tier', 'auto', 'unknown'],
    },
    {
      name: 'output-sourced row',
      row: {
        ...baseImageRow,
        request_id: 'req-admin-output-image',
        image_size: '4K',
        image_input_size: '1024x1024',
        image_output_size: '3840x2160',
        image_size_source: 'output',
        image_size_breakdown: { '4K': 1 },
      },
      expected: ['4K', 'Upstream output', '1024x1024', '3840x2160', '4K x 1'],
    },
    {
      name: 'input-sourced row',
      row: {
        ...baseImageRow,
        request_id: 'req-admin-input-image',
        image_size: '1K',
        image_input_size: '1024x1024',
        image_output_size: null,
        image_size_source: 'input',
      },
      expected: ['1K', 'Request input', '1024x1024', 'unknown'],
    },
    {
      name: 'legacy unstandardized row',
      row: {
        ...baseImageRow,
        request_id: 'req-admin-legacy-unstandardized-image',
        image_size: '512x512',
        image_input_size: null,
        image_output_size: null,
        image_size_source: null,
      },
      expected: ['legacy unstandardized: 512x512', 'Legacy record', 'unknown'],
    },
  ])('shows image usage metadata for $name', async ({ row, expected }) => {
    const wrapper = mount(UsageTable, {
      props: {
        data: [row],
        loading: false,
        columns: [],
      },
      global: {
        stubs: {
          DataTable: DataTableStub,
          EmptyState: true,
          Icon: true,
          Teleport: true,
        },
      },
    })

    await wrapper.find('.group.relative').trigger('mouseenter')
    await nextTick()

    const text = wrapper.text()
    expect(text).toContain('Image count')
    expect(text).toContain('Billing size')
    expect(text).toContain('Size source')
    expect(text).toContain('Input size')
    expect(text).toContain('Output size')
    expect(text).toContain('Per-image price')
    expect(text).toContain('Image total price')
    for (const value of expected) {
      expect(text).toContain(value)
    }
  })

  // A historical image record lacks an explicit billing mode and recorded size. The test
  // protects image classification while keeping unavailable size visible instead of
  // inventing a tier.
  it('displays historical image rows with missing billing_mode as image usage without a 2K fallback', async () => {
    const wrapper = mount(UsageTable, {
      props: {
        data: [
          {
            ...baseImageRow,
            request_id: 'req-admin-legacy-missing-image',
            billing_mode: null,
            image_size: null,
            image_input_size: null,
            image_output_size: null,
            image_size_source: null,
            image_size_breakdown: null,
          },
        ],
        loading: false,
        columns: [],
      },
      global: {
        stubs: {
          DataTable: DataTableStub,
          EmptyState: true,
          Icon: true,
          Teleport: true,
        },
      },
    })

    await wrapper.find('.group.relative').trigger('mouseenter')
    await nextTick()

    const text = wrapper.text()
    expect(text).toContain('Image')
    expect(text).toContain('Image count')
    expect(text).toContain('Per-image price')
    expect(text).toContain('not recorded')
    expect(text).not.toContain('(2K)')
  })
})

describe('admin UsageTable request ID column', () => {
  beforeEach(() => {
    clipboardMocks.copyToClipboard.mockReset().mockResolvedValue(true)
  })

  // The request-ID column receives a complete identifier on a usage row. The test protects
  // showing that identifier and passing its full value to the clipboard helper.
  it('renders and copies the request ID', async () => {
    const wrapper = mount(UsageTable, {
      props: {
        data: [{ ...baseImageRow, request_id: 'req-admin-visible-id' }],
        loading: false,
        columns: [{ key: 'request_id', label: 'Request ID' }],
      },
      global: {
        stubs: {
          DataTable: DataTableStub,
          EmptyState: true,
          Icon: true,
          Teleport: true,
        },
      },
    })

    expect(wrapper.text()).toContain('req-admin-visible-id')
    await wrapper.get('button[title="Copy request ID"]').trigger('click')

    expect(clipboardMocks.copyToClipboard).toHaveBeenCalledWith(
      'req-admin-visible-id',
      'Request ID copied',
    )
  })

  // An upstream identifier is present while the local request identifier is empty. The test
  // protects the independent upstream copy action and its identifier-specific completion
  // message.
  it('renders and copies the upstream ID', async () => {
    const wrapper = mount(UsageTable, {
      props: {
        data: [{ ...baseImageRow, request_id: '', upstream_request_id: '20260903082826779695' }],
        loading: false,
        columns: [{ key: 'upstream_request_id', label: 'Upstream ID' }],
      },
      global: {
        stubs: {
          DataTable: DataTableStub,
          EmptyState: true,
          Icon: true,
          Teleport: true,
        },
      },
    })

    expect(wrapper.text()).toContain('20260903082826779695')
    const copyButtons = wrapper.findAll('button[title="Copy to clipboard"]')
    expect(copyButtons).toHaveLength(1)
    await copyButtons[0].trigger('click')

    expect(clipboardMocks.copyToClipboard).toHaveBeenCalledWith(
      '20260903082826779695',
      'Upstream ID copied',
    )
  })
})
describe('admin UsageTable IP geolocation batch toolbar', () => {
  // DataTableStubWithIp renders the address slot for each fixture row so batch toolbar
  // tests can inspect page-scoped lookups and actual geographic labels together.
  const DataTableStubWithIp = {
    props: [
      'data', // supplied usage row records
    ],
    template: `
      <div>
        <div v-for="row in data" :key="row.request_id">
          <slot name="cell-ip_address" :row="row" />
        </div>
      </div>
    `,
  }

  beforeEach(() => {
    ipGeoMocks.getEntry.mockReset()
    ipGeoMocks.fetchOne.mockReset()
    ipGeoMocks.fetchBatch.mockReset()
    ipGeoMocks.getEntry.mockReturnValue({ status: 'idle' })
  })

  // The visible column list omits the IP address column. The test protects hiding the batch
  // location toolbar even when the supplied rows contain addresses.
  it('does not render the batch toolbar when the ip_address column is not visible', () => {
    const wrapper = mount(UsageTable, {
      props: {
        data: [{ request_id: 'r1', ip_address: '8.8.8.8' }],
        loading: false,
        columns: [],
      },
      global: { stubs: { DataTable: DataTableStubWithIp, EmptyState: true, Teleport: true } },
    })
    expect(wrapper.text()).not.toContain('usage.ipGeo.batchFetch')
  })

  // The IP address column is visible with pending addresses in its rows. The test protects
  // rendering pending lookup feedback and enabling the batch lookup action.
  it('renders the batch toolbar with a pending count when the ip_address column is visible', () => {
    const wrapper = mount(UsageTable, {
      props: {
        data: [
          { request_id: 'r1', ip_address: '8.8.8.8' },
          { request_id: 'r2', ip_address: '8.8.8.8' },
          { request_id: 'r3', ip_address: '1.1.1.1' },
        ],
        loading: false,
        columns: [{ key: 'ip_address', label: 'IP' }],
      },
      global: { stubs: { DataTable: DataTableStubWithIp, EmptyState: true, Teleport: true } },
    })
    expect(wrapper.text()).toContain('usage.ipGeo.pending')
    const button = wrapper.find('button')
    expect(button.exists()).toBe(true)
    expect((button.element as HTMLButtonElement).disabled).toBe(false)
  })

  // The visible page repeats an address across several usage rows. The test protects
  // deduplicating the batch payload and leaving failure events absent after successful
  // lookup.
  it('fetches deduplicated IPs from the current page when the batch button is clicked', async () => {
    ipGeoMocks.fetchBatch.mockResolvedValue(true)
    const wrapper = mount(UsageTable, {
      props: {
        data: [
          { request_id: 'r1', ip_address: '8.8.8.8' },
          { request_id: 'r2', ip_address: '8.8.8.8' },
          { request_id: 'r3', ip_address: '1.1.1.1' },
        ],
        loading: false,
        columns: [{ key: 'ip_address', label: 'IP' }],
      },
      global: { stubs: { DataTable: DataTableStubWithIp, EmptyState: true, Teleport: true } },
    })
    await wrapper.find('button').trigger('click')
    expect(ipGeoMocks.fetchBatch).toHaveBeenCalledWith(['8.8.8.8', '1.1.1.1'])
    expect(wrapper.emitted('ipGeoBatchFailed')).toBeUndefined()
  })

  // The batch location helper reports a network-level failure. The test protects emitting
  // exactly one failure event so the parent page can display its notification.
  it('emits ipGeoBatchFailed when the batch request reports a network-level failure', async () => {
    ipGeoMocks.fetchBatch.mockResolvedValue(false)
    const wrapper = mount(UsageTable, {
      props: {
        data: [{ request_id: 'r1', ip_address: '8.8.8.8' }],
        loading: false,
        columns: [{ key: 'ip_address', label: 'IP' }],
      },
      global: { stubs: { DataTable: DataTableStubWithIp, EmptyState: true, Teleport: true } },
    })
    await wrapper.find('button').trigger('click')
    expect(wrapper.emitted('ipGeoBatchFailed')).toHaveLength(1)
  })

  // A successful location cache entry exists for a usage row address. The test protects
  // rendering both the address and its resolved geographic label in the cell.
  it('renders IpGeoCell content for ip_address cells', () => {
    ipGeoMocks.getEntry.mockReturnValue({
      status: 'success',
      label: 'CN · Guangdong · Shenzhen',
      detail: {},
    })
    const wrapper = mount(UsageTable, {
      props: {
        data: [{ request_id: 'r1', ip_address: '121.35.47.43' }],
        loading: false,
        columns: [{ key: 'ip_address', label: 'IP' }],
      },
      global: { stubs: { DataTable: DataTableStubWithIp, EmptyState: true, Teleport: true } },
    })
    expect(wrapper.text()).toContain('121.35.47.43')
    expect(wrapper.text()).toContain('CN · Guangdong · Shenzhen')
  })
})

// DataTableStubWithUser renders member attribution alongside model, billing, token, and
// cost slots for deleted-user and compact-column assertions.
const DataTableStubWithUser = {
  props: [
    'data', // supplied usage row records
  ],
  template: `
    <div>
      <div v-for="row in data" :key="row.request_id">
        <slot name="cell-user" :row="row" />
        <slot name="cell-model" :row="row" :value="row.model" />
        <slot name="cell-billing_mode" :row="row" />
        <slot name="cell-tokens" :row="row" />
        <slot name="cell-cost" :row="row" />
      </div>
    </div>
  `,
}

describe('admin UsageTable deleted-user badge', () => {
  // A compact member row contains a very long email address. The test protects responsive
  // alignment classes, truncated display text, complete hover text, and visible numeric
  // identity.
  it('right-aligns compact members on mobile and constrains long emails on desktop', () => {
    const email = 'member.with.a.very.long.address@tokenrouter.example.com'
    const wrapper = mount(UsageTable, {
      props: {
        data: [{ request_id: 'req-long-email', user_id: 3559, user: { id: 3559, email } }],
        loading: false,
        columns: [{ key: 'user', label: 'User' }],
        userClickable: false,
        compactUserColumn: true,
      },
      global: {
        stubs: {
          DataTable: DataTableStubWithUser,
          EmptyState: true,
          Icon: true,
          Teleport: true,
        },
      },
    })

    expect(wrapper.get('[data-test="usage-user-cell"]').classes()).toEqual(
      expect.arrayContaining(['justify-end', 'md:w-32', 'md:justify-start']),
    )
    expect(wrapper.get('[data-test="usage-user-cell"]').classes()).not.toContain('w-32')
    expect(wrapper.get('[data-test="usage-user-email"]').classes()).toContain('truncate')
    expect(wrapper.get('[data-test="usage-user-email"]').text()).toBe('m***s')
    expect(wrapper.get('[data-test="usage-user-email"]').attributes('title')).toBe(email)
    expect(wrapper.text()).toContain('#3559')
  })

  // A member email has a single-character local part. The test protects repeating that
  // character around the mask and retaining the complete email in the hover title.
  it('uses the same first-and-last masking rule for a one-character email local part', () => {
    const wrapper = mount(UsageTable, {
      props: {
        data: [
          { request_id: 'req-short-email', user_id: 8, user: { id: 8, email: 'a@example.com' } },
        ],
        loading: false,
        columns: [{ key: 'user', label: 'User' }],
        userClickable: false,
        compactUserColumn: true,
      },
      global: {
        stubs: { DataTable: DataTableStubWithUser, EmptyState: true, Icon: true, Teleport: true },
      },
    })

    expect(wrapper.get('[data-test="usage-user-email"]').text()).toBe('a***a')
    expect(wrapper.get('[data-test="usage-user-email"]').attributes('title')).toBe('a@example.com')
  })

  // A member row includes both a custom username and email. The test protects username
  // precedence in the cell and retaining the email in the hover title.
  it('prefers a custom username while preserving the email in the title', () => {
    const wrapper = mount(UsageTable, {
      props: {
        data: [
          {
            request_id: 'req-username',
            user_id: 7,
            user: { id: 7, username: 'Ada', email: 'ada@example.com' },
          },
        ],
        loading: false,
        columns: [{ key: 'user', label: 'User' }],
        userClickable: false,
        compactUserColumn: true,
      },
      global: {
        stubs: { DataTable: DataTableStubWithUser, EmptyState: true, Icon: true, Teleport: true },
      },
    })

    expect(wrapper.get('[data-test="usage-user-email"]').text()).toBe('Ada')
    expect(wrapper.get('[data-test="usage-user-email"]').attributes('title')).toBe(
      'Ada (ada@example.com)',
    )
  })

  // A historical usage row references a soft-deleted user record. The test protects
  // rendering the deleted badge while keeping the masked member identity visible for
  // attribution.
  it('renders deleted badge for a soft-deleted user row', () => {
    const row = {
      request_id: 'req-deleted-user-1',
      model: 'claude-3',
      user_id: 2,
      user: { id: 2, email: 'd@test.com', deleted_at: '2026-05-28T00:00:00Z' },
      actual_cost: 0,
      total_cost: 0,
      input_cost: 0,
      output_cost: 0,
      rate_multiplier: 1,
      input_tokens: 1,
      output_tokens: 1,
    }

    const wrapper = mount(UsageTable, {
      props: {
        data: [row],
        loading: false,
        columns: [{ key: 'user', label: 'User' }],
      },
      global: {
        stubs: {
          DataTable: DataTableStubWithUser,
          EmptyState: true,
          Icon: true,
          Teleport: true,
        },
      },
    })

    expect(wrapper.text()).toContain('Deleted')
    expect(wrapper.text()).toContain('d***d')
  })

  // A usage row references an active user with no deletion timestamp. The test protects
  // omitting the deleted badge while retaining the normal masked identity text.
  it('does NOT render deleted badge for an active user row', () => {
    const row = {
      request_id: 'req-active-user-1',
      model: 'claude-3',
      user_id: 3,
      user: { id: 3, email: 'active@test.com', deleted_at: null },
      actual_cost: 0,
      total_cost: 0,
      input_cost: 0,
      output_cost: 0,
      rate_multiplier: 1,
      input_tokens: 1,
      output_tokens: 1,
    }

    const wrapper = mount(UsageTable, {
      props: {
        data: [row],
        loading: false,
        columns: [{ key: 'user', label: 'User' }],
      },
      global: {
        stubs: {
          DataTable: DataTableStubWithUser,
          EmptyState: true,
          Icon: true,
          Teleport: true,
        },
      },
    })

    expect(wrapper.text()).not.toContain('Deleted')
    expect(wrapper.text()).toContain('a***e')
  })
})

// Reported cache-write counts cover untagged models, partial splits, and overreported buckets
// across Claude and OpenAI families. Every positive cache-write bucket renders a concrete TTL
// tag without exposing untagged token counts.
it.each([
  { ttl: [['5m', '687']], counts: { cache_creation_5m_tokens: 687 }, total: 687 },
  { ttl: [['1h', '687']], counts: { cache_creation_1h_tokens: 687 }, total: 687 },
  {
    ttl: [
      ['5m', '80'],
      ['1h', '20'],
    ],
    counts: { cache_creation_5m_tokens: 80, cache_creation_1h_tokens: 20 },
    total: 100,
  },
  {
    ttl: [
      ['30m', '100'],
      ['12h', '200'],
    ],
    counts: { cache_creation_30m_tokens: 100, cache_creation_12h_tokens: 200 },
    total: 300,
  },
  {
    ttl: [['12h', '200']],
    counts: { cache_creation_30m_tokens: 0, cache_creation_12h_tokens: 200 },
    total: 200,
  },
  {
    ttl: [
      ['5m', '80'],
      ['1h', '20'],
    ],
    counts: { cache_creation_1h_tokens: 20 },
    total: 100,
  },
  { ttl: [['30m', '100']], counts: {}, total: 100 },
  {
    ttl: [['5m', '100']],
    counts: { model: 'claude-sonnet-4-6' },
    total: 100,
  },
  {
    ttl: [['5m', '100']],
    counts: { cache_creation_5m_tokens: 120 },
    total: 100,
  },
  { ttl: [], counts: {}, total: 0 },
])(
  'shows a concrete billed write lifetime for each positive bucket %#',
  ({ total, counts, ttl }) => {
    const wrapper = mount(UsageTable, {
      props: {
        data: [
          {
            ...baseImageRow,
            billing_mode: 'token',
            image_count: 0,
            cache_creation_tokens: total,
            ...counts,
          },
        ],
        columns: [{ key: 'tokens', label: 'Tokens' }],
        loading: false,
      },
      global: {
        stubs: { DataTable: DataTableStub, EmptyState: true, Icon: true, Teleport: true },
      },
    })

    const written = wrapper.findAll('[data-testid="cache-write-bucket"]')
    expect(
      written.map((bucket) => {
        const tag = bucket.find('[data-cache-write-ttl]')
        return [tag.exists() ? tag.text() : 'unknown', bucket.get('span').text()]
      }),
    ).toEqual(ttl)
    wrapper.unmount()
  },
)

// An administrator usage row carries cache_write_anomaly metadata while an ordinary row omits
// that field. Rendering exposes the anomaly badge and raw upstream detail tooltip only when
// cache_write_anomaly is present.
it('renders administrator cache-write anomaly badges and raw upstream tooltip details', async () => {
  const wrapper = mount(UsageTable, {
    props: {
      data: [
        {
          ...baseImageRow,
          request_id: 'req-anomaly',
          model: 'claude-sonnet-4-6',
          billing_mode: 'token',
          image_count: 0,
          cache_creation_tokens: 311,
          cache_creation_5m_tokens: 311,
          cache_creation_1h_tokens: 0,
          cache_write_anomaly: {
            kind: 'partial_breakdown' as const,
            reported_total: 311,
            reported_5m: 211,
            reported_1h: 0,
          },
        },
      ],
      columns: [{ key: 'tokens', label: 'Tokens' }],
      loading: false,
    },
    global: {
      stubs: { DataTable: DataTableStub, EmptyState: true, Icon: true, Teleport: true },
    },
  })

  const badge = wrapper.get('[data-testid="cache-write-anomaly-badge"]')
  expect(badge.text()).toBe('Anomaly')
  expect(badge.attributes('title')).toBe('Partial breakdown (raw total:311, 5m:211, 1h:0)')

  await wrapper.find('.group.relative').trigger('mouseenter')
  await nextTick()

  const detail = wrapper.get('[data-testid="cache-write-anomaly-detail"]')
  expect(detail.text()).toContain('Cache Write Anomaly')
  expect(detail.text()).toContain('Partial split')
  expect(detail.text()).toContain('Reported total')
  expect(detail.text()).toContain('311')
  expect(detail.text()).toContain('Reported 5m / 1h')
  expect(detail.text()).toContain('211 / 0')

  await wrapper.setProps({
    data: [
      {
        ...baseImageRow,
        request_id: 'req-openai-anomaly',
        model: 'gpt-6.2-sol',
        billing_mode: 'token',
        image_count: 0,
        cache_creation_tokens: 12000,
        cache_creation_30m_tokens: 9000,
        cache_creation_12h_tokens: 3000,
        cache_write_anomaly: {
          kind: 'partial_breakdown' as const,
          reported_total: 12000,
          reported_30m: 6000,
          reported_12h: 3000,
        },
      },
    ],
  })
  await wrapper.find('.group.relative').trigger('mouseenter')
  await nextTick()
  const openaiDetail = wrapper.get('[data-testid="cache-write-anomaly-detail"]')
  expect(openaiDetail.text()).toContain('Reported 30m / 12h')
  expect(openaiDetail.text()).toContain('6,000 / 3,000')

  await wrapper.setProps({
    data: [
      {
        ...baseImageRow,
        request_id: 'req-normal',
        model: 'claude-sonnet-4-6',
        billing_mode: 'token',
        image_count: 0,
        cache_creation_tokens: 311,
        cache_creation_5m_tokens: 311,
        cache_creation_1h_tokens: 0,
      },
    ],
  })
  expect(wrapper.find('[data-testid="cache-write-anomaly-badge"]').exists()).toBe(false)
  wrapper.unmount()
})

// Compact fixtures exercise the actual row slots in their canonical column order.
// The caller may override either the record or the administrator billing flags.
const CompactSlotTable = {
  props: ['columns', 'data', 'density'],
  template: `
    <div :data-density="density">
      <div v-for="row in data" :key="row.id">
        <div v-for="column in columns" :key="column.key" :data-cell="column.key">
          <slot :name="'cell-' + column.key" :row="row" />
        </div>
      </div>
    </div>
  `,
}

// compactUsageRow combines user-visible usage with administrator-only diagnostics
// to prove that the shared table does not expose those fields in other layouts.
const compactUsageRow = {
  ...baseImageRow,
  id: 19,
  platform: 'openai',
  created_at: '2025-11-25T14:06:18Z',
  request_id: 'req-1234567890-complete',
  upstream_request_id: 'upstream-1234567890-complete',
  user_id: 7,
  user: { id: 7, email: 'ada@example.test', deleted_at: '2025-11-01T00:00:00Z' },
  api_key: { name: 'Team Key' },
  group: { name: 'Team Group' },
  provider: { id: 2, name: 'Private provider' },
  inbound_endpoint: '/v1/responses',
  upstream_endpoint: '/private/upstream/endpoint',
  model: 'requested-model',
  model_mapping_chain:
    'requested-model → first-route-model → a-long-intermediate-route-model → routed-model',
  upstream_model: 'routed-model',
  upstream_model_mismatch: true,
  upstream_response_model: 'actual-model',
  requested_reasoning_effort: 'high',
  reasoning_effort: 'low',
  billing_mode: 'token',
  billing_type: 0,
  actual_cost: 0.25,
  total_cost: 0.8,
  provider_stats_cost: 0.97,
  provider_rate_multiplier: 2,
  input_cost: 0.15,
  input_tokens: 160,
  output_tokens: 42,
  cache_read_tokens: 8,
  cache_creation_tokens: 10,
  cache_creation_30m_tokens: 7,
  cache_creation_12h_tokens: 3,
  cache_ttl_overridden: true,
  cache_write_anomaly: { kind: 'partial_breakdown', reported_total: 10, reported_30m: 7 },
  duration_ms: 2020,
  ttft_ms: 804,
  ttft_mode: 'first_event',
  tps: 0,
  detailed_timing: { provider_slot_acquired_ms: 120 },
  user_agent: 'openai-python/1.68.2 Python/3.12 (extra data)',
  ip_address: '2001:db8:1111:2222:3333:4444:5555:6666',
} as AdminUsageLog

// mountCompactUsage keeps the real clipboard buttons and disclosure controls mounted.
function mountCompactUsage(
  role: UsageTableRole,
  row: AdminUsageLog = compactUsageRow,
  flags: { showProviderBilling?: boolean; showStandardCost?: boolean } = {},
) {
  const pinia = createPinia()
  pinia.state.value.auth = { user: { role: role === 'admin' ? 'admin' : 'user' } }
  return mount(UsageTable, {
    attachTo: document.body,
    props: {
      compactRole: role,
      columns: getUsageTableColumns(role, (key) => messages[key] ?? key),
      data: [row],
      showIpGeoToolbar: false,
      ...flags,
    },
    global: {
      plugins: [pinia],
      stubs: {
        DataTable: CompactSlotTable,
        Icon: true,
        RouterLink: RouterLinkStub,
        Teleport: true,
        MotionTransition: { inheritAttrs: false, template: '<slot />' },
      },
    },
  })
}

describe('compact UsageTable', () => {
  beforeEach(() => {
    clipboardMocks.copyToClipboard.mockReset().mockResolvedValue(true)
    ipGeoMocks.getEntry.mockReturnValue({ status: 'idle' })
    ipGeoMocks.fetchOne.mockReset().mockResolvedValue(true)
  })

  // An administrator can inspect long model mappings and timing, navigate to the
  // user balance, copy exact header values, and query an IPv6 address in one row.
  it('keeps administrator details and original values reachable in compact cells', async () => {
    const wrapper = mountCompactUsage('admin')
    try {
      expect(wrapper.get('[data-density]').attributes('data-density')).toBe('compact')
      expect(wrapper.findAll('[data-cell]')).toHaveLength(9)
      const time = wrapper.get('[data-cell="time"] time')
      expect(time.findAll('span')).toHaveLength(2)
      expect(time.attributes('datetime')).toBe(compactUsageRow.created_at)
      expect(time.attributes('title')).toMatch(/^2025-11-\d\d \d\d:\d\d:\d\d$/)
      expect(wrapper.get('[data-cell="caller"]').text()).toMatch(/a\*\*\*a\s*#7\s*Deleted/)
      await wrapper.get('button[data-test="usage-user-email"]').trigger('click')
      expect(wrapper.emitted('userClick')).toEqual([[7, 'ada@example.test']])
      const route = wrapper.get('[data-cell="route"]')
      expect(route.text()).toContain('Private provider')
      expect(route.get('[title="/private/upstream/endpoint"]').exists()).toBe(true)

      const model = wrapper.get('[data-cell="model_spec"]')
      expect(model.get('[data-testid="upstream-response-model"]').text()).toContain('actual-model')
      const mapping = model.get('button[aria-label="Model mapping"]')
      await mapping.trigger('click')
      expect(mapping.attributes('aria-expanded')).toBe('true')
      expect(
        model
          .get('[data-testid="usage-model-details"]')
          .findAll('ol li')
          .map((step) => step.text()),
      ).toEqual([
        'requested-model',
        'first-route-model',
        'a-long-intermediate-route-model',
        'routed-model',
      ])
      expect(model.get('[data-testid="usage-model-details"]').text()).toMatch(/high\s*→\s*low/i)
      document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape' }))

      const tokens = wrapper.get('[data-cell="tokens"]')
      expect(
        tokens
          .get('dl')
          .findAll('dd')
          .map((count) => count.text()),
      ).toEqual(['160', '42', '8', '10'])
      expect(tokens.text()).toContain('30m 7 · 12h 3')
      expect(tokens.get('[data-testid="cache-write-anomaly-badge"]').exists()).toBe(true)
      const tokenDetails = tokens.get('button[aria-label="usage.tokenDetails"]')
      await tokenDetails.trigger('focus')
      expect(wrapper.get('[data-testid="cache-write-anomaly-detail"]').text()).toContain(
        'Partial split',
      )
      await tokenDetails.trigger('keydown', { key: 'Escape' })
      expect(wrapper.find('[data-testid="token-detail-tooltip"]').exists()).toBe(false)

      const performance = wrapper.get('[data-cell="performance"]')
      expect(performance.text()).toContain('0.0 tok/s')
      expect(performance.text()).toContain('804ms')
      const timing = performance.get('button[aria-label="Detailed Timing"]')
      await timing.trigger('focus')
      expect(timing.attributes('aria-describedby')).toBe(
        wrapper.get('[data-testid="timing-detail-tooltip"]').attributes('id'),
      )
      await timing.trigger('keydown', { key: 'Escape' })
      expect(wrapper.find('[data-testid="timing-detail-tooltip"]').exists()).toBe(false)

      const trace = wrapper.get('[data-cell="request_trace"]')
      expect(trace.text()).toContain('req-12…lete')
      expect(trace.text()).toContain('upstre…lete')
      expect(trace.getComponent(RouterLinkStub).props('to')).toEqual({
        path: '/admin/requests',
        query: { request_id: compactUsageRow.request_id },
      })
      await trace.get('button[title="Copy request ID"]').trigger('click')
      await trace.get('button[aria-label="Copy upstream request ID"]').trigger('click')
      const network = wrapper.get('[data-cell="client_network"]')
      expect(network.get('[title="' + compactUsageRow.ip_address + '"]').text()).toBe(
        compactUsageRow.ip_address,
      )
      const ua = network.get('[title="' + compactUsageRow.user_agent + '"]')
      expect(ua.text()).toBe('openai-python/1.68.2')
      await network.get('button[aria-label="Copy user agent"]').trigger('click')
      await network
        .findAll('button')
        .find((button) => button.text() === 'usage.ipGeo.fetch')!
        .trigger('click')
      expect(clipboardMocks.copyToClipboard.mock.calls.map(([value]) => value)).toEqual([
        compactUsageRow.request_id,
        compactUsageRow.upstream_request_id,
        compactUsageRow.user_agent,
      ])
      expect(ipGeoMocks.fetchOne).toHaveBeenCalledWith(compactUsageRow.ip_address)
    } finally {
      wrapper.unmount()
    }
  })

  // Team owners see read-only member attribution and personal accounts see the
  // key and group. Neither layout may expose administrator-only data by any control.
  it.each(['teamOwner', 'personal'] as const)(
    'scopes sensitive fields for %s layout',
    async (role) => {
      const wrapper = mountCompactUsage(role)
      try {
        expect(wrapper.find('[data-test="usage-user-email"]').exists()).toBe(role === 'teamOwner')
        expect(wrapper.find('button[data-test="usage-user-email"]').exists()).toBe(false)
        expect(wrapper.get('[data-cell="caller"]').text()).toContain('Team Key')
        expect(wrapper.get('[data-cell="route"]').text()).toBe('/v1/responses')
        expect(wrapper.find('[data-testid="upstream-response-model"]').exists()).toBe(false)
        expect(wrapper.find('[data-testid="cache-write-anomaly-badge"]').exists()).toBe(false)
        expect(wrapper.find('button[aria-label="Detailed Timing"]').exists()).toBe(false)
        expect(wrapper.find('button[aria-label="Copy upstream request ID"]').exists()).toBe(false)
        expect(wrapper.get('[data-cell="request_trace"]').find('a').exists()).toBe(false)
        expect(wrapper.html()).not.toContain(compactUsageRow.upstream_request_id)
        expect(wrapper.html()).not.toContain('Private provider')

        const cost = wrapper.get('[data-cell="cost"]')
        expect(cost.text()).toContain('$0.250000')
        expect(cost.text()).toContain('Balance')
        expect(cost.find('[data-testid="usage-original-cost"]').exists()).toBe(false)
        await cost.get('button[aria-label="Cost Breakdown"]').trigger('focus')
        const details = wrapper.get('[data-testid="cost-detail-tooltip"]').text()
        expect(details).toContain('$0.250000')
        expect(details).not.toMatch(/\$0\.800000|\$0\.150000|\$1\.940000|Provider billed|Original/)
      } finally {
        wrapper.unmount()
      }
    },
  )

  // An image record exposes its measured dimensions and frame count to the user
  // while the charge details retain only the user's actual amount.
  it('keeps image observations available without exposing standard image prices', async () => {
    const wrapper = mountCompactUsage('personal', {
      ...compactUsageRow,
      billing_mode: 'image',
      image_frame_count: 12,
      image_size_source: 'output',
      image_input_size: '1024x1024',
      image_output_size: '1536x1024',
    })
    try {
      const tokens = wrapper.get('[data-cell="tokens"]')
      expect(tokens.text()).toContain('2 images · 12 frames')
      expect(tokens.text()).toContain('2K')
      expect(tokens.find('button').exists()).toBe(false)

      await wrapper.get('button[aria-label="Cost Breakdown"]').trigger('focus')
      const detail = wrapper.get('[data-testid="cost-detail-tooltip"]').text()
      expect(detail).toContain('Upstream output')
      expect(detail).toContain('1536x1024')
      expect(detail).toContain('$0.250000')
      expect(detail).not.toMatch(/Per-image price|Image total price|Original|Provider billed/)
    } finally {
      wrapper.unmount()
    }
  })

  // Only numeric 0 and 1 identify funding. Other values cannot imply a balance
  // charge or change the stored mode displayed in the model cell.
  it.each([
    [0, 'Balance'],
    [1, 'Subscription'],
    [undefined, undefined],
    [2, undefined],
    ['0', undefined],
    ['1', undefined],
  ] as const)('uses only explicit numeric funding code %s', (billingType, label) => {
    const row = {
      ...compactUsageRow,
      billing_type: billingType as number,
      billing_mode: 'per_request',
    }
    const wrapper = mountCompactUsage('admin', row)
    try {
      const funding = wrapper.find('[data-testid="usage-billing-funding"]')
      expect(funding.exists() ? funding.text() : undefined).toBe(label)
      expect(wrapper.get('[data-cell="model_spec"]').text()).toContain('Per request')
      expect(wrapper.get('[data-cell="tokens"]').text()).toContain('2 images')
      expect(
        wrapper
          .get('[data-cell="tokens"]')
          .find('button[aria-label="usage.tokenDetails"]')
          .exists(),
      ).toBe(false)
    } finally {
      wrapper.unmount()
    }
  })

  // System costs keep the state reported by the server and never acquire customer
  // billing labels, even when a row carries a default numeric funding field.
  it.each([
    ['unknown', 'Unknown', false],
    ['partial', 'Partial', true],
    ['calculated', '$1.940000', true],
  ] as const)(
    'uses recorded %s cost state for a system request',
    async (state, summary, amountVisible) => {
      const row = {
        ...compactUsageRow,
        request_source: 'provider_test',
        system_usage: {
          parent_request_id: 'test-run',
          outcome: 'ok',
          usage_state: 'complete',
          cost_state: state,
        },
      }
      const wrapper = mountCompactUsage('admin', row)
      try {
        expect(wrapper.get('[data-cell="caller"]').text()).toContain('Manual provider test')
        const cost = wrapper.get('[data-cell="cost"]')
        expect(cost.text()).toContain(summary)
        expect(cost.text().includes('$1.940000')).toBe(amountVisible)
        expect(cost.find('[data-testid="usage-billing-funding"]').exists()).toBe(false)
        await cost.get('button[aria-label="Cost Breakdown"]').trigger('click')
        const details = wrapper.get('[data-testid="cost-detail-tooltip"]').text()
        expect(details).toContain(summary)
        expect(details.includes('$1.940000')).toBe(amountVisible)
        expect(details).not.toMatch(/User billed|\$0\.250000|\$0\.800000/)
      } finally {
        wrapper.unmount()
      }
    },
  )

  // Administrators can independently opt out of the provider charge, the standard
  // amount, or both, including the detail panel.
  it.each([
    [true, false],
    [false, true],
  ])(
    'applies separate provider and standard billing flags (%s, %s)',
    async (showProviderBilling, showStandardCost) => {
      const wrapper = mountCompactUsage('admin', compactUsageRow, {
        showProviderBilling,
        showStandardCost,
      })
      try {
        const cost = wrapper.get('[data-cell="cost"]')
        expect(cost.find('[data-testid="usage-original-cost"]').exists()).toBe(showStandardCost)
        expect(cost.text().includes('$1.940000')).toBe(showProviderBilling)
        await cost.get('button[aria-label="Cost Breakdown"]').trigger('focus')
        const details = wrapper.get('[data-testid="cost-detail-tooltip"]').text()
        expect(details.includes('$1.940000')).toBe(showProviderBilling)
        expect(details.includes('$0.800000')).toBe(showStandardCost)
      } finally {
        wrapper.unmount()
      }
    },
  )
})
