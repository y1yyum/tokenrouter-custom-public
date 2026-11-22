import { flushPromises, shallowMount } from '@vue/test-utils'
import { nextTick, reactive, ref } from 'vue'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { attribute, directive, parseVue } from '@/__tests__/helpers/sourceSyntax'
import CustomPageView from '../CustomPageView.vue'

// CustomPageView.spec.ts follows a configured menu through theme, language, and
// route changes. The mounted view uses local settings and Markdown fixtures.
// The homepage template check covers its independent full-page frame binding.

// The route and locale are reactive so each navigation exercises the existing
// component instance, including its theme observer and Markdown request watcher.
const route = reactive({ params: { id: 'migrated_purchase_subscription' } })
const locale = ref('en')

vi.mock('vue-router', async (importOriginal) => ({
  ...(await importOriginal<typeof import('vue-router')>()),
  useRoute: () => route,
}))
vi.mock('vue-i18n', async (importOriginal) => ({
  ...(await importOriginal<typeof import('vue-i18n')>()),
  useI18n: () => ({ t: (key: string) => key, locale }),
}))
vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    isAdmin: false,
    // user rejects identity reads while the caller resolves an ordinary public menu.
    get user(): never {
      throw new Error('Custom menu requested the host account')
    },
    // token rejects credential reads without provisioning a session in the test.
    get token(): never {
      throw new Error('Custom menu requested the host session')
    },
  }),
}))
vi.mock('@/stores/adminSettings', () => ({ useAdminSettingsStore: () => ({}) }))
vi.mock('@/stores', () => ({
  useAppStore: () => ({
    publicSettingsLoaded: true,
    cachedPublicSettings: {
      custom_menu_items: [
        { id: 'migrated_purchase_subscription', url: 'https://checkout.example.test/?plan=a#pay' },
        { id: 'article', url: 'md:guide' },
      ],
    },
  }),
}))
vi.mock('@/api/pages', () => ({
  fetchPageMarkdown: async () => ({
    ok: true,
    contentLanguage: 'en',
    text: '# Guide\n\n<iframe src="https://content.example.test/guide"></iframe>',
  }),
}))

afterEach(() => {
  document.documentElement.classList.remove('dark')
})

describe('configured page frames', () => {
  // A user changes the display settings on a migrated purchase menu and opens
  // a Markdown menu. Both payment controls follow the settings, and each frame
  // suppresses its referrer before the article or checkout page is shown.
  it('updates both checkout destinations and applies the referrer policy to Markdown frames', async () => {
    const wrapper = shallowMount(CustomPageView, {
      global: { stubs: { AppLayout: { template: '<slot />' } } },
    })
    try {
      const frame = wrapper.get('iframe')
      const link = wrapper.get('a[target="_blank"]')
      expect(frame.attributes('src')).toBe(
        'https://checkout.example.test/?plan=a&theme=light&lang=en&ui_mode=embedded#pay',
      )
      expect(link.attributes('href')).toBe(frame.attributes('src'))
      expect(link.attributes('rel')?.split(/\s+/)).toEqual(
        expect.arrayContaining(['noopener', 'noreferrer']),
      )
      expect(frame.attributes('referrerpolicy')).toBe('no-referrer')

      document.documentElement.classList.add('dark')
      locale.value = 'zh'
      await flushPromises()
      expect(frame.attributes('src')).toBe(
        'https://checkout.example.test/?plan=a&theme=dark&lang=zh&ui_mode=embedded#pay',
      )
      expect(link.attributes('href')).toBe(frame.attributes('src'))

      route.params.id = 'article'
      await flushPromises()
      expect(wrapper.get('h1').text()).toBe('Guide')
      expect(wrapper.get('iframe').attributes()).toMatchObject({
        src: 'https://content.example.test/guide',
        referrerpolicy: 'no-referrer',
      })
      expect(wrapper.find('a[target="_blank"]').exists()).toBe(false)

      route.params.id = 'missing'
      await nextTick()
      expect(wrapper.find('iframe').exists()).toBe(false)
    } finally {
      wrapper.unmount()
    }
  })

  // A configured homepage URL has a separate rendering path. Its frame must
  // preserve the configured address and suppress the referrer on its first navigation.
  it('sets the referrer policy on the frame bound to the configured home URL', () => {
    const frames = parseVue('views/HomeView.vue').elements.filter(
      (element) => element.tag === 'iframe',
    )
    expect(frames).toHaveLength(1)
    expect(directive(frames[0], 'bind', 'src')).toBe('homeContent.trim()')
    expect(attribute(frames[0], 'referrerpolicy')).toBe('no-referrer')
  })
})
