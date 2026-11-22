import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import { apiClient } from '@/api/client'
import type { AdvancedSchedulerScoreDiagnosticResponse } from '@/api/admin/providers.types'
import { localizedErrorMessage } from '@/i18n/errors'
import en from '@/i18n/locales/en'
import zh from '@/i18n/locales/zh'
import ModelExclusionsView from '../ModelExclusionsView.vue'

// ModelExclusionsView.spec.ts exercises the administrator's eligibility query
// through the mounted form and the provider API. An in-memory HTTP adapter
// records every request and controls when diagnostics arrive, so edits, retries
// and late replies can be checked against the text visible to the operator.

// The adapter records the serialized request after the real provider API has
// selected the HTTP method and URL. Each test supplies its own response.
const originalAdapter = apiClient.defaults.adapter
const request =
  vi.fn<
    (
      method: string | undefined,
      url: string | undefined,
      body: unknown,
    ) => Promise<AdvancedSchedulerScoreDiagnosticResponse>
  >()
const previewURL = '/admin/providers/731/advanced-scheduler-score/preview'
const defaultBody = {
  group_id: 827,
  requested_model: 'public-alias',
  client_protocol: 'openai_responses',
}

// The overview can report an eligible group even when the server omits its
// detail. The view must obtain the decision for this query from detail.
const overview: AdvancedSchedulerScoreDiagnosticResponse = {
  provider: { id: 731, name: 'West relay', platform: 'openai', type: 'apikey', status: 'active' },
  groups: [{ id: 827, name: 'Destination', eligible: true, status: 'eligible' }],
  generated_at: '2026-11-22T10:00:01Z',
  calculation_version: 'v1',
}

// diagnostic supplies the required scheduler detail around the fields under
// test. Callers provide the admission verdict and any route or hard reasons
// returned for that particular preview.
function diagnostic(
  eligible: boolean,
  evidence: Pick<
    NonNullable<typeof overview.detail>,
    'hard_filter_reasons' | 'plan_model_check'
  > = {},
): AdvancedSchedulerScoreDiagnosticResponse {
  return {
    ...overview,
    detail: {
      eligible,
      group: { id: 827, name: 'Destination' },
      context: { baseline: false, requested_model: 'public-alias' },
      candidate_pool: {
        total_candidates: 1,
        eligible_candidates: Number(eligible),
        excluded_candidates: Number(!eligible),
        exclusion_reasons: {},
        top_k: 1,
        normalization_ranges: { priority_min: 1, priority_max: 1, max_waiting_count: 0 },
        candidates: [],
      },
      metrics: [],
      effective_settings: [],
      policy_signals: [],
      ...evidence,
    },
  }
}

// mountPage installs the selected production catalog and keeps the real
// protocol Select. Its menu renders inline for jsdom interaction, and the
// shared teardown unmounts each page after the test.
function mountPage(locale: 'en' | 'zh' = 'en') {
  const i18n = createI18n({ legacy: false, locale, messages: { en, zh } })
  return mount(ModelExclusionsView, {
    global: {
      plugins: [i18n],
      stubs: {
        AppLayout: { template: '<main><slot /></main>' },
        MotionTransition: { inheritAttrs: false, template: '<slot />' },
        teleport: true,
        Icon: true,
      },
    },
  })
}

// fillForm supplies a valid scenario with padding around its requested model.
async function fillForm(page: VueWrapper) {
  await page.get('#exclusion-provider').setValue('731')
  await page.get('#exclusion-group').setValue('827')
  await page.get('#exclusion-model').setValue('  public-alias  ')
}

// chooseMessages opens the shared protocol Select and activates its Messages option.
async function chooseMessages(page: VueWrapper) {
  await page.get('#exclusion-protocol').trigger('keydown', { key: 'ArrowDown' })
  const messages = page
    .get('[role="listbox"]')
    .findAll('[role="option"]')
    .find((option) => option.text() === 'Messages')
  expect(messages).toBeDefined()
  await messages!.trigger('click')
}

// submit dispatches the form event and drains work that can finish immediately.
async function submit(page: VueWrapper) {
  await page.get('form').trigger('submit')
  await flushPromises()
}

enableAutoUnmount(afterEach)

beforeEach(() => {
  request.mockReset()
  apiClient.defaults.adapter = async (config) => ({
    config,
    status: 200,
    statusText: 'OK',
    headers: {},
    data: {
      code: 0,
      data: await request(config.method, config.url, config.data && JSON.parse(config.data)),
    },
  })
})

afterEach(() => {
  apiClient.defaults.adapter = originalAdapter
  vi.restoreAllMocks()
  for (const [method, url] of request.mock.calls) {
    expect(method).toBe('post')
    expect(url).toMatch(/^\/admin\/providers\/\d+\/advanced-scheduler-score\/preview$/)
  }
})

