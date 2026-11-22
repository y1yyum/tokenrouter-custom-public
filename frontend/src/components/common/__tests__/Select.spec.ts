import { enableAutoUnmount, mount } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { nextTick } from 'vue'

import Select from '../Select.vue'

// Select.spec.ts covers placement, keyboard navigation, and search in the
// teleported menu. Attached controls receive keys through the focused element,
// while a mocked trigger rectangle exercises placement in a narrow viewport.

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

// originalInnerWidth retains the test environment viewport width so teardown restores
// it after the constrained-menu case.
const originalInnerWidth = window.innerWidth

enableAutoUnmount(afterEach)

// Release the resources retained by this case before the next one, keeping browser
// state and component lifecycle observations isolated across assertions.
afterEach(() => {
  document.body.innerHTML = ''
  Object.defineProperty(window, 'innerWidth', {
    configurable: true,
    value: originalInnerWidth,
  })
  vi.restoreAllMocks()
})

describe('Select dropdown viewport constraints', () => {
  // A narrow viewport contains a trigger near its right edge and a long option label.
  // The teleported panel must clamp its horizontal offset and retain the expected
  // minimum and maximum widths.
  it('repositions the teleported dropdown within the viewport', async () => {
    Object.defineProperty(window, 'innerWidth', {
      configurable: true,
      value: 320,
    })

    vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockReturnValue({
      x: 220,
      y: 20,
      top: 20,
      right: 300,
      bottom: 60,
      left: 220,
      width: 80,
      height: 40,
      toJSON: () => ({}),
    })

    const wrapper = mount(Select, {
      props: {
        modelValue: null,
        options: [
          {
            value: 'example',
            label: 'very-long-unbroken-option-value-that-must-not-overflow',
          },
        ],
      },
    })

    await wrapper.get('button').trigger('click')
    await nextTick()
    await nextTick()

    const dropdown = document.body.querySelector<HTMLElement>('.select-dropdown-portal')

    expect(dropdown).not.toBeNull()
    expect(dropdown?.style.left).toBe('104px')
    expect(dropdown?.style.minWidth).toBe('80px')
    expect(dropdown?.style.maxWidth).toBe('288px')
  })
})

// An exact search creates a manual action beside the existing catalog option.
// Distinct render keys keep both choices mounted without Vue duplicate-key warnings.
it('keeps creation and existing options distinct for an exact query', async () => {
  const warnings = vi.spyOn(console, 'warn').mockImplementation(() => {})
  const wrapper = mount(Select, {
    props: {
      modelValue: '',
      searchable: true,
      creatable: true,
      options: [{ value: 'target', label: 'target' }],
    },
  })
  await wrapper.get('button').trigger('click')
  await nextTick()
  const search = document.body.querySelector<HTMLInputElement>('.select-search-input')!
  search.value = 'target'
  search.dispatchEvent(new Event('input', { bubbles: true }))
  await nextTick()
  const options = [...document.body.querySelectorAll<HTMLElement>('[role="option"]')]
  expect(options).toHaveLength(2)
  expect(warnings.mock.calls.flat().join(' ')).not.toContain('Duplicate keys')
  options[1]!.click()
  await nextTick()
  expect(wrapper.emitted('update:modelValue')?.at(-1)).toEqual(['target'])
})

// mountKeyboardSelect attaches the trigger so focus can move between it and the
// teleported search input. The disabled middle option makes an accidental
// selection of that row observable through the emitted value.
function mountKeyboardSelect(searchable = false) {
  return mount(Select, {
    attachTo: document.body,
    props: {
      modelValue: 'first',
      searchable,
      options: [
        { value: 'first', label: 'First' },
        { value: 'disabled', label: 'Unavailable', disabled: true },
        { value: 'next', label: 'Next' },
      ],
    },
  })
}

// pressKey dispatches to the focused control and returns the cancelable event
// after Vue renders. jsdom needs an explicit click for the default action of
// an unhandled Enter on a button.
async function pressKey(key: string) {
  const target = document.activeElement!
  const event = new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true })
  if (target.dispatchEvent(event) && key === 'Enter' && target instanceof HTMLButtonElement) {
    target.click()
  }
  await nextTick()
  return event
}

describe('Select keyboard interaction', () => {
  // A short list leaves focus on its trigger after opening. Moving past a
  // disabled option and confirming must publish the next enabled value once.
  it('selects the next enabled option with Enter, ArrowDown, and Enter on the trigger', async () => {
    const wrapper = mountKeyboardSelect()
    const trigger = wrapper.get('button').element
    trigger.focus()
    await pressKey('Enter')
    expect(document.body.querySelector('[role="listbox"]')).not.toBeNull()
    expect(document.activeElement).toBe(trigger)

    await pressKey('ArrowDown')
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    await pressKey('Enter')
    expect(wrapper.emitted('update:modelValue')).toEqual([['next']])
    expect(trigger.getAttribute('aria-expanded')).toBe('false')
    expect(document.activeElement).toBe(trigger)
  })

  // Dismissing the menu discards the pending keyboard highlight. Tab must leave
  // its default traversal available and Escape must return focus to the trigger.
  it.each(['Tab', 'Escape'])('dismisses the pending selection with %s', async (key) => {
    const wrapper = mountKeyboardSelect()
    const trigger = wrapper.get('button').element
    trigger.focus()
    await pressKey('Enter')
    await pressKey('ArrowDown')
    expect(trigger.getAttribute('aria-expanded')).toBe('true')

    const event = await pressKey(key)
    expect(trigger.getAttribute('aria-expanded')).toBe('false')
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    if (key === 'Tab') expect(event.defaultPrevented).toBe(false)
    else expect(document.activeElement).toBe(trigger)
  })

  // Opening a searchable list transfers focus to its input. Enter on the
  // filtered result must publish that value and restore focus to the trigger.
  it('confirms a filtered option from the focused search input', async () => {
    const wrapper = mountKeyboardSelect(true)
    const trigger = wrapper.get('button').element
    trigger.focus()
    await pressKey('Enter')
    const search = document.body.querySelector<HTMLInputElement>('[role="listbox"] input')!
    expect(document.activeElement).toBe(search)
    search.value = 'Next'
    search.dispatchEvent(new Event('input', { bubbles: true }))
    await nextTick()

    await pressKey('Enter')
    expect(wrapper.emitted('update:modelValue')).toEqual([['next']])
    expect(document.activeElement).toBe(trigger)
  })
})
