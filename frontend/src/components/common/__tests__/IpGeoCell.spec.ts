import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'

/**
 * IpGeoCell.spec.ts checks IP lookup display states and user actions with a controlled
 * cache substitute. Each case supplies its cache entry and observes the rendered
 * controls or delegated request, including forced refresh and external detail
 * navigation.
 */

/**
 * mocks supplies shared lookup spies for the module substitute; beforeEach resets
 * both spies to isolate the supplied IP cache state scenarios.
 */
const mocks = vi.hoisted(() => ({
  getEntry: vi.fn(),
  fetchOne: vi.fn(),
}))

vi.mock('@/utils/ipGeoLookup', () => ({
  getEntry: mocks.getEntry,
  fetchOne: mocks.fetchOne,
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => {
        const table: Record<string, string> = {
          'common.loading': 'Loading...',
          'usage.ipGeo.fetch': 'Fetch region',
          'usage.ipGeo.failed': 'Failed',
          'usage.ipGeo.private': 'Private address',
          'usage.ipGeo.refreshTitle': 'Refresh',
          'usage.ipGeo.detailOrg': 'ISP',
          'usage.ipGeo.detailTimezone': 'Timezone',
          'usage.ipGeo.detailAccuracy': 'Accuracy',
          'usage.ipGeo.detailCoordinates': 'Coordinates',
        }
        // Unlisted messages remain identifiable by their keys so a missing test
        // translation does not hide which component label was requested.
        return table[key] ?? key
      },
    }),
  }
})

import IpGeoCell from '../IpGeoCell.vue'