describe('ModelExclusionsView', () => {
  // Mounting and completing a valid scenario leave the operator in the idle
  // state. Only submission posts the trimmed model and default protocol.
  it('等待提交，按默认协议查询，并区分无详情与可调度', async () => {
    request.mockResolvedValue(overview)
    const page = mountPage()
    const labels = en.admin.providers.modelExclusions
    await flushPromises()
    expect(page.text()).toContain(labels.idle)
    expect(page.get('#exclusion-protocol').text()).toBe('Responses')
    expect(page.get('#exclusion-source').element).toHaveProperty('value', '')
    expect(request).not.toHaveBeenCalled()
    await fillForm(page)
    await flushPromises()
    expect(request).not.toHaveBeenCalled()

    await submit(page)
    expect(request).toHaveBeenCalledExactlyOnceWith('post', previewURL, defaultBody)
    expect(page.text()).toContain(labels.noDetail)
    for (const other of [labels.idle, labels.eligible, labels.excluded, labels.noReasons]) {
      expect(page.text()).not.toContain(other)
    }
  })

  // Both catalogs expose the same submission and verdict states. An eligible
  // detail needs no explanation, while an excluded detail can lack a reason.
  it.each([
    ['en', 'Check eligibility', 'This provider is eligible for this model.', 'Provider excluded'],
    ['zh', '查询排除原因', '此提供商可调度该模型。', '此提供商已被排除'],
  ] as const)(
    '%s 显示提交、可调度和无具体原因的排除状态',
    async (locale, inspect, eligible, excluded) => {
      const labels = { en, zh }[locale].admin.providers.modelExclusions
      request
        .mockResolvedValueOnce(diagnostic(true))
        .mockResolvedValueOnce(diagnostic(false, { hard_filter_reasons: [] }))
      const page = mountPage(locale)
      expect(page.get('button[type="submit"]').text()).toBe(inspect)
      expect(page.get('label[for="exclusion-provider"]').text()).toBe(labels.provider)
      expect(page.get('label[for="exclusion-group"]').text()).toBe(labels.group)
      await fillForm(page)
      await submit(page)
      expect(page.text()).toContain(eligible)
      expect(page.text()).not.toContain(excluded)
      expect(page.text()).not.toContain(labels.noDetail)
      expect(page.text()).not.toContain(labels.noReasons)

      await submit(page)
      expect(page.text()).toContain(excluded)
      expect(page.text()).toContain(labels.noReasons)
      expect(page.text()).not.toContain(eligible)
      expect(request).toHaveBeenCalledTimes(2)
    },
  )

  // A routed request returns two owners and two hard reasons. The page
  // identifies the provider, shows the scheduler's route and preserves unfamiliar codes.
  it('按所选来源组与协议查询，显示服务端路由、归属及原始未知原因', async () => {
    const route = {
      source_group_id: 649,
      group_id: 827,
      client_protocol: 'anthropic_messages',
      upstream_protocol: 'openai_chat_completions',
      requested_model: 'public-alias',
      routing_model: 'group-alias',
      actual_model: 'wire-model',
      checked_at: '2026-11-22T09:45:07Z',
      enabled: true,
      owners: [
        {
          plan_id: 512,
          result_id: 901,
          policy_version: 17,
          started_at: '2026-11-22T08:20:00Z',
          reason: 'incorrect_answer',
        },
        {
          plan_id: 513,
          result_id: 902,
          policy_version: 18,
          started_at: '2026-11-22T08:30:00Z',
          reason: 'timeout',
        },
      ],
    }
    request.mockResolvedValue(
      diagnostic(false, {
        plan_model_check: route,
        hard_filter_reasons: ['plan_model_restricted', 'future_gate'],
      }),
    )
    const page = mountPage()
    await fillForm(page)
    await page.get('#exclusion-source').setValue('649')
    await chooseMessages(page)
    expect(request).not.toHaveBeenCalled()
    await submit(page)
    expect(request).toHaveBeenCalledExactlyOnceWith('post', previewURL, {
      ...defaultBody,
      source_group_id: 649,
      client_protocol: 'anthropic_messages',
    })

    const labels = en.admin.providers.modelExclusions
    const text = page.text()
    for (const value of [
      overview.provider.name,
      '731',
      labels.excluded,
      labels.route,
      '649 → 827',
      labels.upstream,
      'anthropic_messages → openai_chat_completions',
      route.requested_model,
      labels.routingModel,
      route.routing_model,
      route.actual_model,
      labels.checkedAt,
      route.checked_at,
    ]) {
      expect(text).toContain(value)
    }
    const items = page.findAll('li').map((item) => item.text())
    expect(items).toContain(
      en.admin.providers.advancedSchedulerScore.filterReasons.plan_model_restricted,
    )
    expect(items).toContain('future_gate')
    for (const owner of route.owners) {
      const item = items.find((line) => line.includes(owner.reason))
      expect(item).toContain(String(owner.plan_id))
      expect(item).toContain(String(owner.result_id))
      expect(item).toContain(String(owner.policy_version))
    }
    expect(text).not.toContain(labels.noReasons)
  })

  // A failed preview leaves the same scenario available for resubmission.
  // The next response clears the alert and publishes the recovered eligibility decision.
  it('查询失败后可原样重试，成功后清除错误', async () => {
    request.mockRejectedValueOnce(new Error('offline')).mockResolvedValueOnce(diagnostic(true))
    const page = mountPage()
    await fillForm(page)
    await submit(page)
    expect(page.get('[role="alert"]').text()).toBe(localizedErrorMessage('NETWORK_ERROR', 0))
    expect(page.get('button[type="submit"]').element).toHaveProperty('disabled', false)
    await submit(page)
    expect(request.mock.calls).toEqual([
      ['post', previewURL, defaultBody],
      ['post', previewURL, defaultBody],
    ])
    expect(page.find('[role="alert"]').exists()).toBe(false)
    expect(page.text()).toContain(en.admin.providers.modelExclusions.eligible)
  })

  // An API rejection can lack a displayable message. The view falls back
  // to its own localized error so the operator still sees a reason to retry.
  it('预览接口没有返回错误文案时显示本页提示', async () => {
    const preview = vi.spyOn(adminAPI.providers, 'previewAdvancedSchedulerScore')
    preview.mockRejectedValueOnce({})
    const page = mountPage('zh')
    await fillForm(page)
    await submit(page)
    expect(preview).toHaveBeenCalledExactlyOnceWith(731, defaultBody)
    expect(page.get('[role="alert"]').text()).toBe(zh.admin.providers.modelExclusions.failed)
  })

  // Editing any member of the scenario invalidates its pending preview.
  // Without a second submission the old reply must leave the page idle.
  it.each([
    ['provider', '732'],
    ['group', '828'],
    ['source', '649'],
    ['model', 'new-alias'],
    ['protocol', 'Messages'],
  ])('改动 %s 后丢弃旧结果', async (field, value) => {
    const pending = Promise.withResolvers<AdvancedSchedulerScoreDiagnosticResponse>()
    request.mockReturnValueOnce(pending.promise)
    const page = mountPage()
    await fillForm(page)
    await submit(page)
    expect(request).toHaveBeenCalledExactlyOnceWith('post', previewURL, defaultBody)
    if (field === 'protocol') await chooseMessages(page)
    else await page.get(`#exclusion-${field}`).setValue(value)

    pending.resolve(diagnostic(false, { hard_filter_reasons: ['obsolete_reason'] }))
    await flushPromises()
    expect(page.text()).toContain(en.admin.providers.modelExclusions.idle)
    expect(page.text()).not.toContain(en.admin.providers.modelExclusions.excluded)
    expect(page.text()).not.toContain('obsolete_reason')
    expect(request).toHaveBeenCalledTimes(1)
  })

  // The replacement preview succeeds while its predecessor is still pending.
  // Rejecting the first request afterwards must leave the newer decision visible.
  it('较早请求的错误不会覆盖新查询', async () => {
    const first = Promise.withResolvers<AdvancedSchedulerScoreDiagnosticResponse>()
    request.mockReturnValueOnce(first.promise).mockResolvedValueOnce(diagnostic(true))
    const page = mountPage()
    await fillForm(page)
    await submit(page)
    await page.get('#exclusion-model').setValue('second-model')
    await submit(page)
    expect(request).toHaveBeenNthCalledWith(2, 'post', previewURL, {
      ...defaultBody,
      requested_model: 'second-model',
    })
    first.reject(new Error('obsolete failure'))
    await flushPromises()
    expect(page.find('[role="alert"]').exists()).toBe(false)
    expect(page.text()).toContain(en.admin.providers.modelExclusions.eligible)
  })

  // Each identifier must be a positive safe integer when supplied. Dispatching
  // submit directly checks the validation boundary even when browser constraints are bypassed.
  it.each(['provider', 'group', 'source'])('拦截 %s 的非正数、小数及越界整数', async (field) => {
    const page = mountPage()
    await fillForm(page)
    const invalid = ['0', '-1', '1.5', '9007199254740992']
    if (field !== 'source') invalid.push('')
    for (const value of invalid) {
      await page.get(`#exclusion-${field}`).setValue(value)
      await submit(page)
      expect(request, `${field}=${value}`).not.toHaveBeenCalled()
    }
  })

  // Empty and whitespace-only models cannot identify a scheduler scenario.
  // A populated provider and destination still produce zero previews until a model is supplied.
  it.each(['', '   '])('拦截空白模型 %j', async (value) => {
    const page = mountPage()
    await fillForm(page)
    await page.get('#exclusion-model').setValue(value)
    await submit(page)
    expect(request).not.toHaveBeenCalled()
  })
})
