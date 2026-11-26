import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'

import { useDesktopTableViewport } from '@/composables/useDesktopTableViewport'
import { TABLE_DESKTOP_MEDIA_QUERY } from '@/constants/layout'
import DataTable from '../DataTable.vue'
import type { Column } from '../types'

// DataTable.spec.ts checks responsive rendering, sorting, selection, and virtual
// measurement ownership. Mounted tables and local browser stubs reproduce
// viewport changes and replaced datasets, while direct virtualizer observations
// verify which cached row identities remain reusable.

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, fallback?: string) => (typeof fallback === 'string' ? fallback : key),
  }),
}))

// stubDesktopMatchMedia installs a media query stub reporting the desktop
// branch.
const stubDesktopMatchMedia = () => {
  Object.defineProperty(window, 'matchMedia', {
    writable: true,
    value: vi.fn().mockImplementation((query: string) => ({
      matches: true,
      media: query,
      onchange: null,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      addListener: vi.fn(),
      removeListener: vi.fn(),
      dispatchEvent: vi.fn(),
    })),
  })
}

// stubMobileMatchMedia installs a media query stub reporting the mobile branch.
const stubMobileMatchMedia = () => {
  Object.defineProperty(window, 'matchMedia', {
    writable: true,
    value: vi.fn().mockImplementation((query: string) => ({
      matches: false,
      media: query,
      onchange: null,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      addListener: vi.fn(),
      removeListener: vi.fn(),
      dispatchEvent: vi.fn(),
    })),
  })
}

// These columns pair fixed controls with unequal data widths and provide both
// header layouts for server-side and client-side sorting tests.
const compactColumns: Column[] = [
  { key: 'select', label: '', width: '44px' },
  {
    key: 'identity',
    label: 'Identity',
    width: '200px',
    sortOptions: [
      { key: 'name', label: 'Name' },
      { key: 'id', label: 'ID' },
    ],
    subtitleSortLayout: 'inline',
  },
  {
    key: 'groups',
    label: 'Groups',
    width: '100px',
    sortOptions: [
      { key: 'priority', label: 'Priority' },
      { key: 'rate_multiplier', label: 'Rate' },
    ],
    subtitleSortLayout: 'subline',
  },
  { key: 'actions', label: 'Actions', width: '106px' },
]