describe('IpGeoCell', () => {
  beforeEach(() => {
    mocks.getEntry.mockReset()
    mocks.fetchOne.mockReset()
  })

  /**
   * An idle IP cache entry offers a lookup action to the user. Clicking it delegates
   * the current address to fetchOne through the normal lookup path.
   */
  it('renders a clickable fetch link in idle state and triggers fetchOne on click', async () => {
    mocks.getEntry.mockReturnValue({ status: 'idle' })
    const wrapper = mount(IpGeoCell, { props: { ip: '8.8.8.8' } })
    expect(wrapper.text()).toContain('Fetch region')
    await wrapper.find('button').trigger('click')
    expect(mocks.fetchOne).toHaveBeenCalledWith('8.8.8.8')
  })

  /**
   * A loading lookup entry is displayed while its result is pending. The cell exposes
   * a named busy status and a skeleton while hiding action buttons.
   */
  it('加载属地时显示带加载标签的行内骨架', () => {
    mocks.getEntry.mockReturnValue({ status: 'loading' })
    const wrapper = mount(IpGeoCell, { props: { ip: '8.8.8.8' } })
    expect(wrapper.get('[role="status"]').attributes('aria-label')).toBe('Loading...')
    expect(wrapper.get('[role="status"]').attributes('aria-busy')).toBe('true')
    expect(wrapper.find('.skeleton').exists()).toBe(true)
    expect(wrapper.find('button').exists()).toBe(false)
  })

  /**
   * A successful lookup provides a location label and supplementary network details.
   * The tooltip exposes those details and the refresh button passes the force flag to
   * fetchOne.
   */
  it('renders success state with label, tooltip detail, and a refresh button', async () => {
    mocks.getEntry.mockReturnValue({
      status: 'success',
      label: 'CN · Guangdong · Shenzhen',
      detail: {
        organization: 'AS4134 Chinanet',
        timezone: 'Asia/Shanghai',
        accuracy: 10,
        latitude: '22.5',
        longitude: '114.0',
      },
    })
    const wrapper = mount(IpGeoCell, { props: { ip: '121.35.47.43' } })
    expect(wrapper.text()).toContain('CN · Guangdong · Shenzhen')
    const buttons = wrapper.findAll('button')
    expect(buttons.length).toBe(2)
    expect(buttons[0].attributes('title')).toContain('AS4134 Chinanet')
    expect(buttons[0].attributes('title')).toContain('Asia/Shanghai')
    await buttons[1].trigger('click')
    expect(mocks.fetchOne).toHaveBeenCalledWith('121.35.47.43', true)
  })

  /**
   * A successful location label is clicked to inspect external detail. The cell opens
   * the address lookup URL in a new context with noopener and noreferrer features.
   */
  it('opens the external lookup page when the label is clicked', async () => {
    mocks.getEntry.mockReturnValue({ status: 'success', label: 'US · California', detail: {} })
    const openSpy = vi.spyOn(window, 'open').mockImplementation(() => null)
    const wrapper = mount(IpGeoCell, { props: { ip: '8.8.4.4' } })
    await wrapper.findAll('button')[0].trigger('click')
    expect(openSpy).toHaveBeenCalledWith(
      'https://www.iplocation.net/ip-lookup?query=8.8.4.4',
      '_blank',
      'noopener,noreferrer',
    )
    openSpy.mockRestore()
  })

  /**
   * A lookup cache entry has failed and displays a retry action. Clicking the failed
   * label delegates the same address to fetchOne for another lookup attempt.
   */
  it('renders failed state as a clickable retry', async () => {
    mocks.getEntry.mockReturnValue({ status: 'error' })
    const wrapper = mount(IpGeoCell, { props: { ip: '8.8.8.8' } })
    expect(wrapper.text()).toContain('Failed')
    await wrapper.find('button').trigger('click')
    expect(mocks.fetchOne).toHaveBeenCalledWith('8.8.8.8')
  })

  /**
   * A private address has a nonpublic lookup state in the shared cache. The cell
   * displays the private address label with no clickable lookup action.
   */
  it('renders private state as non-clickable text', () => {
    mocks.getEntry.mockReturnValue({ status: 'private' })
    const wrapper = mount(IpGeoCell, { props: { ip: '192.168.1.1' } })
    expect(wrapper.text()).toContain('Private address')
    expect(wrapper.find('button').exists()).toBe(false)
  })

  /**
   * A public IPv6 address has a long location label and no supplementary details.
   * Both values remain available on hover, and the refresh and detail actions use
   * the complete IPv6 address.
   */
  it('紧凑 IPv6 有独立 IP 行、完整属地提示和原地址操作', async () => {
    const ip = '2001:4860:4860:0000:0000:0000:0000:8888'
    const label = 'US · California · A location with a long display name'
    mocks.getEntry.mockReturnValue({ status: 'success', label })
    const openSpy = vi.spyOn(window, 'open').mockImplementation(() => null)
    const wrapper = mount(IpGeoCell, { props: { ip, compact: true } })
    try {
      const ipLine = wrapper.get('div.truncate')
      expect(ipLine.text()).toBe(ip)
      expect(ipLine.attributes('title')).toBe(ip)
      expect(mocks.getEntry).toHaveBeenCalledWith(ip)
      const [location, refresh] = wrapper.findAll('button')
      expect(location.classes()).toContain('truncate')
      expect(location.attributes('title')).toBe(label)
      expect(refresh.classes()).toContain('shrink-0')
      await location.trigger('click')
      expect(openSpy).toHaveBeenCalledWith(
        'https://www.iplocation.net/ip-lookup?query=2001%3A4860%3A4860%3A0000%3A0000%3A0000%3A0000%3A8888',
        '_blank',
        'noopener,noreferrer',
      )
      await refresh.trigger('click')
      expect(mocks.fetchOne).toHaveBeenCalledWith(ip, true)
    } finally {
      // Restore window.open even if the assertions fail so other cases use their
      // own independent navigation spies.
      openSpy.mockRestore()
    }
  })

  /**
   * An IPv6 lookup in the idle or error state remains actionable below the address.
   * Both buttons delegate the full address through the ordinary lookup path.
   */
  it.each([
    ['idle', 'Fetch region'],
    ['error', 'Failed'],
  ])('紧凑属地保留查询和重试 %s', async (status, text) => {
    const ip = '2001:4860:4860::8888'
    mocks.getEntry.mockReturnValue({ status })
    const wrapper = mount(IpGeoCell, { props: { ip, compact: true } })

    expect(wrapper.get('div.truncate').text()).toBe(ip)
    expect(wrapper.get('button').text()).toBe(text)
    await wrapper.get('button').trigger('click')
    expect(mocks.fetchOne).toHaveBeenCalledWith(ip)
  })

  /**
   * A compact cell has no address supplied by the caller. It renders a placeholder
   * and avoids reading or fetching a lookup entry for the empty value.
   */
  it('紧凑属地空值只显示占位符', () => {
    const wrapper = mount(IpGeoCell, { props: { ip: '', compact: true } })

    expect(wrapper.text()).toBe('-')
    expect(wrapper.find('button').exists()).toBe(false)
    expect(mocks.getEntry).not.toHaveBeenCalled()
  })
})
