<template>
  <section class="card space-y-4 p-4" :aria-label="heading">
    <header>
      <h3 class="text-sm font-semibold text-gray-900 dark:text-white">{{ heading }}</h3>
      <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
        {{ t('admin.usage.imported.billing') }}
      </p>
    </header>
    <div class="flex flex-wrap items-center gap-2">
      <div v-segmented class="segmented" role="group" :aria-label="t('admin.dashboard.timeRange')">
        <button
          v-for="scope in scopes"
          :key="scope"
          type="button"
          class="segmented-item px-3 py-1.5 text-sm"
          :class="{ 'segmented-item-active': dateScope === scope }"
          :aria-pressed="dateScope === scope"
          @click="dateScope = scope"
          >{{ t('admin.usage.imported.' + scope) }}</button
        >
      </div>
      <slot v-if="dateScope === 'selectedDates'" name="selected-dates">
        <span class="text-sm text-gray-500 dark:text-gray-400"
          >{{ startDate }} – {{ endDate }}</span
        >
      </slot>
    </div>
    <form
      v-if="view === 'minutes'"
      class="flex flex-wrap items-end gap-2"
      @submit.prevent="model = modelDraft"
    >
      <div class="w-48 max-w-full">
        <label class="input-label" for="raks-group">{{ t('admin.dashboard.group') }}</label>
        <Select
          id="raks-group"
          v-model="groupId"
          :options="groupOptions"
          :aria-label="t('admin.dashboard.group')"
        />
      </div>
      <label>
        <span class="input-label">{{ t('admin.usage.imported.exactModel') }}</span>
        <input
          v-model="modelDraft"
          type="search"
          class="input"
          :placeholder="t('admin.usage.allModels')"
        />
      </label>
      <button type="submit" class="btn btn-secondary">{{ t('common.search') }}</button>
    </form>
    <LoadErrorState
      v-if="failed"
      :message="t('admin.usage.imported.failedToLoad')"
      @retry="reload"
    />
    <template v-else>
      <DataTable
        :columns="columns"
        :data="rows"
        row-key="id"
        :loading="!report"
        class="tabular-nums"
      >
        <template #empty>{{ t('admin.usage.imported.empty') }}</template>
      </DataTable>
      <Pagination
        v-if="view === 'minutes' && report && report.total"
        :page="page"
        :total="report.total"
        :page-size="pageSize"
        :show-page-size-selector="false"
        show-jump
        @update:page="page = $event"
      />
    </template>
  </section>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  getImportedCalls,
  type ImportedCallGroup,
  type ImportedCallReport,
} from '@/api/admin/usage'
import Select from '@/components/common/Select.vue'
import DataTable from '@/components/common/table/DataTable.vue'
import LoadErrorState from '@/components/common/LoadErrorState.vue'
import Pagination from '@/components/common/Pagination.vue'
import { vSegmented } from '@/directives/segmented'
import {
  formatDateTimeToMinute,
  formatNumberLocaleString,
  getBrowserTimeZone,
} from '@/utils/format'

// ImportedCallsPanel.vue displays imported group totals on the dashboard and paged
// minute records on the usage page. Both views read the same source counters and
// calculate each latency from the samples that actually recorded that measurement.
// The initial query reads the entire archive. The selected-date option adopts the
// owning page's local date bounds, while minute filters and paging belong to this instance.
//
// Only a completed query can expose pages for navigation. A new selection invalidates
// the previous response, but the group selector keeps the full set for its date range
// even when an exact model query matches no minutes.

// The parent owns both local date bounds and selects the endpoint projection.
const props = defineProps<{
  startDate: string // Local start bound.
  endDate: string // Local end bound.
  view: 'groups' | 'minutes' // Historical aggregation level.
}>()
const { t } = useI18n()
const heading = computed(() =>
  t(props.view === 'groups' ? 'admin.usage.imported.groups' : 'admin.usage.imported.minutes'),
)

// Parent date edits only affect the query while the selected-date scope is active.
const scopes = ['allHistory', 'selectedDates'] as const
const dateScope = ref<(typeof scopes)[number]>('allHistory')
const dateBounds = computed(() =>
  dateScope.value === 'selectedDates'
    ? { start_date: props.startDate, end_date: props.endDate }
    : undefined,
)

