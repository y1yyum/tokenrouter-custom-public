<template>
  <div class="flex min-w-0 flex-col gap-0.5">
    <div v-for="metric in visibleMetrics" :key="metric.key" class="flex min-w-0 items-center">
      <CapacityBadge
        v-if="metric.kind === 'capacity'"
        class="capacity-summary"
        :color-class="metric.colorClass"
        :tooltip="metric.tooltip || metric.title"
        :current="metric.current"
        :max="metric.max"
        :suffix="metric.suffix"
      >
        <Icon :name="metric.icon" size="xs" class="h-2.5 w-2.5 shrink-0" />
      </CapacityBadge>
      <QuotaBadge
        v-else
        class="capacity-summary"
        :used="metric.used"
        :limit="metric.limit"
        :label="metric.label"
      />
    </div>
    <div v-if="compact">
      <ProviderCellDetails :label="t('admin.providers.compact.capacityDetails')">
        <template #trigger>
          <span v-if="metrics.length > visibleMetrics.length"
            >+{{ metrics.length - visibleMetrics.length }}</span
          >
          <Icon v-else name="more" size="xs" />
        </template>
        <dl class="space-y-2">
          <div v-for="metric in metrics" :key="metric.key">
            <dt class="font-medium">{{ metric.title }}</dt>
            <dd v-if="metric.kind === 'capacity'">
              <CapacityBadge
                :color-class="metric.colorClass"
                :current="metric.current"
                :max="metric.max"
                :suffix="metric.suffix"
              />
              <p v-if="metric.tooltip" class="mt-0.5">{{ metric.tooltip }}</p>
            </dd>
            <dd v-else>
              <QuotaBadge :used="metric.used" :limit="metric.limit" :label="metric.label" />
              <p class="mt-0.5">{{
                t(
                  metric.used >= metric.limit
                    ? 'admin.providers.capacity.quota.exceeded'
                    : 'admin.providers.capacity.quota.normal',
                )
              }}</p>
            </dd>
          </div>
        </dl>
      </ProviderCellDetails>
    </div>
  </div>
</template>

<script setup lang="ts">
import Icon from '@/components/icons/Icon.vue'
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { Provider } from '@/api/admin/providers.types'
import CapacityBadge from '@/views/admin/providers/table/CapacityBadge.vue'
import QuotaBadge from '@/views/admin/providers/table/QuotaBadge.vue'
import ProviderCellDetails from './ProviderCellDetails.vue'

/**
 * ProviderCapacityCell.vue displays provider concurrency, Anthropic window-cost and session
 * capacity, request-rate tiers, and local spending quotas. Platform and authentication
 * predicates select applicable badges. Missing current observations use zero in these capacity
 * displays, while positive configured limits determine whether optional capacity dimensions are
 * shown.
 *
 * Window cost and tiered RPM distinguish normal admission from the sticky reserve and
 * exhaustion; sticky-exempt RPM uses a different upper-state rule. Warning colors begin at
 * eighty percent of the base limit. Daily, weekly, monthly, and cumulative spending badges are
 * independently enabled for API-key and Bedrock providers, using the effective usage already
 * supplied on the provider record.
 */

/**
 * props supplies provider capacity observations and configured limits, with eligibility
 * predicates selecting the applicable badges for this table row.
 */
const props = defineProps<{
  provider: Provider // current provider capacity record
  compact?: boolean // Keep only the first three metrics in the row.
}>()

/**
 * The translator provides capacity explanations for normal, sticky-only, full, and exhausted
 * states derived from the configured limits.
 */
const { t } = useI18n()

/**
 * Concurrency state treats a missing observation as zero, uses red at configured capacity, and
 * yellow for nonzero activity below capacity.
 */
const currentConcurrency = computed(() => props.provider.current_concurrency || 0)

const concurrencyClass = computed(() => {
  const current = currentConcurrency.value
  const max = props.provider.concurrency
  if (current >= max) return 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400'
  if (current > 0) return 'bg-yellow-100 text-yellow-700 dark:bg-yellow-900/30 dark:text-yellow-400'
  return 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-dark-300'
})

/**
 * Anthropic OAuth and setup-token records can display a positive window-cost limit, with
 * current cost and sticky reserve controlling badge severity.
 */
const isAnthropicOAuthOrSetupToken = computed(
  () =>
    props.provider.platform === 'anthropic' &&
    (props.provider.type === 'oauth' || props.provider.type === 'setup-token'),
)

const showWindowCost = computed(
  () =>
    isAnthropicOAuthOrSetupToken.value &&
    props.provider.window_cost_limit != null &&
    props.provider.window_cost_limit > 0,
)

const currentWindowCost = computed(() => props.provider.current_window_cost ?? 0)

