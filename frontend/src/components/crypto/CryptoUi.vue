<template>
  <Teleport to="body">
    <div :id="portalId" data-crypto-ui />
  </Teleport>
  <UApp :locale="uiLocale" :portal="'#' + portalId" :toaster="null">
    <slot />
  </UApp>
</template>

<script setup lang="ts">
import { computed, useId } from 'vue'
import { useI18n } from 'vue-i18n'
import UApp from '@nuxt/ui/components/App.vue'
import { en, zh_cn } from '@nuxt/ui/locale'
import { _api, addIcon } from '@iconify/vue'
import { init } from 'virtual:nuxt-ui-icons'
import '@/styles/crypto-ui.css'

// CryptoUi.vue provides localized Nuxt UI controls and a styled portal for the
// deposit and administration pages. Icons use the bundled Lucide collection.
_api.setFetch(async () => {
  throw new Error('Crypto UI icons are available only from the local bundle')
})
init(addIcon)

const portalId = useId()
const { locale } = useI18n()
const uiLocale = computed(() => (locale.value === 'zh' ? zh_cn : en))
</script>
