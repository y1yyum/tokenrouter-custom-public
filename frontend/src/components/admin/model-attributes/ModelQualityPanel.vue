<template>
  <section class="space-y-5">
    <form class="flex flex-wrap items-end gap-3" @submit.prevent="load">
      <label
        ><span class="input-label">{{ t(key + 'provider') }}</span>
        <input v-model="providerId" class="input w-36" type="number" min="1" step="1" required
      /></label>
      <label
        ><span class="input-label">{{ t(key + 'model') }}</span>
        <input v-model.trim="model" class="input" required maxlength="512" placeholder="gpt-5.4"
      /></label>
      <button class="btn btn-primary" :disabled="busy">{{ t('common.search') }}</button>
    </form>
    <p v-if="error" role="alert" class="text-sm text-red-600">{{ error }}</p>
    <template v-if="report">
      <button class="btn btn-secondary" type="button" @click="showRoute = true">{{
        t('admin.providers.advancedSchedulerScore.routeCheck')
      }}</button>
      <AdvancedSchedulerScoreModal
        :provider="{ id: report.provider_id }"
        :show="showRoute"
        @close="showRoute = false"
      />
      <p class="text-sm"
        >#{{ report.provider_id }} · {{ report.model }} · {{ report.from }} — {{ report.until }}</p
      >
      <div class="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <div
          v-for="[metric, numerator, denominator] in metrics"
          :key="metric"
          class="rounded-control border p-3"
        >
          <h3 class="text-sm">{{ t(key + metric) }}</h3>
          <p class="font-semibold">{{
            denominator ? ((100 * numerator) / denominator).toFixed(1) + '%' : 'Unknown'
          }}</p>
          <p class="text-xs"
            >{{ numerator }} / {{ denominator }} · {{ t(key + metric + 'Basis') }}</p
          >
        </div>
      </div>
      <h3 class="font-semibold">{{ t(key + 'owners') }}</h3>
      <p v-if="report.owners.length" class="text-sm">{{ t(key + 'ownerScope') }}</p>
      <p v-if="!report.owners.length">{{ t(key + 'noOwners') }}</p>
      <article
        v-for="owner in report.owners"
        :key="owner.plan_id"
        class="rounded-control border p-3 text-sm"
      >
        <strong>{{ t(key + owner.state) }}</strong> · {{ t(key + 'plan') }} #{{ owner.plan_id }} ·
        {{ t(key + 'result') }} #{{ owner.result_id }}
        <p>{{ owner.requested_model }} → {{ report.model }} · {{ owner.reason }}</p>
        <p>{{ owner.started_at }} · {{ t(key + 'expiry') }} {{ owner.expires_at ?? '—' }}</p>
      </article>
      <h3 class="font-semibold">{{ t(key + 'plans') }}</h3>
      <p v-if="!plans.length">{{ t(key + 'noPlans') }}</p>
      <article
        v-for="plan in plans"
        :key="plan.id"
        class="space-y-2 rounded-control border p-3 text-sm"
      >
        <strong>#{{ plan.id }} · {{ plan.model_id }}</strong> · {{ plan.cron_expression }} ·
        {{ t(key + (plan.enabled ? 'enabled' : 'paused')) }}
        <div class="flex flex-wrap gap-2">
          <button
            type="button"
            class="btn btn-secondary btn-sm"
            :disabled="busy"
            @click="toggle(plan)"
            >{{ t(key + (plan.enabled ? 'pause' : 'resume')) }}</button
          >
          <button
            type="button"
            class="btn btn-secondary btn-sm"
            :disabled="busy"
            @click="history(plan.id)"
            >{{ t(key + 'history') }}</button
          >
        </div>
        <template v-if="historyPlan === plan.id">
          <p v-if="!results.length">{{ t(key + 'noResults') }}</p>
          <p v-for="row in results" :key="row.id"
            >#{{ row.id }} · {{ row.started_at }} · {{ row.quality?.grade ?? row.status }} ·
            {{ row.quality?.upstream_model || 'Unknown' }} ·
            {{ row.quality?.reason || row.error_message }}</p
          >
        </template>
      </article>
    </template>
  </section>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import AdvancedSchedulerScoreModal from '@/components/admin/provider/AdvancedSchedulerScoreModal.vue'
import { useI18n } from 'vue-i18n'
import { scheduledTestsAPI } from '@/api/admin/scheduledTests'
import type {
  ModelQualityReport,
  ScheduledTestPlan,
  ScheduledTestResult,
} from '@/api/admin/scheduledTests.types'

// ModelQualityPanel.vue keeps plan controls, durable verdict ownership and the
// selected model's counters together. Plan status controls future probes. Each owner's
// persisted result and deadline explain why a paused plan can still restrict traffic.
const { t } = useI18n()
const key = 'admin.modelAttributes.quality.'
const showRoute = ref(false)
const providerId = ref<number | string>('')
const model = ref('')
const report = ref<ModelQualityReport>()
const plans = ref<ScheduledTestPlan[]>([])
const results = ref<ScheduledTestResult[]>([])
const historyPlan = ref(0)
const busy = ref(false)
const error = ref('')
let generation = 0

// Both the displayed percentage and the evidence below it use these same counts.
const metrics = computed<Array<[string, number, number]>>(() => {
  const r = report.value
  return r
    ? [
        ['availability', r.success, r.success + r.failed],
        ['cacheHit', r.cache_hits, r.cache_samples],
        ['cacheTokens', r.read_tokens, r.input_tokens],
        ['failure', r.wrong, r.graded],
      ]
    : []
})

watch(
  [providerId, model],
  () => {
    showRoute.value = false
    generation++
    report.value = undefined
    plans.value = []
    historyPlan.value = 0
    busy.value = false
    error.value = ''
  },
  { flush: 'sync' },
)

// Only the current identity may publish a response, including after a plan edit.
async function run(action: (seq: number) => Promise<void>) {
  const seq = ++generation
  busy.value = true
  error.value = ''
  try {
    await action(seq)
  } catch (e) {
    if (seq === generation) error.value = e instanceof Error ? e.message : t('common.error')
  } finally {
    if (seq === generation) busy.value = false
  }
}

// Read plan enablement alongside the selected model's observations. Publish
// after both requests complete for the current provider and model.
async function load() {
  const id = Number(providerId.value)
  if (!Number.isSafeInteger(id) || id <= 0 || !model.value) return
  await run(async (seq) => {
    const [data, list] = await Promise.all([
      scheduledTestsAPI.modelQuality(id, model.value),
      scheduledTestsAPI.listByProvider(id),
    ])
    if (seq !== generation) return
    report.value = data
    plans.value = list.filter(
      (p) => p.quality_enabled || data.owners.some((o) => o.plan_id === p.id),
    )
    historyPlan.value = 0
  })
}

// Pausing affects future runs. Refresh the stored state and expiry independently.
async function toggle(plan: ScheduledTestPlan) {
  await run(async (seq) => {
    await scheduledTestsAPI.update(plan.id, { enabled: !plan.enabled })
    if (seq === generation) await load()
  })
}

// Return recent raw probes for the selected plan, including ungraded results.
async function history(id: number) {
  await run(async (seq) => {
    const rows = await scheduledTestsAPI.listResults(id, 20)
    if (seq !== generation) return
    results.value = rows
    historyPlan.value = id
  })
}
</script>