describe('DataTable', () => {
  beforeEach(() => {
    stubDesktopMatchMedia()
    localStorage.clear()
  })

  // The table mounts under the desktop media stub. It must request the shared
  // 1024-pixel media query so responsive rendering follows the application
  // breakpoint.
  it('uses the shared 1024px desktop breakpoint', () => {
    const wrapper = mount(DataTable, {
      props: {
        columns: [{ key: 'name', label: 'Name' }],
        data: [{ id: 1, name: 'Key' }],
      },
    })

    expect(window.matchMedia).toHaveBeenCalledWith('(min-width: 1024px)')
    wrapper.unmount()
  })

  // A responsive cell and its table share one browser breakpoint subscription.
  // Crossing into cards and back must restore the desktop width and drag handles.
  it('switches with responsive cells and measures the remounted desktop wrapper', async () => {
    let desktop = true
    let clientWidth = 800
    const listeners = new Set<(event: MediaQueryListEvent) => void>()
    const matchMedia = vi.mocked(window.matchMedia)
    const previousMatchMedia = matchMedia.getMockImplementation()!
    matchMedia.mockImplementation((query) => {
      const media = previousMatchMedia(query)
      if (query !== TABLE_DESKTOP_MEDIA_QUERY) return media
      return {
        ...media,
        matches: desktop,
        addEventListener: (_type, listener) =>
          listeners.add(listener as (event: MediaQueryListEvent) => void),
        removeEventListener: (_type, listener) =>
          listeners.delete(listener as (event: MediaQueryListEvent) => void),
      }
    })
    const widthSpy = vi.spyOn(HTMLElement.prototype, 'clientWidth', 'get')
    widthSpy.mockImplementation(() => clientWidth)

    // ResponsiveCell uses the shared breakpoint just like a provider usage cell.
    const ResponsiveCell = defineComponent({
      setup() {
        const isDesktop = useDesktopTableViewport()
        return () => h('span', isDesktop.value ? 'Desktop cell' : 'Card cell')
      },
    })
    const wrapper = mount(DataTable, {
      props: {
        columns: compactColumns,
        columnOrderStorageKey: 'compact-order',
        density: 'compact',
        data: [{ id: 1, name: 'Key' }],
      },
      slots: { 'cell-identity': () => h(ResponsiveCell) },
    })

    // changeViewport delivers the browser breakpoint event to every subscriber.
    const changeViewport = async (matches: boolean, width: number) => {
      desktop = matches
      clientWidth = width
      for (const listener of [...listeners]) {
        listener({ matches, media: TABLE_DESKTOP_MEDIA_QUERY } as MediaQueryListEvent)
      }
      await flushPromises()
    }

    try {
      await flushPromises()
      expect(listeners.size).toBe(1)
      expect(wrapper.get('table').element.style.width).toBe('800px')
      expect(wrapper.findAll('.column-drag-handle')).toHaveLength(2)
      expect(wrapper.text()).toContain('Desktop cell')

      await changeViewport(false, 320)
      expect(wrapper.find('table').exists()).toBe(false)
      expect(wrapper.find('.column-drag-handle').exists()).toBe(false)
      expect(wrapper.text()).toContain('Card cell')

      await changeViewport(true, 350)
      expect(listeners.size).toBe(1)
      expect(wrapper.get('table').element.style.width).toBe('450px')
      expect(wrapper.findAll('.column-drag-handle')).toHaveLength(2)
      expect(wrapper.text()).toContain('Desktop cell')
    } finally {
      wrapper.unmount()
      widthSpy.mockRestore()
    }
  })

  // A pending mobile table shares field headings with its loaded cards. Values
  // and interactive selection only appear once the parent supplies the result.
  it('keeps the mobile card labels visible while row values load', async () => {
    stubMobileMatchMedia()
    const wrapper = mount(DataTable, {
      props: {
        columns: [
          { key: 'name', label: 'Name' },
          { key: 'status', label: 'Status' },
        ],
        data: [],
        loading: true,
        selectable: true,
      },
    })

    const placeholder = wrapper.get('[data-loading-skeleton]')
    expect(placeholder.text()).toBe('NameStatus')
    expect(wrapper.find('input[type="checkbox"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('empty.noData')

    await wrapper.setProps({ data: [{ id: 1, name: 'Example', status: 'Ready' }], loading: false })

    expect(wrapper.find('[data-loading-skeleton]').exists()).toBe(false)
    expect(wrapper.get('[data-field="name"]').text()).toBe('NameExample')
    expect(wrapper.get('[data-test="select-row"]').exists()).toBe(true)
    wrapper.unmount()
  })

  // A sortable column starts ascending and is clicked once. Both direction
  // arrows must remain visible while active color and accessible sort direction
  // switch to descending.
  it('renders paired sort arrows and highlights the active direction', async () => {
    const wrapper = mount(DataTable, {
      props: {
        columns: [
          { key: 'name', label: 'Name', sortable: true },
          { key: 'created_at', label: 'Created', sortable: true },
        ],
        data: [
          { id: 1, name: 'Beta', created_at: '2026-01-02T00:00:00Z' },
          { id: 2, name: 'Alpha', created_at: '2026-01-01T00:00:00Z' },
        ],
        defaultSortKey: 'name',
        defaultSortOrder: 'asc',
      },
      slots: {
        'header-name': '<span data-test="custom-name-header">Name</span>',
      },
    })

    await wrapper.vm.$nextTick()

    const nameHeader = wrapper.findAll('th')[0]
    expect(nameHeader.find('[data-test="custom-name-header"]').exists()).toBe(true)
    expect(nameHeader.attributes('aria-sort')).toBe('ascending')
    expect(nameHeader.findAll('svg')).toHaveLength(2)
    expect(nameHeader.findAll('svg')[0].classes()).toContain('text-primary-600')
    expect(nameHeader.findAll('svg')[1].classes()).toContain('text-gray-300')

    await nameHeader.trigger('click')
    await wrapper.vm.$nextTick()

    expect(nameHeader.attributes('aria-sort')).toBe('descending')
    expect(nameHeader.findAll('svg')[0].classes()).toContain('text-gray-300')
    expect(nameHeader.findAll('svg')[1].classes()).toContain('text-primary-600')
  })

  // Compact widths use the scrollport rather than the viewport. Changing its
  // width or the visible columns redistributes only surplus data-cell space.
  it('sizes compact columns from the container and preserves fixed widths', async () => {
    vi.stubGlobal('ResizeObserver', undefined)
    const wrapper = mount(DataTable, {
      props: { density: 'compact', columns: compactColumns, data: [{ id: 1, name: 'Key' }] },
    })

    try {
      await flushPromises()
      const scrollport = wrapper.get('.table-wrapper').element
      const table = wrapper.get('table')
      // readColumnWidths returns the allocated track sizes in rendered column order.
      const readColumnWidths = () =>
        wrapper.findAll('col').map(({ element }) => element.style.width)
      // resizeScrollport simulates the width left after reserving the scrollbar.
      const resizeScrollport = async (clientWidth: number, scrollWidth: number) => {
        Object.defineProperties(scrollport, {
          clientWidth: { configurable: true, value: clientWidth },
          scrollWidth: { configurable: true, value: scrollWidth },
        })
        window.dispatchEvent(new Event('resize'))
        await wrapper.vm.$nextTick()
      }

      expect(table.classes()).toContain('table-fixed')
      expect(table.classes()).not.toContain('min-w-max')
      expect(wrapper.get('td[data-column-key="select"]').classes()).toContain(
        'table-selection-cell',
      )
      expect(wrapper.get('td[data-column-key="identity"]').classes()).toEqual(
        expect.arrayContaining(['px-2', 'py-2', 'whitespace-normal']),
      )
      await resizeScrollport(901, 901)
      expect(table.element.style.width).toBe('901px')
      expect(readColumnWidths()).toEqual(['44px', '501px', '250px', '106px'])
      expect(wrapper.classes()).not.toContain('is-scrollable')

      await resizeScrollport(400, 450)
      expect(table.element.style.width).toBe('450px')
      expect(readColumnWidths()).toEqual(['44px', '200px', '100px', '106px'])
      expect(wrapper.classes()).toContain('is-scrollable')

      await wrapper.setProps({ columns: compactColumns.filter(({ key }) => key !== 'groups') })
      await resizeScrollport(400, 400)
      expect(table.element.style.width).toBe('400px')
      expect(readColumnWidths()).toEqual(['44px', '250px', '106px'])
      expect(wrapper.classes()).not.toContain('is-scrollable')
    } finally {
      wrapper.unmount()
      vi.unstubAllGlobals()
    }
  })

  // An existing standard table supplies pixel widths. Its header hints and
  // conventional padding remain available without introducing fixed tracks.
  it('keeps standard sizing when density is omitted', () => {
    const wrapper = mount(DataTable, {
      props: {
        columns: [{ key: 'name', label: 'Name', width: '200px' }],
        data: [{ id: 1, name: 'Key' }],
      },
    })
    expect(wrapper.find('colgroup').exists()).toBe(false)
    expect(wrapper.get('table').classes()).toContain('min-w-max')
    expect(wrapper.get('th').element.style.width).toBe('200px')
    expect(wrapper.get('td').classes()).toEqual(expect.arrayContaining(['px-6', 'py-3']))
    wrapper.unmount()
  })

  // A composite header combines native sort controls, a separate group title,
  // and independent help. Activating either sort label changes only its own key.
  it('sorts compact sub-fields independently and announces only the active direction', async () => {
    const wrapper = mount(DataTable, {
      props: {
        density: 'compact',
        columns: compactColumns,
        data: [{ id: 1, name: 'Key' }],
        defaultSortKey: 'name',
        serverSideSort: true,
      },
      slots: { 'header-groups': '<span>Groups</span><button type="button">Help</button>' },
    })
    await wrapper.vm.$nextTick()
    const identity = wrapper.get('th[data-column-key="identity"]')
    const groups = wrapper.get('th[data-column-key="groups"]')
    const [name, id] = identity.findAll('button')
    const [help, priority, rate] = groups.findAll('button')
    expect(identity.text()).not.toContain('Identity')
    expect(groups.text()).toMatch(/Groups.*Priority/)
    expect(identity.attributes('aria-sort')).toBe('ascending')
    expect(groups.attributes('aria-sort')).toBeUndefined()
    expect(name.attributes('aria-label')).toBe('Name (Ascending)')
    expect(name.findAll('svg')).toHaveLength(1)
    expect(id.find('svg').exists()).toBe(false)
    expect(rate.find('svg').exists()).toBe(false)

    await groups.trigger('click')
    await groups.get('span').trigger('click')
    await help.trigger('click')
    expect(wrapper.emitted('sort')).toBeUndefined()

    expect(priority.attributes('type')).toBe('button')
    await id.trigger('click')
    await priority.trigger('click')
    await priority.get('svg').trigger('click')
    expect(priority.attributes('aria-label')).toBe('Priority (Descending)')
    expect(groups.attributes('aria-sort')).toBe('descending')
    expect(identity.attributes('aria-sort')).toBeUndefined()
    expect(identity.find('svg').exists()).toBe(false)
    expect(wrapper.emitted('sort')).toEqual([
      ['id', 'asc'],
      ['priority', 'asc'],
      ['priority', 'desc'],
    ])
    wrapper.unmount()
  })

  // Storage contains a sub-option rather than its visual column key. When the
  // key vanishes, the table overwrites the preference and emits one fallback.
  it('restores composite sorting and falls back once when the active option is removed', async () => {
    const props = {
      density: 'compact' as const,
      columns: compactColumns,
      data: [{ id: 1, name: 'Key' }],
      sortStorageKey: 'compact-sort',
      defaultSortKey: 'name',
      defaultSortOrder: 'desc' as const,
      serverSideSort: true,
    }
    const first = mount(DataTable, { props })
    const rateButton = first.get('button[aria-label="Rate"]')
    await rateButton.trigger('click')
    await rateButton.trigger('click')
    first.unmount()

    const wrapper = mount(DataTable, { props })
    await wrapper.vm.$nextTick()
    expect(wrapper.find('button[aria-label="Rate (Descending)"]').exists()).toBe(true)
    expect(wrapper.emitted('sort')).toBeUndefined()

    await wrapper.setProps({ columns: compactColumns.filter(({ key }) => key !== 'actions') })
    expect(wrapper.emitted('sort')).toBeUndefined()
    const withoutRate = compactColumns.map((column) =>
      column.key === 'groups'
        ? { ...column, sortOptions: column.sortOptions?.slice(0, 1) }
        : column,
    )
    await wrapper.setProps({ columns: withoutRate })
    expect(wrapper.emitted('sort')).toEqual([['name', 'desc']])
    expect(wrapper.get('th[data-column-key="identity"]').attributes('aria-sort')).toBe('descending')
    expect(localStorage.getItem('compact-sort')).toBe(
      JSON.stringify({ key: 'name', order: 'desc' }),
    )

    await wrapper.setProps({ columns: compactColumns })
    expect(wrapper.find('button[aria-label="Rate"]').exists()).toBe(true)
    expect(wrapper.find('button[aria-label="Name (Descending)"]').exists()).toBe(true)
    expect(wrapper.emitted('sort')).toEqual([['name', 'desc']])
    await wrapper.get('button[aria-label="Priority"]').trigger('click')
    await wrapper.setProps({ columns: compactColumns.filter(({ key }) => key !== 'groups') })
    expect(wrapper.emitted('sort')).toEqual([
      ['name', 'desc'],
      ['priority', 'asc'],
      ['name', 'desc'],
    ])
    wrapper.unmount()
  })

  // A hidden persisted option can be stale on the first mount. The header
  // repairs storage silently because initial server loading belongs to the parent.
  it('repairs an unavailable restored key without emitting an initial sort', async () => {
    localStorage.setItem('compact-sort', JSON.stringify({ key: 'priority', order: 'asc' }))
    const wrapper = mount(DataTable, {
      props: {
        density: 'compact',
        columns: compactColumns.filter(({ key }) => key !== 'groups'),
        data: [],
        sortStorageKey: 'compact-sort',
        defaultSortKey: 'name',
        serverSideSort: true,
      },
    })
    await wrapper.vm.$nextTick()
    expect(wrapper.find('button[aria-label="Name (Ascending)"]').exists()).toBe(true)
    expect(localStorage.getItem('compact-sort')).toBe(JSON.stringify({ key: 'name', order: 'asc' }))
    expect(wrapper.emitted('sort')).toBeUndefined()
    wrapper.unmount()
  })

  // Client sorting uses the option's data key, which differs from the visual
  // column key. Removing that column clears the saved choice without a request.
  it('sorts client rows by the child key and clears storage without an available fallback', async () => {
    const wrapper = mount(DataTable, {
      props: {
        density: 'compact',
        columns: compactColumns,
        data: [
          { id: 1, identity: 'Entry one', name: 'Zeta' },
          { id: 2, identity: 'Entry two', name: 'Alpha' },
        ],
        rowKey: 'id',
        sortStorageKey: 'compact-sort',
      },
    })
    await wrapper.get('button[aria-label="Name"]').trigger('click')
    expect(wrapper.findAll('tbody tr').map((row) => row.attributes('data-row-id'))).toEqual([
      '2',
      '1',
    ])

    await wrapper.setProps({ columns: compactColumns.filter(({ key }) => key !== 'identity') })
    expect(localStorage.getItem('compact-sort')).toBeNull()
    expect(wrapper.find('[aria-sort]').exists()).toBe(false)
    expect(wrapper.emitted('sort')).toBeUndefined()
    wrapper.unmount()
  })

  // A compact card and its desktop counterpart receive the same row and value
  // in each slot, including custom selection and the separately rendered actions.
  it('passes the same cell slots and payloads to compact mobile cards', () => {
    const options = {
      props: {
        density: 'compact' as const,
        columns: compactColumns,
        data: [{ id: 7, identity: 'Provider A', actions: 'edit' }],
      },
      slots: {
        'cell-select': '<span data-test="custom-select">{{ params.row.id }}</span>',
        'cell-identity': '<span data-test="custom-identity">{{ params.value }}</span>',
        'cell-actions':
          '<span data-test="custom-actions">{{ params.row.id }} {{ params.value }} {{ params.expanded }}</span>',
      },
    }
    const desktop = mount(DataTable, options)
    stubMobileMatchMedia()
    const mobile = mount(DataTable, options)
    expect(mobile.find('table').exists()).toBe(false)
    expect(mobile.find('[data-field="identity"]').exists()).toBe(true)
    for (const [slot, value] of [
      ['custom-select', '7'],
      ['custom-identity', 'Provider A'],
      ['custom-actions', '7 edit false'],
    ]) {
      const selector = `[data-test="${slot}"]`
      expect(mobile.get(selector).text()).toBe(value)
      expect(desktop.get(selector).text()).toBe(value)
    }
    desktop.unmount()
    mobile.unmount()
  })

  // A small dataset remains below the virtualization threshold. Every record
  // must render in the document and no virtual padding spacer may replace
  // ordinary rows.
  it('renders every row with no virtual padding spacer for small datasets (virtualization off)', async () => {
    const data = Array.from({ length: 8 }, (_, i) => ({ id: i + 1, name: `Row ${i + 1}` }))
    const wrapper = mount(DataTable, {
      props: {
        columns: [{ key: 'name', label: 'Name' }],
        data,
      },
    })

    await wrapper.vm.$nextTick()

    // Small lists use ordinary row rendering.
    expect((wrapper.vm as any).shouldVirtualize).toBe(false)
    // Every data record has a rendered row.
    expect(wrapper.findAll('tbody tr[data-index]')).toHaveLength(data.length)
    // Ordinary rows need no virtual spacer.
    expect(wrapper.findAll('tbody tr[aria-hidden="true"]')).toHaveLength(0)
  })

  // Asynchronous contents widen the table while its wrapper width remains fixed.
  // Observing the inner table must update overflow styling after the measurement
  // callback.
  it('rechecks horizontal overflow when asynchronous content changes the table width', async () => {
    const observers: Array<{
      callback: ResizeObserverCallback // Captured resize observation callback.
      elements: Element[] // Targets registered for observation.
    }> = []
    // TrackingResizeObserver records observed elements and exposes callbacks to
    // reproduce table content resizing.
    class TrackingResizeObserver {
      callback: ResizeObserverCallback // Captured resize observation callback.
      elements: Element[] = [] // Targets registered for observation.

      // constructor records the resize callback and registers this observation fixture.
      constructor(callback: ResizeObserverCallback) {
        this.callback = callback
        observers.push(this)
      }

      observe = vi.fn((element: Element) => this.elements.push(element)) // Observed-target registration spy.
      disconnect = vi.fn() // Observer teardown call spy.
      unobserve = vi.fn() // Target removal call spy.
    }
    const originalResizeObserver = globalThis.ResizeObserver
    globalThis.ResizeObserver = TrackingResizeObserver as unknown as typeof ResizeObserver

    try {
      const wrapper = mount(DataTable, {
        props: {
          columns: [
            { key: 'name', label: 'Name' },
            { key: 'actions', label: 'Actions' },
          ],
          data: [{ id: 1, name: 'Key' }],
        },
      })
      await wrapper.vm.$nextTick()

      const tableWrapper = wrapper.get('.table-wrapper').element
      const table = wrapper.get('table').element
      const tableObserver = observers.find(
        (observer) => observer.elements.includes(tableWrapper) && observer.elements.includes(table),
      )
      expect(tableObserver).toBeDefined()

      // Keep wrapper width fixed while increasing content width so the callback
      // reproduces asynchronous cells changing overflow independently of outer
      // layout.
      Object.defineProperty(tableWrapper, 'clientWidth', { configurable: true, value: 800 })
      Object.defineProperty(tableWrapper, 'scrollWidth', { configurable: true, value: 1200 })
      tableObserver?.callback([], tableObserver as unknown as ResizeObserver)
      await wrapper.vm.$nextTick()

      expect(wrapper.get('.table-wrapper').classes()).toContain('is-scrollable')
      wrapper.unmount()
    } finally {
      globalThis.ResizeObserver = originalResizeObserver
    }
  })

  // A table mounts with the default sticky configuration. Its wrapper must use
  // the thin separator class and omit the separate shadow-style separator class.
  it('uses a line boundary for sticky columns by default', () => {
    const wrapper = mount(DataTable, {
      props: {
        columns: [{ key: 'name', label: 'Name' }],
        data: [{ id: 1, name: 'Key' }],
      },
    })

    expect(wrapper.get('.table-wrapper').classes()).toContain('sticky-boundary-line')
    expect(wrapper.get('.table-wrapper').classes()).not.toContain('sticky-boundary-shadow')
  })

  // The record count exceeds an explicitly small virtualization threshold.
  // Windowed rendering must activate and the virtualizer must receive the
  // complete current dataset count.
  it('switches to windowed rendering once row count exceeds virtualizeThreshold', async () => {
    const data = Array.from({ length: 12 }, (_, i) => ({ id: i + 1, name: `Row ${i + 1}` }))
    const wrapper = mount(DataTable, {
      props: {
        columns: [{ key: 'name', label: 'Name' }],
        data,
        virtualizeThreshold: 3,
      },
    })

    await wrapper.vm.$nextTick()

    // The configured threshold enables windowed rendering.
    expect((wrapper.vm as any).shouldVirtualize).toBe(true)
    // The virtualizer receives the complete row count.
    const exposed = (wrapper.vm as any).virtualizer
    const instance = exposed?.value ?? exposed
    expect(instance.options.count).toBe(data.length)
  })

  // Record IDs differ from their array positions in a virtualized dataset. The
  // virtualizer key callback must use those IDs so measurements follow record
  // identity.
  it('keys the virtualizer size cache by row identity, not index (avoids stale heights on sort/filter)', async () => {
    const data = Array.from({ length: 12 }, (_, i) => ({ id: 100 + i, name: `Row ${i + 1}` }))
    const wrapper = mount(DataTable, {
      props: {
        columns: [{ key: 'name', label: 'Name' }],
        data,
        rowKey: 'id',
        virtualizeThreshold: 3,
      },
    })

    await wrapper.vm.$nextTick()

    const exposed = (wrapper.vm as any).virtualizer
    const instance = exposed?.value ?? exposed
    // Measurement keys use stable record identifiers.
    expect(instance.options.getItemKey(0)).toBe(100)
    expect(instance.options.getItemKey(5)).toBe(105)
  })

  // Pagination replaces every row ID with a different page of equal length. Old
  // element and height entries must be removed from the virtualizer caches.
  it('clears stale row and element caches when pagination replaces the row ID set', async () => {
    const firstPage = Array.from({ length: 100 }, (_, i) => ({ id: i + 1, name: `First ${i + 1}` }))
    const secondPage = Array.from({ length: 100 }, (_, i) => ({
      id: i + 101,
      name: `Second ${i + 1}`,
    }))
    const wrapper = mount(DataTable, {
      props: {
        columns: [{ key: 'name', label: 'Name' }],
        data: firstPage,
        rowKey: 'id',
        virtualizeThreshold: 1,
      },
    })

    await wrapper.vm.$nextTick()

    const exposed = (wrapper.vm as any).virtualizer
    const instance = exposed?.value ?? exposed
    const firstPageIDs = firstPage.map((row) => row.id)
    ;(instance as any).itemSizeCache = new Map(firstPageIDs.map((id) => [id, 156]))
    instance.elementsCache.clear()
    for (const id of firstPageIDs) {
      instance.elementsCache.set(id, document.createElement('tr'))
    }
    const measureElementSpy = vi.spyOn(instance, 'measureElement')

    await wrapper.setProps({ data: secondPage })
    await wrapper.vm.$nextTick()

    const sizeCache = (instance as any).itemSizeCache as Map<number, number>
    expect(sizeCache.size).toBeLessThanOrEqual(secondPage.length)
    expect(instance.elementsCache.size).toBeLessThanOrEqual(secondPage.length)
    expect(firstPageIDs.some((id) => sizeCache.has(id))).toBe(false)
    expect(firstPageIDs.some((id) => instance.elementsCache.has(id))).toBe(false)
    expect(measureElementSpy.mock.calls.some(([node]) => node === null)).toBe(true)
  })

  // Equal-length pages replace object records that have no explicit stable keys.
  // The table must clear retained elements because the new object identities
  // represent different rows.
  it('clears stale caches when equal-length pages replace rows without stable keys', async () => {
    const firstPage = Array.from({ length: 12 }, (_, i) => ({ name: `First ${i + 1}` }))
    const secondPage = Array.from({ length: 12 }, (_, i) => ({ name: `Second ${i + 1}` }))
    const wrapper = mount(DataTable, {
      props: {
        columns: [{ key: 'name', label: 'Name' }],
        data: firstPage,
        virtualizeThreshold: 1,
      },
    })

    await wrapper.vm.$nextTick()

    const exposed = (wrapper.vm as any).virtualizer
    const instance = exposed?.value ?? exposed
    const measureElementSpy = vi.spyOn(instance, 'measureElement')

    await wrapper.setProps({ data: secondPage })
    await wrapper.vm.$nextTick()

    expect(measureElementSpy.mock.calls.some(([node]) => node === null)).toBe(true)
  })

  // Two pages have the same deduplicated keys but different duplicate
  // multiplicities. The table must invalidate measurements because each cached
  // key cannot identify one distinct row.
  it('conservatively clears caches when duplicate row-key multiplicity changes', async () => {
    const firstPage = [
      { id: 1, name: 'First A' },
      { id: 1, name: 'First B' },
      { id: 2, name: 'First C' },
    ]
    const secondPage = [
      { id: 1, name: 'Second A' },
      { id: 2, name: 'Second B' },
      { id: 2, name: 'Second C' },
    ]
    const wrapper = mount(DataTable, {
      props: {
        columns: [{ key: 'name', label: 'Name' }],
        data: firstPage,
        rowKey: 'id',
        virtualizeThreshold: 1,
      },
    })

    await wrapper.vm.$nextTick()

    const exposed = (wrapper.vm as any).virtualizer
    const instance = exposed?.value ?? exposed
    const measureElementSpy = vi.spyOn(instance, 'measureElement')

    await wrapper.setProps({ data: secondPage })
    await wrapper.vm.$nextTick()

    expect(measureElementSpy.mock.calls.some(([node]) => node === null)).toBe(true)
  })

  // The same object records are reversed without explicit row keys. The
  // identity-set comparison must skip the virtualizer's full measurement reset
  // when those object references remain present.
  it('preserves cache when rows without stable keys only reorder the same objects', async () => {
    const data = Array.from({ length: 12 }, (_, i) => ({ name: `Row ${i + 1}` }))
    const wrapper = mount(DataTable, {
      props: {
        columns: [{ key: 'name', label: 'Name' }],
        data,
        virtualizeThreshold: 1,
      },
    })

    await wrapper.vm.$nextTick()

    const exposed = (wrapper.vm as any).virtualizer
    const instance = exposed?.value ?? exposed
    const measureSpy = vi.spyOn(instance, 'measure')

    await wrapper.setProps({ data: [...data].reverse() })
    await wrapper.vm.$nextTick()

    expect(measureSpy).not.toHaveBeenCalled()
  })

  // A dataset with stable IDs is reordered while measured heights exist.
  // Measurement cache entries must survive the reordering and the virtualizer
  // must avoid a full reset.
  it('preserves stable row height cache when the same row IDs are only reordered', async () => {
    const data = Array.from({ length: 100 }, (_, i) => ({ id: i + 1, name: `Row ${i + 1}` }))
    const wrapper = mount(DataTable, {
      props: {
        columns: [{ key: 'name', label: 'Name' }],
        data,
        rowKey: 'id',
        virtualizeThreshold: 1,
      },
    })

    await wrapper.vm.$nextTick()

    const exposed = (wrapper.vm as any).virtualizer
    const instance = exposed?.value ?? exposed
    ;(instance as any).itemSizeCache = new Map(data.map((row) => [row.id, 156]))
    const measureSpy = vi.spyOn(instance, 'measure')

    await wrapper.setProps({ data: [...data].reverse() })
    await wrapper.vm.$nextTick()

    const sizeCache = (instance as any).itemSizeCache as Map<number, number>
    expect(measureSpy).not.toHaveBeenCalled()
    expect(sizeCache.size).toBe(100)
  })

  // Current-page selection begins with a key belonging to another page.
  // Selecting all and then clearing one row must retain the off-page key in both
  // emitted events.
  it('emits controlled current-page selection while preserving off-page keys', async () => {
    const wrapper = mount(DataTable, {
      props: {
        columns: [{ key: 'name', label: 'Name' }],
        data: [
          { id: 1, name: 'One' },
          { id: 2, name: 'Two' },
        ],
        rowKey: 'id',
        selectable: true,
        selectedKeys: [99],
      },
    })

    await wrapper.get('[data-test="select-all"]').setValue(true)

    const selectedAll = wrapper.emitted('update:selectedKeys')?.at(-1)?.[0]
    expect(selectedAll).toEqual([99, 1, 2])

    await wrapper.setProps({ selectedKeys: selectedAll as number[] })
    const rowCheckboxes = wrapper.findAll<HTMLInputElement>('[data-test="select-row"]')
    expect(rowCheckboxes.every((checkbox) => checkbox.element.checked)).toBe(true)

    await rowCheckboxes[0].setValue(false)

    expect(wrapper.emitted('update:selectedKeys')?.at(-1)?.[0]).toEqual([99, 2])
    expect(wrapper.emitted('selectionChange')?.at(-1)?.[0]).toEqual([99, 2])
  })

  // A mobile card is mounted in a 320-pixel host with a custom usage slot. The
  // field and its content must retain shrinkable width classes and render once.
  it('keeps the single usage field shrinkable in a 320px mobile card', () => {
    stubMobileMatchMedia()
    const viewport = document.createElement('div')
    viewport.style.width = '320px'
    document.body.appendChild(viewport)
    const wrapper = mount(DataTable, {
      attachTo: viewport,
      props: {
        columns: [{ key: 'usage', label: 'Usage' }],
        data: [{ id: 1, usage: 'snapshot' }],
        rowKey: 'id',
      },
      slots: {
        'cell-usage': '<div data-test="usage-cell">snapshot</div>',
      },
    })

    expect(viewport.style.width).toBe('320px')
    expect(wrapper.findAll('[data-field="usage"]')).toHaveLength(1)
    expect(wrapper.find('[data-field="ollama_cloud_usage"]').exists()).toBe(false)
    const field = wrapper.get('[data-field="usage"]')
    expect(field.classes()).toContain('min-w-0')
    expect(field.get('div').classes()).toEqual(expect.arrayContaining(['min-w-0', 'max-w-full']))
    expect(wrapper.findAll('[data-test="usage-cell"]')).toHaveLength(1)

    wrapper.unmount()
    viewport.remove()
  })

  // Mobile selection starts with a key outside the current page. The select-all
  // card control must append visible record keys while retaining that existing
  // selection.
  it('offers current-page select all in the mobile card layout', async () => {
    stubMobileMatchMedia()
    const wrapper = mount(DataTable, {
      props: {
        columns: [{ key: 'name', label: 'Name' }],
        data: [
          { id: 1, name: 'One' },
          { id: 2, name: 'Two' },
        ],
        rowKey: 'id',
        selectable: true,
        selectedKeys: [99],
      },
    })

    await wrapper.get('[data-test="select-all-mobile"]').setValue(true)

    expect(wrapper.emitted('update:selectedKeys')?.at(-1)?.[0]).toEqual([99, 1, 2])
  })
})
