<template>
  <div :class="{ 'min-w-0': compact }">
    <div
      v-if="compact"
      class="truncate font-mono text-xs text-gray-600 dark:text-gray-400"
      :title="ip || undefined"
    >
      {{ ip || '-' }}
    </div>

    <template v-if="!compact || ip">
      <div v-if="entry.status === 'idle'" class="text-xs" :class="{ 'mt-0.5': !compact }">
        <button
          type="button"
          class="text-primary-600 underline decoration-dashed underline-offset-2 hover:text-primary-700 dark:text-primary-400 dark:hover:text-primary-300"
          @click="handleFetch"
        >
          {{ t('usage.ipGeo.fetch') }}
        </button>
      </div>

      <div
        v-else-if="entry.status === 'loading'"
        :class="{ 'mt-1': !compact }"
        role="status"
        :aria-label="t('common.loading')"
        aria-busy="true"
        data-loading-skeleton
      >
        <Skeleton :width="96" :height="12" />
      </div>

      <div
        v-else-if="entry.status === 'success'"
        class="flex items-center gap-1 text-xs"
        :class="{ 'mt-0.5': !compact }"
      >
        <button
          type="button"
          class="truncate text-link"
          :class="{ 'min-w-0 text-left': compact }"
          :title="tooltipText"
          @click="handleOpenDetail"
        >
          {{ entry.label }}
        </button>
        <button
          type="button"
          class="text-gray-400 hover:text-primary-600 dark:hover:text-primary-400"
          :class="{ 'shrink-0': compact }"
          :title="t('usage.ipGeo.refreshTitle')"
          @click="handleRefresh"
        >
          <Icon name="refresh" size="xs" />
        </button>
      </div>

      <div v-else-if="entry.status === 'error'" class="text-xs" :class="{ 'mt-0.5': !compact }">
        <button
          type="button"
          class="text-red-600 underline decoration-dashed underline-offset-2 hover:text-red-700 dark:text-red-400 dark:hover:text-red-300"
          @click="handleFetch"
        >
          {{ t('usage.ipGeo.failed') }}
        </button>
      </div>

      <div v-else class="text-xs text-gray-400 dark:text-gray-500" :class="{ 'mt-0.5': !compact }">
        {{ t('usage.ipGeo.private') }}
      </div>
    </template>
  </div>
</template>

<script setup lang="ts">
import Skeleton from '@/components/common/Skeleton.vue'
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import { fetchOne, getEntry } from '@/utils/ipGeoLookup'

/**
 * IpGeoCell.vue displays one IP lookup cache entry and exposes fetch, retry, refresh,
 * and external detail actions. The lookup utility owns request state, so the cell
 * reads the shared entry reactively and renders private addresses as text. Compact
 * cells show the IP above the lookup state and let the location label shrink beside
 * the refresh control while keeping the complete label available on hover.
 */

/**
 * props supplies the address used for the shared lookup entry, refresh requests, and
 * external detail URL, along with a display setting for narrow table columns.
 */
const props = defineProps<{
  ip: string // lookup IP address
  /**
   * compact renders the address above the lookup state, or only a placeholder when
   * the address is empty. Standard callers render their own address above this cell.
   */
  compact?: boolean
}>()
/**
 * The i18n translator resolves this component's message keys using the current
 * application locale and its configured translation catalog.
 */
const { t } = useI18n()

/**
 * entry reads the shared lookup record for the current IP so all cells observe cache
 * state changes.
 */
const entry = computed(() => getEntry(props.ip))

/**
 * tooltipText lists available organization, timezone, accuracy, and coordinate details.
 * It starts with the full location label in compact cells, where width may clip the
 * visible label, including when the lookup supplies no additional details.
 */
const tooltipText = computed(() => {
  const detail = entry.value.detail
  const lines = [
    props.compact ? entry.value.label : '',
    detail?.organization ? `${t('usage.ipGeo.detailOrg')}: ${detail.organization}` : '',
    detail?.timezone ? `${t('usage.ipGeo.detailTimezone')}: ${detail.timezone}` : '',
    detail?.accuracy != null ? `${t('usage.ipGeo.detailAccuracy')}: ${detail.accuracy}km` : '',
    detail?.latitude && detail.longitude
      ? `${t('usage.ipGeo.detailCoordinates')}: ${detail.latitude}, ${detail.longitude}`
      : '',
  ].filter(Boolean)
  return lines.join('\n')
})

/**
 * handleFetch requests the current IP through the shared lookup utility.
 */
const handleFetch = () => {
  void fetchOne(props.ip)
}

/**
 * handleRefresh requests the current IP with the force flag enabled.
 */
const handleRefresh = () => {
  void fetchOne(props.ip, true)
}

/**
 * handleOpenDetail opens the encoded IP in an external lookup page. The new browsing
 * context receives noopener and noreferrer features so the request detail action is
 * isolated from this application window.
 */
const handleOpenDetail = () => {
  window.open(
    `https://www.iplocation.net/ip-lookup?query=${encodeURIComponent(props.ip)}`,
    '_blank',
    'noopener,noreferrer',
  )
}
</script>