const windowCostClass = computed(() => {
  if (!showWindowCost.value) return ''
  const current = currentWindowCost.value
  const limit = props.provider.window_cost_limit || 0
  const reserve = props.provider.window_cost_sticky_reserve || 10
  if (current >= limit + reserve)
    return 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400'
  if (current >= limit)
    return 'bg-orange-100 text-orange-700 dark:bg-orange-900/30 dark:text-orange-400'
  // Warn when spending reaches eighty percent of base capacity.
  if (current >= limit * 0.8)
    return 'bg-yellow-100 text-yellow-700 dark:bg-yellow-900/30 dark:text-yellow-400'
  return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-400'
})

const windowCostTooltip = computed(() => {
  if (!showWindowCost.value) return ''
  const current = currentWindowCost.value
  const limit = props.provider.window_cost_limit || 0
  const reserve = props.provider.window_cost_sticky_reserve || 10
  if (current >= limit + reserve) return t('admin.providers.capacity.windowCost.blocked')
  if (current >= limit) return t('admin.providers.capacity.windowCost.stickyOnly')
  return t('admin.providers.capacity.windowCost.normal')
})

/**
 * Session capacity applies to eligible Anthropic records with a positive maximum; missing
 * active sessions use zero in colors and hints.
 */
const showSessionLimit = computed(
  () =>
    isAnthropicOAuthOrSetupToken.value &&
    props.provider.max_sessions != null &&
    props.provider.max_sessions > 0,
)

const activeSessions = computed(() => props.provider.active_sessions ?? 0)

const sessionLimitClass = computed(() => {
  if (!showSessionLimit.value) return ''
  const current = activeSessions.value
  const max = props.provider.max_sessions || 0
  if (current >= max) return 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400'
  // Warn before the active-session count reaches its configured maximum.
  if (current >= max * 0.8)
    return 'bg-yellow-100 text-yellow-700 dark:bg-yellow-900/30 dark:text-yellow-400'
  return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-400'
})

const sessionLimitTooltip = computed(() => {
  if (!showSessionLimit.value) return ''
  const current = activeSessions.value
  const max = props.provider.max_sessions || 0
  const idle = props.provider.session_idle_timeout_minutes || 5
  if (current >= max) return t('admin.providers.capacity.sessions.full', { idle })
  return t('admin.providers.capacity.sessions.normal', { idle })
})

/**
 * RPM capacity combines the configured base, strategy, current observation, and sticky buffer
 * to distinguish normal, sticky-only, and blocked states.
 */
const showRpmLimit = computed(
  () =>
    isAnthropicOAuthOrSetupToken.value &&
    props.provider.base_rpm != null &&
    props.provider.base_rpm > 0,
)

const currentRPM = computed(() => props.provider.current_rpm ?? 0)
const rpmStrategy = computed(() => props.provider.rpm_strategy || 'tiered')
const rpmStrategyTag = computed(() => (rpmStrategy.value === 'sticky_exempt' ? '[S]' : '[T]'))

/**
 * rpmBuffer uses an explicitly supplied buffer, including zero; otherwise positive base RPM
 * derives a minimum-one buffer from one fifth of base.
 */
const rpmBuffer = computed(() => {
  const base = props.provider.base_rpm || 0
  // Derive a minimum-one reserve when an explicit sticky buffer is absent.
  return props.provider.rpm_sticky_buffer ?? (base > 0 ? Math.max(1, Math.floor(base / 5)) : 0)
})

const rpmClass = computed(() => {
  if (!showRpmLimit.value) return ''
  const current = currentRPM.value
  const base = props.provider.base_rpm ?? 0
  const buffer = rpmBuffer.value
  if (rpmStrategy.value === 'tiered') {
    if (current >= base + buffer)
      return 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400'
    if (current >= base)
      return 'bg-orange-100 text-orange-700 dark:bg-orange-900/30 dark:text-orange-400'
  } else {
    if (current >= base)
      return 'bg-orange-100 text-orange-700 dark:bg-orange-900/30 dark:text-orange-400'
  }
  // Warn when current RPM approaches the base admission threshold.
  if (current >= base * 0.8)
    return 'bg-yellow-100 text-yellow-700 dark:bg-yellow-900/30 dark:text-yellow-400'
  return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-400'
})

const rpmTooltip = computed(() => {
  if (!showRpmLimit.value) return ''
  const current = currentRPM.value
  const base = props.provider.base_rpm ?? 0
  const buffer = rpmBuffer.value
  if (rpmStrategy.value === 'tiered') {
    if (current >= base + buffer) return t('admin.providers.capacity.rpm.tieredBlocked', { buffer })
    if (current >= base) return t('admin.providers.capacity.rpm.tieredStickyOnly', { buffer })
    // Warn when current RPM approaches the base admission threshold.
    if (current >= base * 0.8) return t('admin.providers.capacity.rpm.tieredWarning')
    return t('admin.providers.capacity.rpm.tieredNormal')
  } else {
    if (current >= base) return t('admin.providers.capacity.rpm.stickyExemptOver')
    // Warn when current RPM approaches the base admission threshold.
    if (current >= base * 0.8) return t('admin.providers.capacity.rpm.stickyExemptWarning')
    return t('admin.providers.capacity.rpm.stickyExemptNormal')
  }
})