// Group choices are scoped to the dates. The submitted model can differ from the
// editable text until the administrator applies the search.
const groups = ref<ImportedCallGroup[]>([])
const groupId = ref<number | null>(null)
const modelDraft = ref('')
const model = ref('')
const groupOptions = computed(() => [
  { value: null, label: t('admin.usage.allGroups') },
  ...groups.value.map((group) => ({ value: group.group_id, label: group.group_name })),
])

// History pagination is instance-local so navigating here cannot change the native
// table's page or persist a different page size for export and cleanup screens.
const pageSize = 20
const page = ref(1)
const revision = ref(0)
const report = ref<ImportedCallReport | null>(null)
const failed = ref(false)

// reload repeats the current query including its active filters and page.
function reload() {
  revision.value++
}
defineExpose({ reload })

watch(dateBounds, () => {
  // The old date range may contain different groups. Release those choices before
  // constructing the query so an unavailable group cannot hide the new range.
  groups.value = []
  groupId.value = null
  page.value = 1
})
watch([groupId, model], () => {
  page.value = 1
})

const query = computed(() => ({
  ...dateBounds.value,
  timezone: getBrowserTimeZone(),
  view: props.view,
  ...(props.view === 'minutes' && {
    group_id: groupId.value ?? undefined,
    model: model.value || undefined,
    page: page.value,
    page_size: pageSize,
  }),
}))

watch(
  [query, revision],
  async ([params], _, onCleanup) => {
    const controller = new AbortController()
    // Replacing the query or leaving the view cancels this read. Check its signal
    // after settlement too, since a transport can finish while cancellation is pending.
    onCleanup(() => controller.abort())
    report.value = null
    failed.value = false
    try {
      const data = await getImportedCalls(params, controller.signal)
      if (controller.signal.aborted) return
      groups.value = data.groups
      report.value = data
    } catch {
      // A superseded failure belongs to an obsolete view and must not replace the
      // current table with its retry action.
      if (!controller.signal.aborted) failed.value = true
    }
  },
  { immediate: true },
)

// columns adds the minute and model identifiers only to the paged projection.
const columns = computed(() => [
  ...(props.view === 'minutes' ? [{ key: 'minute', label: t('admin.usage.imported.minute') }] : []),
  { key: 'group', label: t('admin.dashboard.group') },
  ...(props.view === 'minutes'
    ? [
        { key: 'model', label: t('admin.dashboard.model') },
        { key: 'platform', label: t('admin.usage.imported.platform') },
      ]
    : []),
  { key: 'requests', label: t('admin.dashboard.totalRequests') },
  { key: 'success', label: t('admin.usage.imported.success') },
  { key: 'errors', label: t('admin.usage.imported.errors') },
  { key: 'tokens', label: t('admin.dashboard.totalTokens') },
  { key: 'duration', label: t('admin.usage.imported.duration') },
  { key: 'ttft', label: t('admin.usage.imported.ttft') },
])

// averageMs renders the observed mean to the nearest millisecond. An absent
// sample has no mean, while an observed zero remains a numeric measurement.
function averageMs(sum: number, count: number) {
  return count > 0 ? formatNumberLocaleString(Math.round(sum / count)) : '—'
}

// metricCells formats the counters shared by both historical table projections.
function metricCells(row: ImportedCallGroup) {
  return {
    group: row.group_name,
    requests: formatNumberLocaleString(row.success_requests + row.error_requests),
    success: formatNumberLocaleString(row.success_requests),
    errors: formatNumberLocaleString(row.error_requests),
    tokens: formatNumberLocaleString(
      row.input_tokens + row.output_tokens + row.cache_creation_tokens + row.cache_read_tokens,
    ),
    duration: averageMs(row.duration_sum_ms, row.duration_count),
    ttft: averageMs(row.ttft_sum_ms, row.ttft_count),
  }
}

// rows retains the endpoint's ordering. A minute key includes platform so rows
// remain distinct when sources used the same group and model in one minute.
const rows = computed(() => {
  if (!report.value) return []
  if (props.view === 'groups') {
    return report.value.groups.map((group) => ({ id: group.group_id, ...metricCells(group) }))
  }
  return (report.value.items ?? []).map((item) => ({
    id: JSON.stringify([item.bucket_start, item.group_id, item.model, item.source_platform]),
    minute: formatDateTimeToMinute(item.bucket_start),
    model: item.model,
    platform: item.source_platform,
    ...metricCells(item),
  }))
})
</script>
