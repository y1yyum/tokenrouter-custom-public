import { flushPromises, shallowMount } from '@vue/test-utils'
import { expect, it, vi } from 'vitest'
import ScheduledTestsPanel from '../ScheduledTestsPanel.vue'

// Capture the public admin request and leave every test plan in local memory.
const create = vi.hoisted(() => vi.fn().mockResolvedValue({}))
vi.mock('@/api/admin', () => ({
  adminAPI: { scheduledTests: { create, listByProvider: async () => [] } },
}))
vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showSuccess: vi.fn(), showError: vi.fn() }),
}))
vi.mock('vue-i18n', async (original) => ({
  ...(await original<typeof import('vue-i18n')>()),
  useI18n: () => ({ t: (key: string) => key }),
}))

// Select quality through the visible form. Reopening another provider must
// clear the draft and restore the ordinary connectivity controls.
it('opts in explicitly and clears quality mode when switching providers', async () => {
  const wrapper = shallowMount(ScheduledTestsPanel, {
    props: { show: true, providerId: 1, qualitySupported: true, modelOptions: [] },
    global: {
      stubs: {
        BaseDialog: { template: '<div><slot /></div>' },
        Collapse: { props: ['open'], template: '<div v-if="open"><slot /></div>' },
      },
    },
  })
  const click = async (label: string) => {
    const button = wrapper.findAll('button').find((item) => item.text().includes(label))
    expect(button).toBeDefined()
    await button!.trigger('click')
  }
  await click('admin.scheduledTests.addPlan')
  const quality = wrapper
    .findAll('label')
    .find((item) => item.text().includes('admin.scheduledTests.qualityProbe'))!
  quality.findComponent({ name: 'Toggle' }).vm.$emit('update:modelValue', true)
  wrapper.findComponent({ name: 'Select' }).vm.$emit('update:modelValue', 'client-x')
  wrapper.findAllComponents({ name: 'Input' })[0].vm.$emit('update:modelValue', '* * * * *')
  await flushPromises()
  expect(wrapper.text()).not.toContain('admin.scheduledTests.autoRecover')
  await click('common.save')
  await flushPromises()
  expect(create).toHaveBeenCalledWith(
    expect.objectContaining({ provider_id: 1, quality_enabled: true, auto_recover: false }),
  )
  await wrapper.setProps({ show: false })
  await wrapper.setProps({ show: true, providerId: 2, qualitySupported: false })
  await click('admin.scheduledTests.addPlan')
  expect(wrapper.text()).not.toContain('admin.scheduledTests.qualityProbe')
  expect(wrapper.text()).toContain('admin.scheduledTests.autoRecover')
  wrapper.unmount()
})
