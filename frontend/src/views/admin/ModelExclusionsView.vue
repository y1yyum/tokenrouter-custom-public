<template>
  <AppLayout>
    <div class="space-y-4">
      <form class="card grid gap-4 p-6 sm:grid-cols-2 lg:grid-cols-3" @submit.prevent="preview">
        <div>
          <label class="input-label" for="exclusion-provider">{{ t(page + 'provider') }}</label>
          <input
            id="exclusion-provider"
            v-model="form.provider"
            class="input"
            type="number"
            min="1"
            step="1"
            required
          />
        </div>
        <div>
          <label class="input-label" for="exclusion-group">{{ t(page + 'group') }}</label>
          <input
            id="exclusion-group"
            v-model="form.group"
            class="input"
            type="number"
            min="1"
            step="1"
            required
          />
        </div>
        <div>
          <label class="input-label" for="exclusion-source">{{ t(score + 'sourceGroup') }}</label>
          <input
            id="exclusion-source"
            v-model="form.source"
            class="input"
            type="number"
            min="1"
            step="1"
          />
        </div>
        <div>
          <label class="input-label" for="exclusion-protocol">{{ t(score + 'entry') }}</label>
          <Select
            id="exclusion-protocol"
            v-model="form.protocol"
            :aria-label="t(score + 'entry')"
            :options="schedulerPreviewEntries"
          />
        </div>
        <div>
          <label class="input-label" for="exclusion-model">{{ t(score + 'requestedModel') }}</label>
          <input id="exclusion-model" v-model="form.model" class="input" required />
        </div>
        <div class="flex items-end">
          <button class="btn btn-primary" type="submit" :disabled="busy || !valid">{{
            t(page + 'inspect')
          }}</button>
        </div>
      </form>

      <section class="card space-y-4 p-6" aria-live="polite" :aria-busy="busy">
        <p v-if="busy">{{ t('common.loading') }}</p>
        <p v-else-if="error" role="alert">{{ error }}</p>
        <p v-else-if="!response">{{ t(page + 'idle') }}</p>
        <p v-else-if="!detail">{{ t(page + 'noDetail') }}</p>
        <template v-else>
          <div>
            <p class="text-sm text-gray-600 dark:text-dark-300"
              >{{ response.provider.name }} · #{{ response.provider.id }} ·
              {{ detail.group.name }} · #{{ detail.group.id }}</p
            >
            <h2 class="mt-1 font-semibold">{{
              t(page + (detail.eligible ? 'eligible' : 'excluded'))
            }}</h2>
          </div>
          <ul v-if="detail.hard_filter_reasons?.length" class="list-disc space-y-1 pl-5 text-sm">
            <li v-for="reason in detail.hard_filter_reasons" :key="reason">{{
              reasonLabel(reason)
            }}</li>
          </ul>
          <p v-else-if="!detail.eligible">{{ t(page + 'noReasons') }}</p>
          <template v-if="check">
            <dl class="grid gap-4 text-sm sm:grid-cols-2 lg:grid-cols-3">
              <div v-for="[label, value] in routeFields" :key="label" class="min-w-0">
                <dt class="text-gray-600 dark:text-dark-300">{{ label }}</dt>
                <dd class="break-words font-medium">{{ value }}</dd>
              </div>
            </dl>
            <p v-if="check.source_group_id !== check.group_id" class="text-sm">{{
              t(score + 'fallbackAccess')
            }}</p>
            <p v-if="!check.enabled" class="text-sm">{{ t(score + 'planCheckOff') }}</p>
            <ul v-else-if="check.owners?.length" class="space-y-1 text-sm">
              <li v-for="owner in check.owners" :key="owner.plan_id">
                {{
                  t(score + 'planOwner', {
                    plan: owner.plan_id,
                    result: owner.result_id,
                    version: owner.policy_version,
                  })
                }}
                <span v-if="owner.reason"> · {{ owner.reason }}</span>
              </li>
            </ul>
          </template>
        </template>
      </section>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import { schedulerPreviewEntries } from '@/api/admin/providers'
import type { AdvancedSchedulerScoreDiagnosticResponse } from '@/api/admin/providers.types'
import Select from '@/components/common/Select.vue'
import AppLayout from '@/components/layout/AppLayout.vue'
import { extractApiErrorMessage } from '@/utils/apiError'

// ModelExclusionsView.vue lets administrators inspect one provider in a specific HTTP
// routing scenario. The scheduler owns the eligibility decision and reports the route
// and any active plan owners. Editing any input invalidates the previous observation.
const { t, te } = useI18n()
const page = 'admin.providers.modelExclusions.'
const score = 'admin.providers.advancedSchedulerScore.'
const form = reactive({
  provider: '',
  group: '',
  source: '',
  protocol: 'openai_responses',
  model: '',
})
const response = ref<AdvancedSchedulerScoreDiagnosticResponse>()
const detail = computed(() => response.value?.detail)
const check = computed(() => detail.value?.plan_model_check)
const busy = ref(false)
const error = ref('')
let generation = 0

// An absent source selects the destination itself. Only positive, exactly representable
// IDs may enter a diagnostic request because JavaScript numbers carry all three IDs.
const valid = computed(
  () =>
    [form.provider, form.group, ...(form.source === '' ? [] : [form.source])].every(
      (value) => Number.isSafeInteger(Number(value)) && Number(value) > 0,
    ) && form.model.trim() !== '',
)

// Unresolved route fields stay unknown even if the form contains a plausible upstream value.
const routeFields = computed(() => {
  const route = check.value
  if (!route) return []
  const unknown = t('common.unknown')
  return [
    [t(page + 'route'), `${route.source_group_id} → ${route.group_id}`],
    [t(page + 'upstream'), `${route.client_protocol} → ${route.upstream_protocol || unknown}`],
    [t(score + 'requestedModel'), route.requested_model],
    [t(page + 'routingModel'), route.routing_model || unknown],
    [t('admin.providers.actualModel'), route.actual_model || unknown],
    [t(page + 'checkedAt'), route.checked_at || unknown],
  ]
})

// Clearing on edit allows another scenario to run before an older network call finishes.
watch(
  form,
  () => {
    generation++
    response.value = undefined
    busy.value = false
    error.value = ''
  },
  { flush: 'sync' },
)

/** reasonLabel preserves new server reason codes until a matching translation ships. */
function reasonLabel(reason: string) {
  const key = score + 'filterReasons.' + reason
  return te(key) ? t(key) : reason
}

/** preview reads one scenario and accepts its outcome only while those inputs still own
 * the view. Both late failures and late successes must leave a newer query undisturbed. */
async function preview() {
  if (!valid.value || busy.value) return
  const current = ++generation
  response.value = undefined
  busy.value = true
  error.value = ''
  try {
    const result = await adminAPI.providers.previewAdvancedSchedulerScore(Number(form.provider), {
      group_id: Number(form.group),
      ...(form.source === '' ? {} : { source_group_id: Number(form.source) }),
      client_protocol: form.protocol,
      requested_model: form.model.trim(),
    })
    if (current === generation) response.value = result
  } catch (cause) {
    if (current === generation) error.value = extractApiErrorMessage(cause, t(page + 'failed'))
  } finally {
    if (current === generation) busy.value = false
  }
}
</script>
