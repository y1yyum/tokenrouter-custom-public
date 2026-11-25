// ProviderCompactCells.spec.ts checks detail reachability without adding lines to the row.
import { mount } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { describe, expect, it, vi } from 'vitest'
import type { Provider } from '@/api/admin/providers.types'
import type { Group } from '@/api/groups.types'
import ProviderCapacityCell from '../ProviderCapacityCell.vue'
import ProviderGroupsCell from '../ProviderGroupsCell.vue'
import ProviderStatusIndicator from '../ProviderStatusIndicator.vue'

vi.mock('vue-i18n', async () => ({
  ...(await vi.importActual('vue-i18n')),
  useI18n: () => ({
    t: (key: string, values?: Record<string, unknown>) =>
      values ? [key, ...Object.values(values)].join(' ') : key,
  }),
}))

// provider supplies the fields relevant to an active row's capacity and restrictions.
const provider = {
  id: 8,
  platform: 'openai',
  type: 'apikey',
  status: 'active',
  schedulable: true,
  concurrency: 8,
  current_concurrency: 0,
} as Provider

describe('compact provider cells', () => {
  // Six groups produce one summary badge and five hidden names in a dismissible panel.
  it('opens all groups and returns focus to the overflow trigger on Escape', async () => {
    const groups = Array.from({ length: 6 }, (_, i) => ({
      id: i,
      name: 'Group ' + (i + 1) + ' with a long name',
      rate_multiplier: 1,
    })) as Group[]
    const wrapper = mount(ProviderGroupsCell, {
      attachTo: document.body,
      props: { groups, compact: true },
    })

    const trigger = wrapper.get('button')
    expect(trigger.text()).toBe('+5')
    expect(wrapper.text()).toContain(groups[0].name)
    expect(wrapper.text()).not.toContain(groups[5].name)
    await trigger.trigger('click')

    const panel = document.getElementById(trigger.attributes('aria-controls')!)!
    expect(document.activeElement).toBe(panel)
    expect(Array.from(panel.querySelectorAll('li'), (entry) => entry.textContent)).toEqual(
      groups.map(({ name }) => name),
    )
    panel.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }))
    expect(document.activeElement).toBe(trigger.element)
    wrapper.unmount()
  })

  // The three visible capacity entries remain bounded, and the detail keeps its USD labels.
  it('shows monthly and total USD limits in the capacity details', async () => {
    const wrapper = mount(ProviderCapacityCell, {
      props: {
        compact: true,
        provider: {
          ...provider,
          quota_daily_limit: 15,
          quota_weekly_limit: 25,
          quota_monthly_limit: 40,
          quota_monthly_used: 5,
          quota_limit: 125,
          quota_used: 100,
        },
      },
      global: { plugins: [createPinia()] },
    })

    const trigger = wrapper.get('button')
    expect(trigger.text()).toBe('+2')
    await trigger.trigger('click')
    const panel = document.getElementById(trigger.attributes('aria-controls')!)!
    expect(panel.textContent).toContain('admin.providers.compact.monthlyQuota')
    expect(panel.textContent).toContain('$40.00')
    expect(panel.textContent).toContain('admin.providers.compact.totalQuota')
    expect(panel.textContent).toContain('$125.00')
    wrapper.unmount()
  })

  // A global cooldown can take visual priority while temporary restriction details remain
  // available through the same parent event.
  it('retains temporary recovery details during a global rate limit', async () => {
    const wrapper = mount(ProviderStatusIndicator, {
      props: {
        compact: true,
        provider: {
          ...provider,
          rate_limit_reset_at: '2099-07-20T00:00:00Z',
          temp_unschedulable_until: '2099-07-21T00:00:00Z',
        },
      },
    })

    expect(wrapper.text()).toContain('admin.providers.status.rateLimited')
    const opener = wrapper.get('button[aria-label="admin.providers.compact.statusDetails"]')
    await opener.trigger('click')
    const panel = document.getElementById(opener.attributes('aria-controls')!)!
    const action = Array.from(panel.querySelectorAll('button')).find((button) =>
      button.textContent?.includes('admin.providers.status.viewTempUnschedDetails'),
    )
    action?.click()
    expect(wrapper.emitted('show-temp-unsched')?.[0]).toEqual([wrapper.props('provider')])
    wrapper.unmount()
  })
})