/**
 * formatCost renders a current window cost with two fractional digits. Null or undefined
 * observations display zero, matching the capacity badge treatment of missing current usage
 * rather than an unknown placeholder.
 */
const formatCost = (value: number | null | undefined) => {
  if (value === null || value === undefined) return '0'
  return value.toFixed(2)
}

/**
 * Local spending badges apply to API-key and Bedrock records, with each positive daily, weekly,
 * monthly, or total limit displayed independently.
 */
const isQuotaEligible = computed(
  () => props.provider.type === 'apikey' || props.provider.type === 'bedrock',
)

const showDailyQuota = computed(
  () =>
    isQuotaEligible.value &&
    props.provider.quota_daily_limit != null &&
    props.provider.quota_daily_limit > 0,
)
const showWeeklyQuota = computed(
  () =>
    isQuotaEligible.value &&
    props.provider.quota_weekly_limit != null &&
    props.provider.quota_weekly_limit > 0,
)
/**
 * showMonthlyQuota exposes a monthly spending badge when the eligible provider has a positive
 * monthly limit, independently of other quota dimensions.
 */
const showMonthlyQuota = computed(
  () =>
    isQuotaEligible.value &&
    props.provider.quota_monthly_limit != null &&
    props.provider.quota_monthly_limit > 0,
)
const showTotalQuota = computed(
  () =>
    isQuotaEligible.value && props.provider.quota_limit != null && props.provider.quota_limit > 0,
)

// CapacityMetric preserves the existing badge inputs alongside a readable detail label.
type CapacityMetric =
  | {
      kind: 'capacity'
      key: string
      title: string
      icon: 'grid' | 'dollar' | 'users' | 'clock'
      colorClass: string
      current: string | number
      max: string | number
      suffix?: string
      tooltip?: string
    }
  | {
      kind: 'quota'
      key: string
      title: string
      used: number
      limit: number
      label?: string
    }

// metrics uses the same eligibility and observations for inline and expanded values.
const metrics = computed<CapacityMetric[]>(() => {
  const provider = props.provider
  const items: CapacityMetric[] = [
    {
      kind: 'capacity',
      key: 'concurrency',
      title: t('admin.providers.concurrency'),
      icon: 'grid',
      colorClass: concurrencyClass.value,
      current: currentConcurrency.value,
      max: provider.concurrency,
    },
  ]
  if (showWindowCost.value) {
    items.push({
      kind: 'capacity',
      key: 'windowCost',
      title: t('admin.providers.compact.windowCost'),
      icon: 'dollar',
      colorClass: windowCostClass.value,
      current: '$' + formatCost(currentWindowCost.value),
      max: '$' + formatCost(provider.window_cost_limit),
      tooltip: windowCostTooltip.value,
    })
  }
  if (showSessionLimit.value) {
    items.push({
      kind: 'capacity',
      key: 'session',
      title: t('admin.providers.compact.sessions'),
      icon: 'users',
      colorClass: sessionLimitClass.value,
      current: activeSessions.value,
      max: provider.max_sessions!,
      tooltip: sessionLimitTooltip.value,
    })
  }
  if (showRpmLimit.value) {
    items.push({
      kind: 'capacity',
      key: 'rpm',
      title: t('admin.providers.compact.rpm'),
      icon: 'clock',
      colorClass: rpmClass.value,
      current: currentRPM.value,
      max: provider.base_rpm!,
      suffix: rpmStrategyTag.value,
      tooltip: rpmTooltip.value,
    })
  }

  const quotas = [
    {
      key: 'daily',
      used: provider.quota_daily_used,
      limit: provider.quota_daily_limit,
      label: 'D',
      enabled: showDailyQuota.value,
    },
    {
      key: 'weekly',
      used: provider.quota_weekly_used,
      limit: provider.quota_weekly_limit,
      label: 'W',
      enabled: showWeeklyQuota.value,
    },
    {
      key: 'monthly',
      used: provider.quota_monthly_used,
      limit: provider.quota_monthly_limit,
      label: 'M',
      enabled: showMonthlyQuota.value,
    },
    {
      key: 'total',
      used: provider.quota_used,
      limit: provider.quota_limit,
      enabled: showTotalQuota.value,
    },
  ]
  for (const quota of quotas) {
    if (!quota.enabled || quota.limit == null) continue
    items.push({
      kind: 'quota',
      key: quota.key,
      title: t('admin.providers.compact.' + quota.key + 'Quota'),
      used: quota.used ?? 0,
      limit: quota.limit,
      label: quota.label,
    })
  }
  return items
})

const visibleMetrics = computed(() => (props.compact ? metrics.value.slice(0, 3) : metrics.value))
</script>

<style scoped>
.capacity-summary {
  max-width: 100%;
  overflow: hidden;
  white-space: nowrap;
}
</style>
