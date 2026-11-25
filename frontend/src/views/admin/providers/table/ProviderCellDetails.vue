<!-- ProviderCellDetails.vue opens compact cell details outside the table's scroll container. -->
<template>
  <button
    v-if="!inline"
    ref="trigger"
    type="button"
    class="inline-flex min-w-0 max-w-full shrink-0 items-center rounded-compact px-1 text-xs leading-4 text-link focus-visible:outline focus-visible:outline-2 focus-visible:outline-primary-600"
    :aria-label="label"
    :aria-expanded="open"
    :aria-controls="open || keepMounted ? panelId : undefined"
    :title="label"
    @click.stop="toggle"
  >
    <slot name="trigger">{{ label }}</slot>
  </button>

  <Teleport to="body" :disabled="inline">
    <div
      v-if="inline || keepMounted || open"
      v-show="inline || open"
      :id="inline ? undefined : panelId"
      ref="panel"
      :role="inline ? undefined : 'dialog'"
      :aria-label="inline ? undefined : label"
      :tabindex="inline ? undefined : -1"
      :class="
        inline
          ? undefined
          : 'fixed z-teleport-dropdown overflow-auto rounded-surface border border-gray-200 bg-white p-3 text-xs text-gray-700 shadow-lg dark:border-dark-600 dark:bg-dark-900 dark:text-gray-200'
      "
      :style="inline ? undefined : style"
      @keydown.esc="onEscape"
    >
      <button
        v-if="!inline"
        type="button"
        class="float-right ml-2 rounded-compact p-0.5 hover:bg-gray-100 dark:hover:bg-dark-700"
        :aria-label="t('common.close')"
        @click="close(true)"
      >
        <Icon name="x" size="xs" />
      </button>
      <slot :close="close" />
    </div>
  </Teleport>
</template>

<script setup lang="ts">
import { nextTick, ref, useId, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import { useFloatingMotion } from '@/composables/useFloatingMotion'
import { getFloatingPanelPosition } from '@/utils/floatingPanel'

// ProviderCellDetails keeps provider detail panels out of the table scroll
// container. Usage can retain its mounted controls and their request state.
defineProps<{
  label: string // Accessible detail name.
  inline?: boolean // Render the body in place for the regular cell.
  keepMounted?: boolean // Preserve hidden content and its active observations.
}>()

const { t } = useI18n()
const panelId = useId()
const trigger = ref<HTMLButtonElement | null>(null)
const panel = ref<HTMLElement | null>(null)
const open = ref(false)
const style = ref<Record<string, string>>({})

// close restores focus for keyboard dismissal and explicit close actions.
function close(restoreFocus = false) {
  if (!open.value) return
  open.value = false
  if (restoreFocus) trigger.value?.focus()
}

// Escape closes the active dialog without dismissing an enclosing page overlay.
function onEscape(event: KeyboardEvent) {
  if (!open.value) return
  event.stopPropagation()
  close(true)
}

// reposition keeps the panel inside the viewport as the table or window scrolls.
function reposition() {
  if (!trigger.value) return
  const rect = trigger.value.getBoundingClientRect()
  if (rect.bottom < 0 || rect.top > window.innerHeight) {
    close()
    return
  }
  const pos = getFloatingPanelPosition(
    rect,
    document.documentElement.clientWidth || window.innerWidth,
    window.innerHeight,
    { maxWidth: 384, align: 'left' },
  )
  style.value = {
    top: pos.top === null ? 'auto' : pos.top + 'px',
    bottom: pos.bottom === null ? 'auto' : pos.bottom + 'px',
    left: pos.left + 'px',
    width: pos.width + 'px',
    maxHeight: pos.maxHeight + 'px',
  }
}

// toggle transfers focus into the teleported panel, where its controls follow
// the panel heading in the tab order.
async function toggle() {
  if (open.value) {
    close(true)
    return
  }
  reposition()
  open.value = true
  await nextTick()
  if (open.value) panel.value?.focus()
}

useFloatingMotion(trigger, () => open.value, close, reposition)

// Install document listeners only for the open panel.
watch(open, (visible, _, onCleanup) => {
  if (!visible) return
  const handleOutside = (event: Event) => {
    if (!(event.target instanceof Node)) return
    if (trigger.value?.contains(event.target) || panel.value?.contains(event.target)) return
    close()
  }
  document.addEventListener('pointerdown', handleOutside, true)
  document.addEventListener('focusin', handleOutside, true)
  window.addEventListener('scroll', reposition, true)
  window.addEventListener('resize', reposition)
  onCleanup(() => {
    document.removeEventListener('pointerdown', handleOutside, true)
    document.removeEventListener('focusin', handleOutside, true)
    window.removeEventListener('scroll', reposition, true)
    window.removeEventListener('resize', reposition)
  })
})
</script>

