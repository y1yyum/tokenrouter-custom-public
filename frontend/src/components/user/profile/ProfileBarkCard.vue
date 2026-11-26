<!-- ProfileBarkCard.vue manages the account's Bark notification settings. -->
<template>
  <div
    class="overflow-hidden rounded-surface border border-primary-900/10 bg-white dark:border-dark-500 dark:bg-dark-900"
  >
    <div class="border-b border-primary-900/10 dark:border-dark-500 px-5 py-4 sm:px-6">
      <h2 class="text-base font-semibold text-primary-900 dark:text-dark-50">
        {{ title || t('profile.bark.title') }}
      </h2>
      <p class="mt-1 text-sm text-gray-600 dark:text-dark-300">
        {{ description || t('profile.bark.description') }}
      </p>
    </div>
    <div class="space-y-6 p-5 sm:p-6">
      <div v-if="loadError" role="alert" class="text-sm text-red-600 dark:text-red-400">
        <p>{{ loadError }}</p>
        <button
          type="button"
          class="btn btn-secondary mt-2"
          :disabled="loading"
          data-test="bark-refresh"
          @click="refresh"
        >
          {{ t('common.refresh') }}
        </button>
      </div>
      <p class="text-sm text-gray-500 dark:text-gray-400">{{
        t('profile.bark.changeVerificationHint')
      }}</p>
      <div class="flex items-center gap-2 text-sm">
        <span
          class="inline-block h-2 w-2 rounded-full"
          :class="status.configured ? 'bg-green-500' : 'bg-gray-400'"
        ></span>
        <span class="text-gray-500 dark:text-gray-400">
          {{ status.configured ? t('profile.bark.configured') : t('profile.bark.notConfigured') }}
        </span>
      </div>

      <div>
        <label for="bark-device-key" class="input-label">{{ t('profile.bark.deviceKey') }}</label>
        <input
          id="bark-device-key"
          v-model="deviceKey"
          type="password"
          autocomplete="off"
          spellcheck="false"
          :disabled="loading || saving || !!loadError"
          class="input w-full"
          :placeholder="t('profile.bark.deviceKeyPlaceholder')"
        />
        <p class="mt-1 text-xs text-gray-400">{{
          t(status.configured ? 'profile.bark.keepKeyHint' : 'profile.bark.deviceKeyHint')
        }}</p>
      </div>

      <div class="flex items-center justify-between">
        <label for="bark-enabled" class="input-label mb-0">{{ t('profile.bark.enabled') }}</label>
        <Toggle
          id="bark-enabled"
          v-model="enabled"
          :aria-label="t('profile.bark.enabled')"
          :disabled="loading || saving || !!loadError"
          data-test="bark-enabled"
        />
      </div>

      <div class="flex items-center gap-2">
        <button
          type="button"
          class="btn btn-primary"
          :disabled="loading || saving || !!loadError || (!deviceKey.trim() && !status.configured)"
          data-test="bark-save"
          @click="save"
        >
          {{ saving ? t('common.saving') : t('profile.bark.save') }}
        </button>
        <button
          v-if="status.configured"
          type="button"
          class="btn btn-secondary"
          :disabled="loading || saving || !!loadError"
          data-test="bark-clear"
          @click="clear"
        >
          {{ t('profile.bark.clear') }}
        </button>
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import Toggle from '@/components/common/Toggle.vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { userAPI, type BarkSettings } from '@/api/user'
import { extractApiErrorMessage } from '@/utils/apiError'

const { t } = useI18n()
const appStore = useAppStore()
defineProps<{ title?: string; description?: string }>()

const status = ref<BarkSettings>({ configured: false, enabled: false })
const deviceKey = ref('')
const enabled = ref(false)
const saving = ref(false)
const loading = ref(true)
const loadError = ref('')
let disposed = false

async function refresh() {
  loading.value = true
  loadError.value = ''
  try {
    status.value = await userAPI.getBarkSettings()
    enabled.value = status.value.enabled
  } catch {
    loadError.value = t('profile.bark.loadFailed')
  } finally {
    loading.value = false
  }
}
onMounted(refresh)
onUnmounted(() => {
  disposed = true
  deviceKey.value = ''
})

async function requestChange(key: string | undefined, nextEnabled: boolean, successKey: string) {
  saving.value = true
  try {
    // The backend keeps this endpoint step-up free: the Bark key only controls
    // where this user's own notifications go, it is encrypted at rest, and the
    // original device is notified before the change is applied.
    await userAPI.updateBarkSettings(key, nextEnabled)
    deviceKey.value = ''
    await refresh()
    if (!disposed) appStore.showSuccess(t(successKey))
  } catch (err: unknown) {
    if (!disposed) appStore.showError(extractApiErrorMessage(err, t('common.error')))
  } finally {
    saving.value = false
  }
}

async function save() {
  if (
    loading.value ||
    saving.value ||
    loadError.value ||
    (!deviceKey.value.trim() && !status.value.configured)
  )
    return
  await requestChange(deviceKey.value.trim() || undefined, enabled.value, 'profile.bark.saved')
}

async function clear() {
  if (loading.value || saving.value || loadError.value || !status.value.configured) return
  await requestChange('', false, 'profile.bark.cleared')
}
</script>
