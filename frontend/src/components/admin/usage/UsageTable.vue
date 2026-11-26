<template>
  <div :class="flat ? '' : 'card overflow-hidden'">
    <div
      v-if="showIpGeoToolbar"
      class="flex items-center justify-end gap-2 border-b border-gray-200 px-4 py-2 dark:border-dark-700"
    >
      <span v-if="pendingIpCount > 0" class="text-xs text-gray-500 dark:text-gray-400">
        {{ t('usage.ipGeo.pending', { count: pendingIpCount }) }}
      </span>
      <button
        type="button"
        class="inline-flex items-center gap-1 rounded-compact px-2 py-1 text-xs font-medium text-primary-600 transition-colors hover:bg-primary-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-primary-400 dark:hover:bg-primary-900/30"
        :disabled="ipGeoBatchLoading || pendingIpCount === 0"
        @click="handleBatchFetchIpGeo"
      >
        {{ ipGeoBatchLoading ? t('usage.ipGeo.batchFetching') : t('usage.ipGeo.batchFetch') }}
      </button>
    </div>
    <div class="overflow-auto">
      <DataTable
        :column-order-storage-key="columnOrderStorageKey"
        :initial-column-order="initialColumnOrder"
        :density="isCompact ? 'compact' : 'default'"
        :columns="columns"
        :data="data"
        :loading="loading"
        :server-side-sort="serverSideSort"
        :default-sort-key="defaultSortKey"
        :default-sort-order="defaultSortOrder"
        @sort="(key, order) => $emit('sort', key, order)"
      >
        <template #cell-time="{ row }">
          <time
            class="block min-w-0 text-xs tabular-nums text-gray-600 dark:text-gray-400"
            :datetime="compactTimes.get(row.created_at)?.full ? row.created_at : undefined"
            :title="compactTimes.get(row.created_at)?.full"
          >
            <span class="block truncate">{{ compactTimes.get(row.created_at)?.date || '-' }}</span>
            <span v-if="compactTimes.get(row.created_at)?.time" class="block truncate">
              {{ compactTimes.get(row.created_at)?.time }}
            </span>
          </time>
        </template>

        <template #cell-caller="{ row }">
          <div class="w-full min-w-0 space-y-0.5 text-xs">
            <div
              v-if="compactRole !== 'personal'"
              class="flex min-w-0 items-baseline gap-1"
              data-test="usage-user-cell"
            >
              <span
                v-if="showAdminDetails && row.system_usage"
                class="truncate"
                :title="systemRequestLabel(row)"
              >
                {{ systemRequestLabel(row) }}
              </span>
              <template v-else>
                <button
                  v-if="
                    showAdminDetails && userClickable && row.user_id && usageUserDisplayName(row)
                  "
                  type="button"
                  class="min-w-0 truncate font-medium text-primary-600 underline decoration-dashed underline-offset-2 hover:text-primary-700 dark:text-primary-400 dark:hover:text-primary-300"
                  :aria-label="
                    t('admin.usage.clickToViewBalance') + ' · ' + usageUserDisplayName(row)
                  "
                  :title="usageUserTitle(row)"
                  data-test="usage-user-email"
                  @click="emit('userClick', row.user_id, row.user?.email)"
                >
                  {{ usageUserDisplayName(row) }}
                </button>
                <span
                  v-else
                  class="min-w-0 truncate font-medium text-gray-900 dark:text-white"
                  :title="usageUserTitle(row) || undefined"
                  data-test="usage-user-email"
                >
                  {{ usageUserDisplayName(row) || '-' }}
                </span>
                <span v-if="row.user_id" class="shrink-0 text-gray-500 dark:text-gray-400">
                  #{{ row.user_id }}
                </span>
                <span
                  v-if="row.user?.deleted_at"
                  class="min-w-0 truncate rounded-compact bg-rose-100 px-1 text-rose-600 dark:bg-rose-500/20 dark:text-rose-400"
                  :title="t('admin.usage.userDeletedBadge')"
                >
                  {{ t('admin.usage.userDeletedBadge') }}
                </span>
              </template>
            </div>
            <div
              class="flex min-w-0 items-center gap-1 text-gray-600 dark:text-gray-400"
              :title="row.api_key?.name || undefined"
            >
              <Icon name="key" size="xs" class="shrink-0" />
              <span class="truncate">{{ row.api_key?.name || '-' }}</span>
            </div>
            <GroupBadge
              v-if="row.group"
              class="max-w-full"
              :name="row.group.display_name || row.group.name"
              :display-brand="row.group.display_brand"
              :show-rate="false"
              :title="row.group.display_name || row.group.name"
            />
            <span v-else class="block text-gray-400 dark:text-gray-500">-</span>
          </div>
        </template>

        <template #cell-route="{ row }">
          <div class="w-full min-w-0 space-y-0.5 text-xs text-gray-600 dark:text-gray-400">
            <div
              v-if="showAdminDetails"
              class="truncate font-medium text-gray-900 dark:text-white"
              :title="row.provider?.name || undefined"
            >
              {{ row.provider?.name || '-' }}
            </div>
            <div class="truncate" :title="row.inbound_endpoint?.trim() || undefined">
              <span v-if="showAdminDetails" class="mr-1">{{ t('usage.inbound') }}</span>
              {{ row.inbound_endpoint?.trim() || '-' }}
            </div>
            <div
              v-if="showAdminDetails && showUpstreamEndpoint"
              class="truncate"
              :title="row.upstream_endpoint?.trim() || undefined"
            >
              <span class="mr-1">{{ t('usage.upstream') }}</span>
              {{ row.upstream_endpoint?.trim() || '-' }}
            </div>
          </div>
        </template>

        <template #cell-model_spec="{ row }">
          <!-- A dedicated control keeps the complete mapping reachable when long names consume the line. -->
          <div
            class="grid w-full min-w-0 grid-cols-[minmax(0,1fr)_auto] items-baseline gap-x-1 text-xs"
          >
            <div
              class="col-span-2 flex min-w-0 items-center gap-1 font-medium text-gray-900 dark:text-white"
            >
              <ProviderIcon v-if="row.platform" :brand="row.platform" size="12px" hide-fallback />
              <span class="self-baseline truncate" :title="row.model">{{ row.model || '-' }}</span>
            </div>
            <span
              v-if="modelMappings.get(row)?.length"
              class="truncate text-gray-600 dark:text-gray-400"
              :title="forwardedModel(row)"
            >
              ↳ {{ forwardedModel(row) }}
            </span>
            <span
              v-if="
                showAdminDetails &&
                row.upstream_model_mismatch === true &&
                row.upstream_response_model
              "
              class="col-span-2 truncate text-orange-600 dark:text-orange-400"
              :title="responseModelTitle(row)"
              data-testid="upstream-response-model"
            >
              {{ t('usage.compactTable.responseModel') }} · {{ row.upstream_response_model }}
            </span>
            <div
              class="flex min-w-0 items-baseline gap-1"
              :class="{ 'col-span-2': modelMappings.get(row)?.length }"
              :title="modelMetadata(row)"
            >
              <span
                class="shrink-0 rounded-compact px-1 font-medium"
                :class="getRequestTypeBadgeClass(row)"
              >
                {{ getRequestTypeLabel(row) }}
              </span>
              <span
                class="shrink-0 rounded-compact px-1 font-medium"
                :class="getBillingModeBadgeClass(row.billing_mode || getDisplayBillingMode(row))"
              >
                {{ getBillingModeLabel(row.billing_mode || getDisplayBillingMode(row), t) }}
              </span>
              <span
                v-if="row.reasoning_effort || row.requested_reasoning_effort"
                class="min-w-0 truncate"
              >
                {{ reasoningEffortSummary(row) }}
              </span>
              <span
                v-if="row.native_compaction_v2"
                class="min-w-0 truncate rounded-compact bg-fuchsia-100 px-1 text-fuchsia-800 dark:bg-fuchsia-900 dark:text-fuchsia-200"
              >
                {{ t('usage.nativeCompactionV2') }}
              </span>
            </div>
            <HelpTooltip
              class="col-start-2 self-center"
              :class="{ 'row-start-2': modelMappings.get(row)?.length }"
              trigger="both"
              interactive
              :accessible-label="t('usage.compactTable.modelDetails')"
            >
              <template #trigger="{ open, panelId }">
                <button
                  type="button"
                  class="flex h-4 w-4 shrink-0 items-center justify-center rounded-full text-gray-400 hover:text-primary-500 focus-visible:ring-2 focus-visible:ring-primary-500/40 dark:text-gray-500"
                  :aria-label="
                    t(
                      modelMappings.get(row)?.length
                        ? 'usage.compactTable.modelMapping'
                        : 'usage.compactTable.modelDetails',
                    )
                  "
                  :aria-expanded="open"
                  :aria-controls="panelId"
                  :title="
                    t(
                      modelMappings.get(row)?.length
                        ? 'usage.compactTable.modelMapping'
                        : 'usage.compactTable.modelDetails',
                    )
                  "
                >
                  <Icon name="infoCircle" size="xs" />
                </button>
              </template>
              <div class="space-y-1 break-words pr-4" data-testid="usage-model-details">
                <div class="font-semibold">{{ row.model }}</div>
                <template v-if="modelMappings.get(row)?.length">
                  <div>{{ t('usage.compactTable.modelMapping') }}</div>
                  <ol class="list-decimal pl-4">
                    <li v-for="(model, index) in modelMappings.get(row)" :key="index">{{
                      model
                    }}</li>
                  </ol>
                  <div v-if="row.upstream_model && row.upstream_model !== lastMappedModel(row)">
                    {{ t('usage.upstream') }} · {{ row.upstream_model }}
                  </div>
                </template>
                <div
                  v-if="
                    showAdminDetails &&
                    row.upstream_model_mismatch === true &&
                    row.upstream_response_model
                  "
                >
                  {{ responseModelTitle(row) }}
                </div>
                <div>{{ modelMetadata(row) }}</div>
              </div>
            </HelpTooltip>
          </div>
        </template>

        <template #cell-request_trace="{ row }">
          <div class="w-full min-w-0 space-y-0.5 text-xs">
            <RequestIdLink compact :value="row.request_id" :link="showAdminDetails" />
            <div
              v-if="showAdminDetails && row.upstream_request_id"
              class="flex min-w-0 items-center gap-1"
            >
              <span class="shrink-0 text-gray-500 dark:text-gray-400">{{
                t('usage.upstream')
              }}</span>
              <span
                class="min-w-0 truncate font-mono text-gray-600 dark:text-gray-400"
                :title="row.upstream_request_id"
              >
                {{ truncateMiddle(row.upstream_request_id) }}
              </span>
              <button
                type="button"
                class="shrink-0 rounded-compact p-0.5 text-gray-400 hover:bg-gray-100 hover:text-gray-600 dark:hover:bg-dark-700 dark:hover:text-gray-300"
                :class="{ 'text-green-500': copiedRequestId === row.upstream_request_id }"
                :aria-label="t('usage.compactTable.copyUpstreamId')"
                :title="
                  copiedRequestId === row.upstream_request_id
                    ? t('keys.copied')
                    : t('usage.compactTable.copyUpstreamId')
                "
                @click="copyUpstreamRequestId(row.upstream_request_id)"
              >
                <Icon
                  :name="copiedRequestId === row.upstream_request_id ? 'check' : 'copy'"
                  size="xs"
                />
              </button>
            </div>
          </div>
        </template>

        <template #cell-client_network="{ row }">
          <div class="w-full min-w-0 space-y-0.5 text-xs">
            <IpGeoCell compact :ip="row.ip_address || ''" />
            <div
              v-if="row.user_agent"
              class="flex min-w-0 items-center gap-1 text-gray-600 dark:text-gray-400"
            >
              <span class="min-w-0 truncate" :title="row.user_agent">{{
                formatCompactUserAgent(row.user_agent)
              }}</span>
              <button
                type="button"
                class="shrink-0 rounded-compact p-0.5 text-gray-400 hover:bg-gray-100 hover:text-gray-600 dark:hover:bg-dark-700 dark:hover:text-gray-300"
                :class="{ 'text-green-500': copiedUserAgent === row.id }"
                :aria-label="t('usage.compactTable.copyUserAgent')"
                :title="
                  copiedUserAgent === row.id
                    ? t('keys.copied')
                    : t('usage.compactTable.copyUserAgent')
                "
                @click="copyUserAgent(row)"
              >
                <Icon :name="copiedUserAgent === row.id ? 'check' : 'copy'" size="xs" />
              </button>
            </div>
          </div>
        </template>

        <template #cell-user="{ row }">
          <!-- Mobile cards align member values with their other fields; desktop rows retain a fixed width for scanning. -->
          <div
            class="flex items-center text-sm"
            :class="compactUserColumn ? 'min-w-0 justify-end gap-1 md:w-32 md:justify-start' : ''"
            data-test="usage-user-cell"
          >
            <span v-if="row.system_usage">
              {{ t(`admin.usage.requestSources.${row.request_source}`) }}
            </span>
            <button
              v-else-if="usageUserDisplayName(row) && userClickable && row.user_id"
              class="font-medium text-primary-600 underline decoration-dashed underline-offset-2 transition-colors hover:text-primary-700 dark:text-primary-400 dark:hover:text-primary-300"
              :class="compactUserColumn ? 'min-w-0 truncate' : ''"
              @click="$emit('userClick', row.user_id, row.user?.email)"
              :title="compactUserColumn ? usageUserTitle(row) : t('admin.usage.clickToViewBalance')"
              data-test="usage-user-email"
            >
              {{ usageUserDisplayName(row) }}
            </button>
            <span
              v-else-if="usageUserDisplayName(row)"
              class="font-medium text-gray-900 dark:text-white"
              :class="compactUserColumn ? 'min-w-0 truncate' : ''"
              :title="compactUserColumn ? usageUserTitle(row) : undefined"
              data-test="usage-user-email"
            >
              {{ usageUserDisplayName(row) }}
            </span>
            <span v-else class="font-medium text-gray-900 dark:text-white">-</span>
            <span
              v-if="row.user?.deleted_at"
              class="ml-1 inline-flex shrink-0 items-center rounded-compact px-1 py-px text-xs font-medium leading-tight bg-rose-100 text-rose-600 ring-1 ring-inset ring-rose-200 dark:bg-rose-500/20 dark:text-rose-400 dark:ring-rose-500/30"
            >
              {{ t('admin.usage.userDeletedBadge') }}
            </span>
            <span v-if="row.user_id" class="ml-1 shrink-0 text-gray-500 dark:text-gray-400"
              >#{{ row.user_id }}</span
            >
          </div>
        </template>

        <template #cell-api_key="{ row }">
          <span class="text-sm text-gray-900 dark:text-white">{{ row.api_key?.name || '-' }}</span>
        </template>

        <template #cell-provider="{ row }">
          <span class="text-sm text-gray-900 dark:text-white">{{ row.provider?.name || '-' }}</span>
        </template>

        <template #cell-model="{ row }">
          <div
            v-if="row.model_mapping_chain && row.model_mapping_chain.includes('→')"
            class="space-y-0.5 text-xs"
          >
            <div
              v-for="(step, i) in (row.model_mapping_chain as string).split('→')"
              :key="i"
              class="break-all"
              :class="
                i === 0
                  ? 'font-medium text-gray-900 dark:text-white'
                  : 'text-gray-500 dark:text-gray-400'
              "
              :style="i > 0 ? `padding-left: ${i * 0.75}rem` : ''"
            >
              <span v-if="i > 0" class="mr-0.5">↳</span>{{ step }}
            </div>
          </div>
          <div
            v-else-if="row.upstream_model && row.upstream_model !== row.model"
            class="space-y-0.5 text-xs"
          >
            <div class="break-all font-medium text-gray-900 dark:text-white">
              {{ row.model }}
            </div>
            <div class="break-all text-gray-500 dark:text-gray-400">
              <span class="mr-0.5">↳</span>{{ row.upstream_model }}
            </div>
          </div>
          <span v-else class="font-medium text-gray-900 dark:text-white">{{ row.model }}</span>
          <!-- The returned model is a separate observation displayed when it differs from the final forwarded model. -->
          <div
            v-if="row.upstream_model_mismatch === true && row.upstream_response_model"
            data-testid="upstream-response-model"
            class="break-all pl-3 text-xs text-orange-600 dark:text-orange-400"
            :title="`${t('usage.upstreamResponseModel')}: ${row.upstream_model || row.model} → ${row.upstream_response_model}`"
          >
            <span class="mr-1">↳</span>{{ row.upstream_response_model }}
          </div>
        </template>

        <template #cell-reasoning_effort="{ row }">
          <div
            v-if="hasReasoningEffortMapping(row)"
            data-testid="reasoning-effort-cell"
            class="space-y-0.5 text-xs"
          >
            <div class="font-medium text-gray-900 dark:text-white">
              {{ formatReasoningEffort(row.requested_reasoning_effort) }}
            </div>
            <div class="text-gray-500 dark:text-gray-400">
              <span class="mr-0.5">↳</span>{{ formatReasoningEffort(row.reasoning_effort) }}
            </div>
          </div>
          <span
            v-else
            data-testid="reasoning-effort-cell"
            class="text-sm text-gray-900 dark:text-white"
          >
            {{ formatReasoningEffort(row.requested_reasoning_effort || row.reasoning_effort) }}
          </span>
        </template>

        <template #cell-endpoint="{ row }">
          <div class="max-w-[320px] space-y-1 text-xs">
            <div class="break-all text-gray-700 dark:text-gray-300">
              <span class="font-medium text-gray-500 dark:text-gray-400"
                >{{ t('usage.inbound') }}:</span
              >
              <span class="ml-1">{{ row.inbound_endpoint?.trim() || '-' }}</span>
            </div>
            <div v-if="showUpstreamEndpoint" class="break-all text-gray-700 dark:text-gray-300">
              <span class="font-medium text-gray-500 dark:text-gray-400"
                >{{ t('usage.upstream') }}:</span
              >
              <span class="ml-1">{{ row.upstream_endpoint?.trim() || '-' }}</span>
            </div>
          </div>
        </template>

        <template #cell-group="{ row }">
          <!-- Both usage views share the API-key group badge; the cost column owns rate display. -->
          <GroupBadge
            v-if="row.group"
            :name="row.group.display_name || row.group.name"
            :display-brand="row.group.display_brand"
            :show-rate="false"
          />
          <span v-else class="text-sm text-gray-400 dark:text-gray-500">-</span>
        </template>

        <template #cell-stream="{ row }">
          <div class="flex flex-wrap items-center gap-1">
            <span
              class="inline-flex items-center rounded-compact px-2 py-0.5 text-xs font-medium"
              :class="getRequestTypeBadgeClass(row)"
            >
              {{ getRequestTypeLabel(row) }}
            </span>
            <span
              v-if="row.native_compaction_v2"
              class="inline-flex items-center rounded-compact bg-fuchsia-100 px-2 py-0.5 text-xs font-medium text-fuchsia-800 dark:bg-fuchsia-900 dark:text-fuchsia-200"
            >
              {{ t('usage.nativeCompactionV2') }}
            </span>
          </div>
        </template>

        <template #cell-billing_mode="{ row }">
          <span
            class="inline-flex items-center rounded-compact px-2 py-0.5 text-xs font-medium"
            :class="getBillingModeBadgeClass(getDisplayBillingMode(row))"
          >
            {{ getBillingModeLabel(getDisplayBillingMode(row), t) }}
          </span>
        </template>

        <template #cell-tokens="{ row }">
          <!-- Image usage displays image counts and billing dimensions. -->

          <div v-if="isCompact && isImageUsage(row)" class="w-full min-w-0 text-xs">
            <div class="flex items-baseline gap-1 text-gray-900 dark:text-white">
              <Icon name="modalityImage" size="xs" class="shrink-0 text-indigo-500" />
              <span class="truncate font-medium" :title="imageUsageCount(row)">{{
                imageUsageCount(row)
              }}</span>
            </div>
            <div
              class="truncate text-gray-500 dark:text-gray-400"
              :title="formatImageBillingSize(row, t)"
            >
              {{ formatImageBillingSize(row, t) }}
            </div>
          </div>
          <div v-else-if="isImageUsage(row)" class="flex items-center gap-1.5">
            <Icon name="modalityImage" size="sm" class="h-4 w-4 text-indigo-500" />
            <!-- The image count provides the mobile card label's text baseline. -->
            <span class="self-baseline font-medium text-gray-900 dark:text-white"
              >{{ row.image_count }}{{ t('usage.imageUnit')
              }}<span v-if="row.image_frame_count">
                · {{ row.image_frame_count }}{{ t('usage.imageFrameUnit') }}</span
              ></span
            >
            <span class="text-gray-400">({{ formatImageBillingSize(row, t) }})</span>
          </div>
          <!-- Token usage -->
          <div
            v-else
            class="flex gap-1.5"
            :class="isCompact ? 'w-full min-w-0 items-end' : 'items-center'"
          >
            <div v-if="isCompact" class="min-w-0 grow space-y-0.5 text-xs">
              <dl class="grid min-w-0 grid-cols-2 gap-x-2 tabular-nums">
                <div
                  v-for="item in compactTokenCounts(row)"
                  :key="item.label"
                  class="flex min-w-0 items-baseline justify-between gap-0.5"
                >
                  <dt class="shrink-0 text-gray-500 dark:text-gray-400">{{ t(item.label) }}</dt>
                  <dd
                    class="truncate font-medium text-gray-900 dark:text-white"
                    :title="item.value"
                    >{{ item.value }}</dd
                  >
                </div>
              </dl>
              <div
                v-if="row.cache_creation_tokens > 0"
                class="truncate text-amber-600 dark:text-amber-400"
                :title="cacheBucketSummary(row)"
              >
                {{ cacheBucketSummary(row) }}
              </div>
              <div
                v-if="row.cache_ttl_overridden || (showAdminDetails && row.cache_write_anomaly)"
                class="flex min-w-0 items-baseline gap-1"
              >
                <span
                  v-if="row.cache_ttl_overridden"
                  class="shrink-0 rounded-compact bg-rose-100 px-1 text-rose-600 dark:bg-rose-500/20 dark:text-rose-400"
                  :title="t('usage.cacheTtlOverriddenHint')"
                  >R</span
                >
                <span
                  v-if="showAdminDetails && row.cache_write_anomaly"
                  class="truncate rounded-compact bg-amber-100 px-1 text-amber-700 dark:bg-amber-500/20 dark:text-amber-400"
                  :title="formatCacheWriteAnomalyText(row.cache_write_anomaly)"
                  data-testid="cache-write-anomaly-badge"
                >
                  {{ t('admin.usage.cacheWriteAnomalyBadge') }}
                </span>
              </div>
            </div>
            <div v-else class="space-y-1 text-sm">
              <div class="flex items-center gap-2">
                <div class="inline-flex items-center gap-1">
                  <Icon name="arrowDown" size="sm" class="h-3.5 w-3.5 text-emerald-500" />
                  <!-- The input token count provides the mobile card label's text baseline. -->
                  <span class="self-baseline font-medium text-gray-900 dark:text-white">{{
                    row.input_tokens?.toLocaleString() || 0
                  }}</span>
                </div>
                <div class="inline-flex items-center gap-1">
                  <Icon name="arrowUp" size="sm" class="h-3.5 w-3.5 text-violet-500" />
                  <span class="font-medium text-gray-900 dark:text-white">{{
                    row.output_tokens?.toLocaleString() || 0
                  }}</span>
                </div>
              </div>
              <div v-if="hasImageInputTokens(row)" class="flex items-center gap-2">
                <div class="inline-flex items-center gap-1">
                  <Icon name="arrowDown" size="sm" class="h-3.5 w-3.5 text-fuchsia-500" />
                  <span class="font-medium text-fuchsia-600 dark:text-fuchsia-400">{{
                    uncachedImageInputTokens(row).toLocaleString()
                  }}</span>
                </div>
              </div>
              <div v-if="hasImageOutputTokens(row)" class="flex items-center gap-2">
                <div class="inline-flex items-center gap-1">
                  <Icon name="sparkles" size="sm" class="h-3.5 w-3.5 text-pink-500" />
                  <span class="font-medium text-pink-600 dark:text-pink-400">{{
                    row.image_output_tokens.toLocaleString()
                  }}</span>
                </div>
              </div>
              <div
                v-if="row.cache_read_tokens > 0 || row.cache_creation_tokens > 0"
                class="flex flex-wrap items-center gap-x-2 gap-y-1"
              >
                <div v-if="row.cache_read_tokens > 0" class="inline-flex items-center gap-1">
                  <Icon name="archive" size="xs" class="h-3.5 w-3.5 text-sky-500" />
                  <span class="font-medium text-sky-600 dark:text-sky-400">{{
                    formatCacheTokens(row.cache_read_tokens)
                  }}</span>
                </div>
                <div
                  v-if="row.cache_creation_tokens > 0"
                  class="inline-flex flex-wrap items-center gap-1"
                >
                  <Icon name="edit" size="xs" class="h-3.5 w-3.5 text-amber-500" />
                  <span
                    v-for="bucket in cacheWriteBuckets(row)"
                    :key="bucket.ttl"
                    data-testid="cache-write-bucket"
                    class="inline-flex items-center gap-1"
                  >
                    <span class="font-medium text-amber-600 dark:text-amber-400">{{
                      formatCacheTokens(bucket.tokens)
                    }}</span>
                    <span
                      :data-cache-write-ttl="bucket.ttl"
                      class="inline-flex items-center rounded-compact px-1 py-px text-xs font-medium leading-tight bg-orange-100 text-orange-600 ring-1 ring-inset ring-orange-200 dark:bg-orange-500/20 dark:text-orange-400 dark:ring-orange-500/30"
                      >{{ bucket.ttl }}</span
                    >
                  </span>
                  <span
                    v-if="row.cache_write_anomaly"
                    data-testid="cache-write-anomaly-badge"
                    :title="formatCacheWriteAnomalyText(row.cache_write_anomaly)"
                    class="inline-flex items-center rounded-compact px-1 py-px text-xs font-medium leading-tight bg-amber-100 text-amber-700 ring-1 ring-inset ring-amber-200 dark:bg-amber-500/20 dark:text-amber-400 dark:ring-amber-500/30 cursor-help"
                    >{{ t('admin.usage.cacheWriteAnomalyBadge') }}</span
                  >
                  <span
                    v-if="row.cache_ttl_overridden"
                    :title="t('usage.cacheTtlOverriddenHint')"
                    class="inline-flex items-center rounded-compact px-1 py-px text-xs font-medium leading-tight bg-rose-100 text-rose-600 ring-1 ring-inset ring-rose-200 dark:bg-rose-500/20 dark:text-rose-400 dark:ring-rose-500/30 cursor-help"
                    >R</span
                  >
                </div>
              </div>
            </div>
            <!-- Token Detail Tooltip -->
            <button
              type="button"
              class="group relative shrink-0"
              :aria-label="t('usage.tokenDetails')"
              :aria-describedby="tokenTooltipData === row ? tokenTooltipId : undefined"
              @focus="showTokenTooltip($event, row)"
              @click="showTokenTooltip($event, row)"
              @blur="hideTokenTooltip"
              @keydown.esc="hideTokenTooltip"
              @mouseenter="showTokenTooltip($event, row)"
              @mouseleave="hideTokenTooltip"
            >
              <span
                class="flex h-4 w-4 cursor-help items-center justify-center rounded-full bg-gray-100 transition-colors group-hover:bg-primary-100 dark:bg-gray-700 dark:group-hover:bg-primary-500/8"
              >
                <Icon
                  name="infoCircle"
                  size="xs"
                  class="text-gray-400 group-hover:text-primary-500 dark:text-gray-500 dark:group-hover:text-primary-500"
                />
              </span>
            </button>
          </div>
        </template>

        <template #cell-cost="{ row }">
          <div :class="isCompact ? 'w-full min-w-0 text-xs' : 'text-sm'">
            <div class="flex min-w-0 items-center gap-1">
              <span v-if="isSystemRow(row) && !showProviderBilling" class="text-gray-500">-</span>
              <span
                v-else-if="isSystemRow(row) && row.system_usage?.cost_state === 'unknown'"
                class="text-gray-500 dark:text-gray-400"
              >
                {{ t('usage.costUnknown') }}
              </span>
              <!-- System costs and customer charges share amount styling while retaining their recorded USD or configured balance unit. -->
              <span
                v-else
                class="min-w-0 truncate font-medium text-green-600 dark:text-green-400"
                :title="
                  isSystemRow(row)
                    ? t('usage.providerCost') + ' · ' + formatDetailedUsdAmount(providerBilled(row))
                    : formatDetailedBalance(row.actual_cost)
                "
              >
                {{
                  isSystemRow(row)
                    ? formatDetailedUsdAmount(providerBilled(row))
                    : formatDetailedBalance(row.actual_cost)
                }}
              </span>
              <!-- L identifies applied long-context billing; the recorded multiplier determines its charge rather than a fixed doubling. -->
              <span
                v-if="row.long_context_billing_applied"
                data-testid="long-context-billing-marker"
                :title="t('usage.compactTable.longContextBilling')"
                class="inline-flex items-center rounded-compact px-1 py-px text-xs font-semibold leading-tight bg-amber-100 text-amber-700 ring-1 ring-inset ring-amber-200 dark:bg-amber-500/20 dark:text-amber-300 dark:ring-amber-500/30"
                >L</span
              >
              <!-- Cost details -->
              <button
                type="button"
                class="group relative shrink-0"
                :aria-label="t('usage.costDetails')"
                :aria-describedby="tooltipData === row ? costTooltipId : undefined"
                @focus="showTooltip($event, row)"
                @click="showTooltip($event, row)"
                @blur="hideTooltip"
                @keydown.esc="hideTooltip"
                @mouseenter="showTooltip($event, row)"
                @mouseleave="hideTooltip"
              >
                <span
                  class="flex h-4 w-4 cursor-help items-center justify-center rounded-full bg-gray-100 transition-colors group-hover:bg-primary-100 dark:bg-gray-700 dark:group-hover:bg-primary-500/8"
                >
                  <Icon
                    name="infoCircle"
                    size="xs"
                    class="text-gray-400 group-hover:text-primary-500 dark:text-gray-500 dark:group-hover:text-primary-500"
                  />
                </span>
              </button>
            </div>
            <div
              v-if="
                isCompact &&
                !isSystemRow(row) &&
                showStandardCost &&
                row.total_cost != null &&
                row.total_cost !== row.actual_cost
              "
              class="truncate text-gray-500 line-through dark:text-gray-400"
              :title="t('usage.original') + ' · ' + formatDetailedUsdAmount(row.total_cost)"
              data-testid="usage-original-cost"
            >
              {{ formatDetailedUsdAmount(row.total_cost) }}
            </div>
            <div
              v-if="
                isSystemRow(row) &&
                showProviderBilling &&
                row.system_usage?.cost_state === 'partial'
              "
              class="truncate text-xs text-gray-500 dark:text-gray-400"
            >
              {{ t('usage.costPartial') }}
            </div>
            <div
              v-else-if="
                !isSystemRow(row) && showProviderBilling && row.provider_rate_multiplier != null
              "
              class="truncate text-xs text-orange-500 dark:text-orange-400"
              :title="
                t('usage.providerBilled') + ' · ' + formatDetailedUsdAmount(providerBilled(row))
              "
            >
              {{ isCompact ? t('usage.compactTable.providerCost') : 'A' }}
              {{ formatDetailedUsdAmount(providerBilled(row)) }}
            </div>
            <div
              v-if="isCompact && !isSystemRow(row) && billingFundingLabel(row)"
              class="truncate text-gray-600 dark:text-gray-400"
              data-testid="usage-billing-funding"
            >
              {{ billingFundingLabel(row) }}
            </div>
          </div>
        </template>

        <!-- Latency combines first-token and total-duration severity: the edge colors identify each measure and the middle segment blends them. -->
        <template #[latencyCell]="{ row }">
          <div class="flex items-stretch gap-2" :class="{ 'w-full min-w-0': isCompact }">
            <span
              class="w-1 shrink-0 rounded-full"
              :class="
                row.ttft_ms != null
                  ? [
                      'bg-gradient-to-b from-40% to-60%',
                      LATENCY_BAR_FROM_CLASSES[firstTokenSeverity(row.ttft_ms)],
                      LATENCY_BAR_TO_CLASSES[durationSeverity(row.duration_ms ?? 0)],
                    ]
                  : LATENCY_BAR_CLASSES[durationSeverity(row.duration_ms ?? 0)]
              "
              aria-hidden="true"
            ></span>
            <!-- The text grid provides the mobile card label's first-row baseline. -->
            <div
              class="grid items-baseline gap-x-2 gap-y-0.5 self-baseline text-xs"
              :class="
                isCompact
                  ? 'min-w-0 grid-cols-[minmax(0,1fr)_auto]'
                  : 'grid-cols-[max-content_max-content]'
              "
            >
              <span
                class="text-gray-500 dark:text-gray-400"
                :class="{ truncate: isCompact }"
                :title="
                  t(
                    row.ttft_mode === 'first_event'
                      ? 'usage.latencyFirstEvent'
                      : 'usage.latencyFirstToken',
                  )
                "
                >{{
                  t(
                    row.ttft_mode === 'first_event'
                      ? 'usage.latencyFirstEvent'
                      : 'usage.latencyFirstToken',
                  )
                }}</span
              >
              <span
                v-if="row.ttft_ms != null"
                class="font-medium tabular-nums"
                :class="LATENCY_TEXT_CLASSES[firstTokenSeverity(row.ttft_ms)]"
                >{{ formatDuration(row.ttft_ms) }}</span
              >
              <span v-else class="text-gray-400 dark:text-gray-500">-</span>
              <span class="text-gray-400 dark:text-gray-500">{{ t('usage.latencyDuration') }}</span>
              <span
                class="font-medium tabular-nums"
                :class="LATENCY_TEXT_CLASSES[durationSeverity(row.duration_ms ?? 0)]"
                >{{ formatDuration(row.duration_ms) }}</span
              >
              <span
                v-if="isCompact"
                class="col-span-2 truncate tabular-nums text-gray-600 dark:text-gray-400"
                :title="t('usage.tpsHint')"
              >
                {{ formatUsageTps(row) }}
              </span>
            </div>
            <button
              v-if="showAdminDetails && row.detailed_timing"
              type="button"
              class="group relative flex h-4 w-4 shrink-0 items-center justify-center rounded-full bg-gray-100 text-gray-400 transition-colors hover:bg-primary-100 hover:text-primary-500 focus:outline-none focus-visible:ring-2 focus-visible:ring-primary-500/40 dark:bg-dark-700 dark:text-gray-500 dark:hover:bg-primary-500/8 dark:hover:text-primary-500"
              :aria-label="t('usage.detailedTiming')"
              :title="t('usage.detailedTiming')"
              :aria-describedby="timingTooltipData === row ? timingTooltipId : undefined"
              @focus="showTimingTooltip($event, row)"
              @blur="hideTimingTooltip"
              @keydown.esc="closeTimingTooltip"
              @mouseenter="showTimingTooltip($event, row)"
              @mouseleave="hideTimingTooltip"
              @click.stop="toggleTimingTooltip($event, row)"
            >
              <Icon name="infoCircle" size="xs" />
            </button>
          </div>
        </template>

        <template #cell-tps="{ row }">
          <span
            class="cursor-help whitespace-nowrap text-sm tabular-nums text-gray-600 dark:text-gray-400"
            :title="t('usage.tpsHint')"
          >
            {{ formatUsageTps(row) }}
          </span>
        </template>

        <template #cell-created_at="{ value }">
          <span class="text-sm text-gray-600 dark:text-gray-400">{{ formatDateTime(value) }}</span>
        </template>

        <template #cell-request_id="{ row }">
          <RequestIdLink :value="row.request_id" />
        </template>

        <template #cell-upstream_request_id="{ row }">
          <div v-if="row.upstream_request_id" class="flex max-w-[160px] items-center gap-1.5">
            <span
              class="truncate font-mono text-xs text-gray-500 dark:text-gray-400"
              :title="row.upstream_request_id"
            >
              {{ row.upstream_request_id }}
            </span>
            <button
              type="button"
              class="shrink-0 rounded-compact p-0.5 text-gray-400 transition-colors hover:bg-gray-100 hover:text-gray-600 dark:hover:bg-dark-700 dark:hover:text-gray-300"
              :class="
                copiedRequestId === row.upstream_request_id
                  ? 'text-green-500 hover:text-green-500'
                  : ''
              "
              :title="
                copiedRequestId === row.upstream_request_id
                  ? t('keys.copied')
                  : t('keys.copyToClipboard')
              "
              @click="copyUpstreamRequestId(row.upstream_request_id)"
            >
              <Icon
                :name="copiedRequestId === row.upstream_request_id ? 'check' : 'copy'"
                size="sm"
                class="h-3.5 w-3.5"
              />
            </button>
          </div>
          <span v-else class="text-sm text-gray-400 dark:text-gray-500">-</span>
        </template>

        <template #cell-user_agent="{ row }">
          <span
            v-if="row.user_agent"
            class="text-sm text-gray-600 dark:text-gray-400 block max-w-[320px] truncate"
            :title="row.user_agent"
            >{{ formatUserAgent(row.user_agent) }}</span
          >
          <span v-else class="text-sm text-gray-400 dark:text-gray-500">-</span>
        </template>

        <template #cell-ip_address="{ row }">
          <div v-if="row.ip_address">
            <span class="text-sm font-mono text-gray-600 dark:text-gray-400">{{
              row.ip_address
            }}</span>
            <IpGeoCell :ip="row.ip_address" />
          </div>
          <span v-else class="text-sm text-gray-400 dark:text-gray-500">-</span>
        </template>

        <template #empty><EmptyState :message="t('usage.noRecords')" /></template>
      </DataTable>
    </div>
  </div>

  <!-- Token Tooltip Portal -->
  <Teleport to="body">
    <MotionTransition name="fade">
      <div
        v-if="tokenTooltipVisible"
        :inert="!tokenTooltipVisible || undefined"
        ref="tokenTooltipRef"
        :id="tokenTooltipId"
        role="tooltip"
        data-testid="token-detail-tooltip"
        class="pointer-events-none fixed z-teleport-tooltip"
        :class="{ invisible: !tokenTooltipReady }"
        :style="{
          left: tokenTooltipPosition.x + 'px',
          top: tokenTooltipPosition.y + 'px',
        }"
      >
        <div
          class="w-max max-w-[calc(100vw-1.5rem)] break-words whitespace-normal tooltip-panel rounded-control px-3 py-2.5 text-xs shadow-xl md:whitespace-nowrap"
        >
          <div class="space-y-1.5">
            <div>
              <div class="text-xs font-semibold text-gray-300 mb-1">
                {{ t('usage.tokenDetails') }}
              </div>
              <div
                v-if="
                  tokenTooltipData &&
                  tokenTooltipData.input_tokens > 0 &&
                  !hasImageInputTokens(tokenTooltipData)
                "
                class="flex items-center justify-between gap-4"
              >
                <span class="text-gray-400">{{ t('admin.usage.inputTokens') }}</span>
                <span class="font-medium text-white">{{
                  tokenTooltipData.input_tokens.toLocaleString()
                }}</span>
              </div>
              <div
                v-if="
                  tokenTooltipData &&
                  hasImageInputTokens(tokenTooltipData) &&
                  textInputTokens(tokenTooltipData) > 0
                "
                class="flex items-center justify-between gap-4"
              >
                <span class="text-gray-400">{{ t('admin.usage.inputTokens') }}</span>
                <span class="font-medium text-white">{{
                  textInputTokens(tokenTooltipData).toLocaleString()
                }}</span>
              </div>
              <div
                v-if="tokenTooltipData && hasImageInputTokens(tokenTooltipData)"
                class="flex items-center justify-between gap-4"
              >
                <span class="text-gray-400">{{ t('usage.imageInputTokens') }}</span>
                <span class="font-medium text-fuchsia-300">{{
                  uncachedImageInputTokens(tokenTooltipData).toLocaleString()
                }}</span>
              </div>
              <div
                v-if="
                  tokenTooltipData &&
                  tokenTooltipData.output_tokens > 0 &&
                  !hasImageOutputTokens(tokenTooltipData)
                "
                class="flex items-center justify-between gap-4"
              >
                <span class="text-gray-400">{{ t('admin.usage.outputTokens') }}</span>
                <span class="font-medium text-white">{{
                  tokenTooltipData.output_tokens.toLocaleString()
                }}</span>
              </div>
              <div
                v-if="
                  tokenTooltipData &&
                  hasImageOutputTokens(tokenTooltipData) &&
                  textOutputTokens(tokenTooltipData) > 0
                "
                class="flex items-center justify-between gap-4"
              >
                <span class="text-gray-400">{{ t('admin.usage.outputTokens') }}</span>
                <span class="font-medium text-white">{{
                  textOutputTokens(tokenTooltipData).toLocaleString()
                }}</span>
              </div>
              <div
                v-if="tokenTooltipData && hasImageOutputTokens(tokenTooltipData)"
                class="flex items-center justify-between gap-4"
              >
                <span class="text-gray-400">{{ t('usage.imageOutputTokens') }}</span>
                <span class="font-medium text-pink-300">{{
                  tokenTooltipData.image_output_tokens.toLocaleString()
                }}</span>
              </div>
              <div
                v-for="bucket in tokenTooltipCacheWrites"
                :key="bucket.ttl"
                class="flex items-center justify-between gap-4"
              >
                <span class="text-gray-400 flex items-center gap-1.5">
                  {{ t('admin.usage.cacheCreationTokens') }}
                  <span
                    :data-cache-write-ttl="bucket.ttl"
                    class="inline-flex items-center rounded-compact px-1 py-px text-xs font-medium leading-tight bg-orange-500/20 text-orange-400 ring-1 ring-inset ring-orange-500/30"
                    >{{ bucket.ttl }}</span
                  >
                </span>
                <span class="font-medium text-white">{{ bucket.tokens.toLocaleString() }}</span>
              </div>
              <div
                v-if="tokenTooltipData && tokenTooltipData.cache_ttl_overridden"
                class="flex items-center justify-between gap-4"
              >
                <span class="text-gray-400 flex items-center gap-1.5">
                  {{ t('usage.cacheTtlOverriddenLabel') }}
                  <span
                    class="inline-flex items-center rounded-compact px-1 py-px text-xs font-medium leading-tight bg-rose-500/20 text-rose-400 ring-1 ring-inset ring-rose-500/30"
                    >R-{{ tokenTooltipData.cache_creation_1h_tokens > 0 ? '5m' : '1H' }}</span
                  >
                </span>
                <span class="font-medium text-rose-400">{{
                  tokenTooltipData.cache_creation_1h_tokens > 0
                    ? t('usage.cacheTtlOverridden1h')
                    : t('usage.cacheTtlOverridden5m')
                }}</span>
              </div>
              <div
                v-if="tokenTooltipData && tokenTooltipData.cache_read_tokens > 0"
                class="flex items-center justify-between gap-4"
              >
                <span class="text-gray-400">{{ t('admin.usage.cacheReadTokens') }}</span>
                <span class="font-medium text-white">{{
                  tokenTooltipData.cache_read_tokens.toLocaleString()
                }}</span>
              </div>
            </div>
            <div
              v-if="showAdminDetails && tokenTooltipData?.cache_write_anomaly"
              data-testid="cache-write-anomaly-detail"
              class="space-y-1 rounded-compact bg-amber-500/10 px-2 py-1.5 ring-1 ring-inset ring-amber-500/20"
            >
              <div class="flex items-center justify-between gap-4">
                <span class="text-amber-300">{{ t('admin.usage.cacheWriteAnomalyLabel') }}</span>
                <span class="font-medium text-amber-400">{{
                  cacheWriteAnomalyKindLabel(tokenTooltipData.cache_write_anomaly.kind)
                }}</span>
              </div>
              <div class="flex items-center justify-between gap-4">
                <span class="text-gray-400">{{ t('admin.usage.cacheWriteAnomalyRawTotal') }}</span>
                <span class="font-medium tabular-nums text-amber-200">{{
                  tokenTooltipData.cache_write_anomaly.reported_total.toLocaleString()
                }}</span>
              </div>
              <div class="flex items-center justify-between gap-4">
                <span class="text-gray-400">{{
                  cacheWriteAnomalyRawSplitLabel(tokenTooltipData.cache_write_anomaly)
                }}</span>
                <span class="font-medium tabular-nums text-amber-200">{{
                  cacheWriteAnomalyRawSplitValue(tokenTooltipData.cache_write_anomaly)
                }}</span>
              </div>
            </div>
            <div
              class="flex items-center justify-between gap-6 border-t border-gray-700 pt-1.5 dark:border-dark-600"
            >
              <span class="text-gray-400">{{ t('usage.totalTokens') }}</span>
              <span class="font-semibold text-blue-400">{{
                (
                  (tokenTooltipData?.input_tokens || 0) +
                  (tokenTooltipData?.output_tokens || 0) +
                  (tokenTooltipData?.cache_creation_tokens || 0) +
                  (tokenTooltipData?.cache_read_tokens || 0)
                ).toLocaleString()
              }}</span>
            </div>
          </div>
          <div
            v-if="tokenTooltipPosition.placement === 'right'"
            class="tooltip-caret -left-1 -translate-y-1/2 border-b border-l"
            :style="{ top: tokenTooltipPosition.arrowY + 'px' }"
          ></div>
          <div
            v-else-if="tokenTooltipPosition.placement === 'left'"
            class="tooltip-caret -right-1 -translate-y-1/2 border-r border-t"
            :style="{ top: tokenTooltipPosition.arrowY + 'px' }"
          ></div>
        </div>
      </div>
    </MotionTransition>
  </Teleport>

  <!-- Timing details render in a portal; pointer hover opens inspection and click pins it for touch use. -->
  <Teleport to="body">
    <MotionTransition name="fade">
      <div
        v-if="timingTooltipVisible"
        :inert="!timingTooltipVisible || undefined"
        ref="timingTooltipRef"
        :id="timingTooltipId"
        role="tooltip"
        data-testid="timing-detail-tooltip"
        class="pointer-events-none fixed z-teleport-tooltip"
        :class="{ invisible: !timingTooltipReady }"
        :style="{
          left: timingTooltipPosition.x + 'px',
          top: timingTooltipPosition.y + 'px',
        }"
      >
        <div
          class="w-max max-w-[calc(100vw-1.5rem)] break-words whitespace-normal tooltip-panel rounded-control px-3 py-2.5 text-xs shadow-xl md:whitespace-nowrap"
        >
          <div class="text-xs font-semibold text-gray-300 mb-1.5">
            {{ t('usage.detailedTiming') }}
          </div>
          <RequestTimingDetails
            v-if="timingTooltipData"
            :timing="timingTooltipData.detailed_timing ?? {}"
            :stream="timingTooltipData.stream"
          >
            <span class="text-gray-400">{{ t('usage.timingRequestSize') }}</span>
            <span class="font-medium tabular-nums text-right">{{
              formatRequestSize(
                timingTooltipData.detailed_timing?.request_raw_bytes ??
                  timingTooltipData.detailed_timing?.request_content_length,
              )
            }}</span>
            <span class="text-gray-400">{{ t('usage.timingAttempts') }}</span>
            <span class="font-medium tabular-nums text-right">{{
              timingTooltipData.detailed_timing?.upstream_attempt_count ?? '-'
            }}</span>
          </RequestTimingDetails>
          <div
            v-if="
              timingTooltipData?.detailed_timing?.upstream_connection_reused ||
              timingTooltipData?.detailed_timing?.upstream_wrote_request_error
            "
            class="mt-1.5 flex flex-wrap gap-x-2 text-xs"
          >
            <span
              v-if="timingTooltipData.detailed_timing?.upstream_connection_reused"
              class="text-emerald-400"
              >{{ t('usage.timingReused') }}</span
            >
            <span
              v-if="timingTooltipData.detailed_timing?.upstream_wrote_request_error"
              class="text-rose-400"
              >{{ t('usage.timingWriteError') }}</span
            >
          </div>
          <div
            v-if="timingTooltipPosition.placement === 'right'"
            class="tooltip-caret -left-1 -translate-y-1/2 border-b border-l"
            :style="{ top: timingTooltipPosition.arrowY + 'px' }"
          ></div>
          <div
            v-else-if="timingTooltipPosition.placement === 'left'"
            class="tooltip-caret -right-1 -translate-y-1/2 border-r border-t"
            :style="{ top: timingTooltipPosition.arrowY + 'px' }"
          ></div>
        </div>
      </div>
    </MotionTransition>
  </Teleport>

  <!-- Cost Tooltip Portal -->
  <Teleport to="body">
    <MotionTransition name="fade">
      <div
        v-if="tooltipVisible"
        :inert="!tooltipVisible || undefined"
        ref="tooltipRef"
        :id="costTooltipId"
        role="tooltip"
        data-testid="cost-detail-tooltip"
        class="pointer-events-none fixed z-teleport-tooltip"
        :class="{ invisible: !tooltipReady }"
        :style="{
          left: tooltipPosition.x + 'px',
          top: tooltipPosition.y + 'px',
        }"
      >
        <div
          class="w-max max-w-[calc(100vw-1.5rem)] break-words whitespace-normal tooltip-panel rounded-control px-3 py-2.5 text-xs shadow-xl md:whitespace-nowrap"
        >
          <div class="space-y-1.5">
            <!-- Cost Breakdown -->
            <div
              v-if="showCostBreakdown || isImageUsage(tooltipData)"
              class="mb-2 border-b border-gray-700 pb-1.5 dark:border-dark-600"
            >
              <div class="text-xs font-semibold text-gray-300 mb-1">
                {{ t('usage.costDetails') }}
              </div>
              <div
                v-if="showCostBreakdown && tooltipData && tooltipData.input_cost > 0"
                class="flex items-center justify-between gap-4"
              >
                <span class="text-gray-400">{{ t('admin.usage.inputCost') }}</span>
                <span class="font-medium text-white">{{
                  formatDetailedUsdAmount(tooltipData.input_cost)
                }}</span>
              </div>
              <div
                v-if="showCostBreakdown && tooltipData && hasImageInputCost(tooltipData)"
                class="flex items-center justify-between gap-4"
              >
                <span class="text-gray-400">{{ t('usage.imageInputCost') }}</span>
                <span class="font-medium text-fuchsia-300">{{
                  formatDetailedUsdAmount(tooltipData.image_input_cost)
                }}</span>
              </div>
              <div
                v-if="showCostBreakdown && tooltipData && tooltipData.output_cost > 0"
                class="flex items-center justify-between gap-4"
              >
                <span class="text-gray-400">{{ t('admin.usage.outputCost') }}</span>
                <span class="font-medium text-white">{{
                  formatDetailedUsdAmount(tooltipData.output_cost)
                }}</span>
              </div>
              <div
                v-if="showCostBreakdown && tooltipData && hasImageOutputCost(tooltipData)"
                class="flex items-center justify-between gap-4"
              >
                <span class="text-gray-400">{{ t('usage.imageOutputCost') }}</span>
                <span class="font-medium text-pink-300">{{
                  formatDetailedUsdAmount(tooltipData.image_output_cost)
                }}</span>
              </div>
              <!-- Token billing displays prices per million tokens. -->
              <template
                v-if="
                  showCostBreakdown &&
                  tooltipData &&
                  !isImageUsage(tooltipData) &&
                  (!tooltipData.billing_mode || tooltipData.billing_mode === BILLING_MODE_TOKEN)
                "
              >
                <div
                  v-if="tooltipData && textInputTokens(tooltipData) > 0"
                  class="flex items-center justify-between gap-4"
                >
                  <span class="text-gray-400">{{ t('usage.inputTokenPrice') }}</span>
                  <span class="font-medium text-sky-300"
                    >{{
                      formatTokenPricePerMillion(
                        tooltipData.input_cost,
                        textInputTokens(tooltipData),
                      )
                    }}
                    {{ t('usage.perMillionTokens') }}</span
                  >
                </div>
                <div
                  v-if="tooltipData && hasImageInputTokens(tooltipData)"
                  class="flex items-center justify-between gap-4"
                >
                  <span class="text-gray-400">{{ t('usage.imageInputTokenPrice') }}</span>
                  <span class="font-medium text-fuchsia-300"
                    >{{
                      formatTokenPricePerMillion(
                        tooltipData.image_input_cost ?? 0,
                        uncachedImageInputTokens(tooltipData),
                      )
                    }}
                    {{ t('usage.perMillionTokens') }}</span
                  >
                </div>
                <div
                  v-if="
                    tooltipData && tooltipData.output_cost > 0 && textOutputTokens(tooltipData) > 0
                  "
                  class="flex items-center justify-between gap-4"
                >
                  <span class="text-gray-400">{{ t('usage.outputTokenPrice') }}</span>
                  <span class="font-medium text-violet-300"
                    >{{
                      formatTokenPricePerMillion(
                        tooltipData.output_cost,
                        textOutputTokens(tooltipData),
                      )
                    }}
                    {{ t('usage.perMillionTokens') }}</span
                  >
                </div>
                <div
                  v-if="tooltipData && hasImageOutputTokens(tooltipData)"
                  class="flex items-center justify-between gap-4"
                >
                  <span class="text-gray-400">{{ t('usage.imageOutputTokenPrice') }}</span>
                  <span class="font-medium text-pink-300"
                    >{{
                      formatTokenPricePerMillion(
                        tooltipData.image_output_cost ?? 0,
                        tooltipData.image_output_tokens,
                      )
                    }}
                    {{ t('usage.perMillionTokens') }}</span
                  >
                </div>
              </template>
              <template v-else-if="tooltipData && isImageUsage(tooltipData)">
                <div class="flex items-center justify-between gap-4">
                  <span class="text-gray-400">{{ t('usage.imageCount') }}</span>
                  <span class="font-medium text-white"
                    >{{ tooltipData.image_count }}{{ t('usage.imageUnit')
                    }}<span v-if="tooltipData.image_frame_count">
                      · {{ tooltipData.image_frame_count }}{{ t('usage.imageFrameUnit') }}</span
                    ></span
                  >
                </div>
                <div class="flex items-center justify-between gap-4">
                  <span class="text-gray-400">{{ t('usage.imageBillingSize') }}</span>
                  <span class="font-medium text-white">{{
                    formatImageBillingSize(tooltipData, t)
                  }}</span>
                </div>
                <div class="flex items-center justify-between gap-4">
                  <span class="text-gray-400">{{ t('usage.imageSizeSource') }}</span>
                  <span class="font-medium text-white">{{
                    formatImageSizeSource(tooltipData, t)
                  }}</span>
                </div>
                <div class="flex items-center justify-between gap-4">
                  <span class="text-gray-400">{{ t('usage.imageInputSize') }}</span>
                  <span class="font-medium text-white">{{
                    formatImageInputSize(tooltipData, t)
                  }}</span>
                </div>
                <div class="flex items-center justify-between gap-4">
                  <span class="text-gray-400">{{ t('usage.imageOutputSize') }}</span>
                  <span class="font-medium text-white">{{
                    formatImageOutputSize(tooltipData, t)
                  }}</span>
                </div>
                <div
                  v-if="formatImageSizeBreakdown(tooltipData)"
                  class="flex items-center justify-between gap-4"
                >
                  <span class="text-gray-400">{{ t('usage.imageSizeBreakdown') }}</span>
                  <span class="font-medium text-white">{{
                    formatImageSizeBreakdown(tooltipData)
                  }}</span>
                </div>
                <div v-if="showCostBreakdown" class="flex items-center justify-between gap-4">
                  <span class="text-gray-400">{{ t('usage.imageUnitPrice') }}</span>
                  <span class="font-medium text-sky-300"
                    >${{ imageUnitPrice(tooltipData).toFixed(6) }}</span
                  >
                </div>
                <div v-if="showCostBreakdown" class="flex items-center justify-between gap-4">
                  <span class="text-gray-400">{{ t('usage.imageTotalPrice') }}</span>
                  <span class="font-medium text-white"
                    >${{ tooltipData.total_cost?.toFixed(6) || '0.000000' }}</span
                  >
                </div>
              </template>
              <template
                v-else-if="
                  showCostBreakdown &&
                  (!getDisplayBillingMode(tooltipData) ||
                    getDisplayBillingMode(tooltipData) === BILLING_MODE_TOKEN)
                "
              >
                <div
                  v-if="tooltipData && tooltipData.input_tokens > 0"
                  class="flex items-center justify-between gap-4"
                >
                  <span class="text-gray-400">{{ t('usage.inputTokenPrice') }}</span>
                  <span class="font-medium text-sky-300"
                    >{{
                      formatTokenPricePerMillion(tooltipData.input_cost, tooltipData.input_tokens, {
                        currencySymbol: usdUnitSymbol,
                      })
                    }}
                    {{ t('usage.perMillionTokens') }}</span
                  >
                </div>
                <div
                  v-if="
                    tooltipData && tooltipData.output_cost > 0 && textOutputTokens(tooltipData) > 0
                  "
                  class="flex items-center justify-between gap-4"
                >
                  <span class="text-gray-400">{{ t('usage.outputTokenPrice') }}</span>
                  <span class="font-medium text-violet-300"
                    >{{
                      formatTokenPricePerMillion(
                        tooltipData.output_cost,
                        textOutputTokens(tooltipData),
                        { currencySymbol: usdUnitSymbol },
                      )
                    }}
                    {{ t('usage.perMillionTokens') }}</span
                  >
                </div>
                <div
                  v-if="tooltipData && hasImageOutputTokens(tooltipData)"
                  class="flex items-center justify-between gap-4"
                >
                  <span class="text-gray-400">{{ t('usage.imageOutputTokenPrice') }}</span>
                  <span class="font-medium text-pink-300"
                    >{{
                      formatTokenPricePerMillion(
                        tooltipData.image_output_cost ?? 0,
                        tooltipData.image_output_tokens,
                        { currencySymbol: usdUnitSymbol },
                      )
                    }}
                    {{ t('usage.perMillionTokens') }}</span
                  >
                </div>
              </template>
              <div
                v-else-if="showCostBreakdown && tooltipData"
                class="flex items-center justify-between gap-4"
              >
                <span class="text-gray-400">{{
                  tooltipData.billing_mode === BILLING_MODE_IMAGE
                    ? t('usage.imageUnitPrice')
                    : t('usage.unitPrice')
                }}</span>
                <span class="font-medium text-sky-300">{{
                  formatDetailedUsdAmount(tooltipData.total_cost)
                }}</span>
              </div>
              <div
                v-if="showCostBreakdown && tooltipData && tooltipData.cache_creation_cost > 0"
                class="flex items-center justify-between gap-4"
              >
                <span class="text-gray-400">{{ t('admin.usage.cacheCreationCost') }}</span>
                <span class="font-medium text-white">{{
                  formatDetailedUsdAmount(tooltipData.cache_creation_cost)
                }}</span>
              </div>
              <div
                v-if="showCostBreakdown && tooltipData && tooltipData.cache_read_cost > 0"
                class="flex items-center justify-between gap-4"
              >
                <span class="text-gray-400">{{ t('admin.usage.cacheReadCost') }}</span>
                <span class="font-medium text-white">{{
                  formatDetailedUsdAmount(tooltipData.cache_read_cost)
                }}</span>
              </div>
            </div>
            <!-- Rate and Summary -->
            <div class="flex items-center justify-between gap-6">
              <span class="text-gray-400">{{ t('usage.serviceTier') }}</span>
              <span class="font-semibold text-cyan-300">{{
                getUsageServiceTierLabel(tooltipData?.service_tier, t)
              }}</span>
            </div>
            <div class="flex items-center justify-between gap-6">
              <span class="text-gray-400">{{ t('usage.rate') }}</span>
              <span class="font-semibold text-blue-400"
                >{{ formatMultiplier(tooltipData?.rate_multiplier || 1) }}x</span
              >
            </div>
            <div
              v-if="showStandardCost && (!isCompact || !isSystemRow(tooltipData))"
              class="flex items-center justify-between gap-6"
            >
              <span class="text-gray-400">{{ t('usage.original') }}</span>
              <span class="font-medium text-white">{{
                formatDetailedUsdAmount(tooltipData?.total_cost)
              }}</span>
            </div>
            <div
              v-if="!isCompact || !isSystemRow(tooltipData)"
              class="flex items-center justify-between gap-6"
            >
              <span class="text-gray-400">{{ t('usage.userBilled') }}</span>
              <span class="font-semibold text-green-400">{{
                formatDetailedBalance(tooltipData?.actual_cost)
              }}</span>
            </div>
            <!-- Provider billing (separated from user billing) -->
            <template v-if="showProviderBilling">
              <div
                class="flex items-center justify-between gap-6 border-t border-gray-700 pt-1.5 dark:border-dark-600"
              >
                <span class="text-gray-400">{{ t('usage.providerMultiplier') }}</span>
                <span class="font-semibold text-blue-400"
                  >{{ formatMultiplier(tooltipData?.provider_rate_multiplier ?? 1) }}x</span
                >
              </div>
              <div class="flex items-center justify-between gap-6">
                <span class="text-gray-400">{{ t('usage.providerBilled') }}</span>
                <span class="font-semibold text-green-400">
                  {{
                    isCompact && tooltipData?.system_usage?.cost_state === 'unknown'
                      ? t('usage.costUnknown')
                      : formatDetailedUsdAmount(
                          providerBilled({
                            total_cost: tooltipData?.total_cost,
                            provider_stats_cost: tooltipData?.provider_stats_cost,
                            provider_rate_multiplier: tooltipData?.provider_rate_multiplier,
                          }),
                        )
                  }}
                  <span v-if="isCompact && tooltipData?.system_usage?.cost_state === 'partial'">
                    {{ t('usage.costPartial') }}
                  </span>
                </span>
              </div>
            </template>
          </div>
          <div
            v-if="tooltipPosition.placement === 'right'"
            class="tooltip-caret -left-1 -translate-y-1/2 border-b border-l"
            :style="{ top: tooltipPosition.arrowY + 'px' }"
          ></div>
          <div
            v-else-if="tooltipPosition.placement === 'left'"
            class="tooltip-caret -right-1 -translate-y-1/2 border-r border-t"
            :style="{ top: tooltipPosition.arrowY + 'px' }"
          ></div>
        </div>
      </div>
    </MotionTransition>
  </Teleport>
</template>

<script setup lang="ts">
import RequestTimingDetails from '@/components/requests/RequestTimingDetails.vue'
import RequestIdLink from '@/components/common/RequestIdLink.vue'
import MotionTransition from '@/components/common/MotionTransition.vue'
import { computed, nextTick, onMounted, onUnmounted, ref, useId } from 'vue'
import { useI18n } from 'vue-i18n'
import { useBalanceDisplay } from '@/composables/useBalanceDisplay'
import { useClipboard } from '@/composables/useClipboard'
import { COPY_FEEDBACK_MS } from '@/constants/ui'
import {
  formatCompactDateTimeParts,
  formatDateTime,
  formatReasoningEffort,
  reasoningEffortValuesEqual,
  truncateMiddle,
} from '@/utils/format'
import { formatCompactUserAgent } from '@/utils/userAgent'
import { formatCacheTokens, formatMultiplier } from '@/utils/formatters'
import { formatTokenPricePerMillion } from '@/utils/usagePricing'
import { formatRequestSize } from '@/utils/requestTiming'
import { getUsageServiceTierLabel } from '@/utils/usageServiceTier'
import { resolveUsageRequestType } from '@/utils/usageRequestType'
import { formatUsageTps } from '@/utils/usageTps'
import {
  LATENCY_BAR_CLASSES,
  LATENCY_BAR_FROM_CLASSES,
  LATENCY_BAR_TO_CLASSES,
  LATENCY_TEXT_CLASSES,
  durationSeverity,
  firstTokenSeverity,
} from '@/utils/latencyHealth'
import {
  BILLING_MODE_IMAGE,
  BILLING_MODE_TOKEN,
  getBillingModeBadgeClass,
  getBillingModeLabel,
  getDisplayBillingMode,
  imageUnitPrice,
  isImageUsage,
} from '@/utils/billingMode'
import {
  formatImageBillingSize,
  formatImageInputSize,
  formatImageOutputSize,
  formatImageSizeBreakdown,
  formatImageSizeSource,
  hasImageOutputCost,
  hasImageOutputTokens,
  textOutputTokens,
  hasImageInputTokens,
  uncachedImageInputTokens,
  textInputTokens,
  hasImageInputCost,
} from '@/utils/imageUsage'

import DataTable from '@/components/common/table/DataTable.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import HelpTooltip from '@/components/common/HelpTooltip.vue'
import GroupBadge from '@/components/common/GroupBadge.vue'
import IpGeoCell from '@/components/common/IpGeoCell.vue'
import ProviderIcon from '@/components/common/ProviderIcon.vue'
import Icon from '@/components/icons/Icon.vue'
import { fetchBatch, getEntry } from '@/utils/ipGeoLookup'
import type { AdminUsageLog } from '@/api/usage.types'
import type { Column } from '@/components/common/table/types'
import type { UsageTableRole } from './usageTableColumns'

// UsageTable.vue renders usage rows with request identity, billing, tokens, and optional
// diagnostics for administrator and user pages. Compact rows keep the same nine columns
// across roles and cap each field group at four lines. Legacy callers can still select
// individual fields. User billing and provider charges use different amount formatters.
//
// Floating cost, token, and timing panels render outside table cells to escape
// scrolling-container clipping. Their row identities are checked after Vue updates before
// positioning, so a superseded opening cannot position another row's panel. Timing
// additionally supports pinned inspection on touch screens. Viewport measurements choose a
// side placement when space permits and clamp overlays when neither side fits.

// providerBilled multiplies the provider statistics cost by the provider rate. A missing
// statistics cost uses total cost, a missing multiplier uses one, and NaN produces zero for
// display.
function providerBilled(row: {
  total_cost?: number | null // standard total usage cost
  // provider_stats_cost takes precedence over total_cost for provider billing; a null value
  // selects the standard total as its base.
  provider_stats_cost?: number | null
  // provider_rate_multiplier scales the selected provider cost base; omission or null uses
  // a multiplier of one.
  provider_rate_multiplier?: number | null
}): number {
  const base = row.provider_stats_cost != null ? row.provider_stats_cost : (row.total_cost ?? 0)
  const result = base * (row.provider_rate_multiplier ?? 1)
  return Number.isNaN(result) ? 0 : result
}

// Props controls usage rows, visible columns, sorting, and role-specific billing
// presentation.
interface Props {
  data: AdminUsageLog[] // parent-owned usage row records
  loading?: boolean // table query progress flag
  columns: Column[] // visible table column definitions
  // compactRole enables composite columns and applies their role-specific visibility.
  // Omission keeps the individual slots available to legacy tables.
  compactRole?: UsageTableRole
  // columnOrderStorageKey names the local preference owned by this table instance; user and
  // administrator pages supply separate storage keys.
  columnOrderStorageKey?: string
  /** Carries the page migration result across a failed browser storage write. */
  initialColumnOrder?: readonly string[]
  serverSideSort?: boolean // server sorting event flag
  defaultSortKey?: string // initial sorted column key
  defaultSortOrder?: 'asc' | 'desc' // initial sort direction value
  showProviderBilling?: boolean // provider charge visibility flag
  // showStandardCost controls detailed standard charge visibility; user pages disable it
  // when presenting only actual user charges.
  showStandardCost?: boolean
  showUpstreamEndpoint?: boolean // upstream endpoint visibility flag
  // userClickable permits member names to emit administrator balance navigation; user pages
  // disable this action for team member rows.
  userClickable?: boolean
  /* Team usage keeps a compact member column and truncates long email text inside the cell. */
  compactUserColumn?: boolean // compact member display flag
  // flat removes the component card container when a parent card already owns the table and
  // pagination surface.
  flat?: boolean
  // showIpGeoToolbar enables the internal batch toolbar when the IP column is visible;
  // pages with their own toolbar disable it.
  showIpGeoToolbar?: boolean
}

// props supplies table defaults and parent-owned usage records consumed by row slots and
// floating detail panels.
const props = withDefaults(defineProps<Props>(), {
  loading: false,
  columnOrderStorageKey: 'usage-column-order',
  serverSideSort: false,
  defaultSortKey: '',
  defaultSortOrder: 'asc',
  showProviderBilling: true,
  showStandardCost: true,
  showUpstreamEndpoint: true,
  userClickable: true,
  compactUserColumn: false,
  flat: false,
  showIpGeoToolbar: true,
})
// emit publishes user navigation, server sorting, and batch IP lookup failures to the
// owning usage page.
const emit = defineEmits<{
  userClick: [userID: number, email?: string] // selected user navigation payload
  sort: [key: string, order: 'asc' | 'desc'] // column and direction payload
  ipGeoBatchFailed: [] // batch lookup failure notification
}>()
// Translation, balance and USD formatting, and clipboard access supply presentation and
// copy feedback for displayed usage rows.
const { t } = useI18n()
const { balanceUnitSymbol, usdUnitSymbol, formatBalanceAmount, formatUsdAmount } =
  useBalanceDisplay()
const { copyToClipboard } = useClipboard()
// copiedRequestId marks the upstream identifier whose copy action most recently succeeded
// until its feedback timer clears it.
const copiedRequestId = ref<string | null>(null)
// The regular table takes its visibility flags from the caller. Compact rows also
// require administrator layout for fields excluded by the user usage endpoint.
const isCompact = computed(() => props.compactRole !== undefined)
const showAdminDetails = computed(() => !isCompact.value || props.compactRole === 'admin')
const showProviderBilling = computed(() => showAdminDetails.value && props.showProviderBilling)
const showStandardCost = computed(() => showAdminDetails.value && props.showStandardCost)
const showUpstreamEndpoint = computed(() => props.showUpstreamEndpoint)
const userClickable = computed(() => props.userClickable)
const latencyCell = computed(() => (isCompact.value ? 'cell-performance' : 'cell-latency'))

// Compact timestamps and mapping chains are computed once for the current page so
// display, titles, and details reuse the same values.
const compactTimes = computed(
  () =>
    new Map(props.data.map((row) => [row.created_at, formatCompactDateTimeParts(row.created_at)])),
)
const modelMappings = computed(() => {
  if (!showAdminDetails.value) return new Map<AdminUsageLog, string[]>()
  return new Map(props.data.map((row) => [row, modelMappingSteps(row)]))
})

// User-agent feedback belongs to the row whose exact header was copied.
const copiedUserAgent = ref<number | null>(null)
let userAgentFeedbackTimer: ReturnType<typeof setTimeout> | undefined

// copyUserAgent keeps the recorded header intact even though the cell shows a summary.
async function copyUserAgent(row: AdminUsageLog) {
  if (!row.user_agent || !(await copyToClipboard(row.user_agent))) return
  copiedUserAgent.value = row.id
  clearTimeout(userAgentFeedbackTimer)
  userAgentFeedbackTimer = setTimeout(() => {
    copiedUserAgent.value = null
  }, COPY_FEEDBACK_MS)
}

// compactUserColumn follows the parent's compact member-column preference for team table
// width and email truncation.
const compactUserColumn = computed(() => props.compactUserColumn)
// ipGeoBatchLoading disables overlapping toolbar interaction while the current batch lookup
// promise is being awaited.
const ipGeoBatchLoading = ref(false)

// maskEmailLocalPart displays the first and last Unicode characters of the trimmed email
// local part. Empty local parts return empty text; one-character names repeat that
// character around the mask.
const maskEmailLocalPart = (email: string): string => {
  const localPart = email.split('@', 1)[0]?.trim() || ''
  if (!localPart) return ''
  const characters = Array.from(localPart)
  return `${characters[0]}***${characters[characters.length - 1]}`
}

// usageUserDisplayName prefers a nonempty custom username over a masked email local part.
// Missing user names and emails return empty text so the template can use its identifier
// presentation.
const usageUserDisplayName = (row: AdminUsageLog): string => {
  const username = row.user?.username?.trim() || ''
  if (username) return username
  const email = row.user?.email?.trim() || ''
  return email ? maskEmailLocalPart(email) : ''
}

// usageUserTitle joins a distinct username and email for the hover title. If only one
// exists, or both strings match, that single value supplies the complete title.
const usageUserTitle = (row: AdminUsageLog): string => {
  const username = row.user?.username?.trim() || ''
  const email = row.user?.email?.trim() || ''
  if (username && email && username !== email) return `${username} (${email})`
  return username || email
}

// showIpGeoToolbar requires both parent permission and a visible IP column before
// presenting page-level lookup controls.
const showIpGeoToolbar = computed(
  () =>
    props.showIpGeoToolbar &&
    props.columns.some((col) => col.key === (isCompact.value ? 'client_network' : 'ip_address')),
)

// hasReasoningEffortMapping reports a difference between nonempty requested and forwarded
// effort values. Comparison uses the shared normalization rule so equivalent spellings do
// not create an extra mapping label.
const hasReasoningEffortMapping = (row: AdminUsageLog): boolean => {
  const requested = row.requested_reasoning_effort?.trim() || ''
  const forwarded = row.reasoning_effort?.trim() || ''
  return requested !== '' && forwarded !== '' && !reasoningEffortValuesEqual(requested, forwarded)
}

// modelMappingSteps preserves intermediate hops. Older records may carry only
// the upstream model, which still provides the requested-to-forwarded pair.
function modelMappingSteps(row: AdminUsageLog): string[] {
  const steps = row.model_mapping_chain
    ?.split('→')
    .map((step) => step.trim())
    .filter(Boolean)
  if (steps && steps.length > 1) return steps
  if (row.upstream_model && row.upstream_model !== row.model) return [row.model, row.upstream_model]
  return []
}

// lastMappedModel resolves the last recorded hop without altering the mapping chain.
function lastMappedModel(row: AdminUsageLog): string | undefined {
  const steps = modelMappings.value.get(row)
  return steps?.[steps.length - 1]
}

// forwardedModel prefers the observed upstream model over the last mapping hop.
function forwardedModel(row: AdminUsageLog): string | undefined {
  return row.upstream_model || lastMappedModel(row)
}

// responseModelTitle identifies the returned model relative to the forwarded model.
function responseModelTitle(row: AdminUsageLog): string {
  return (
    t('usage.upstreamResponseModel') +
    ' · ' +
    (forwardedModel(row) || row.model) +
    ' → ' +
    row.upstream_response_model
  )
}

// reasoningEffortSummary preserves an observed rewrite of the requested effort.
function reasoningEffortSummary(row: AdminUsageLog): string {
  const requested = formatReasoningEffort(row.requested_reasoning_effort)
  const applied = formatReasoningEffort(row.reasoning_effort)
  if (hasReasoningEffortMapping(row)) return requested + ' → ' + applied
  return row.requested_reasoning_effort ? requested : applied
}

// modelMetadata supplies the complete line in the model details panel and hover title.
function modelMetadata(row: AdminUsageLog): string {
  const labels = [
    getRequestTypeLabel(row),
    getBillingModeLabel(row.billing_mode || getDisplayBillingMode(row), t),
  ]
  if (row.requested_reasoning_effort || row.reasoning_effort)
    labels.push(reasoningEffortSummary(row))
  if (row.native_compaction_v2) labels.push(t('usage.nativeCompactionV2'))
  return labels.join(' · ')
}

// isSystemRow limits internal cost and caller fields to administrator diagnostics.
function isSystemRow(row: AdminUsageLog | null): boolean {
  return showAdminDetails.value && Boolean(row?.system_usage)
}

// systemRequestLabel keeps unnamed system requests distinguishable from customer rows.
function systemRequestLabel(row: AdminUsageLog): string {
  return t(
    row.request_source
      ? 'admin.usage.requestSources.' + row.request_source
      : 'admin.usage.requestSources.system',
  )
}

// billingFundingLabel only accepts explicit balance and subscription codes. Missing
// or unknown codes leave the funding line empty.
function billingFundingLabel(row: AdminUsageLog): string {
  if (row.billing_type === 0) return t('admin.usage.billingTypeBalance')
  if (row.billing_type === 1) return t('admin.usage.billingTypeSubscription')
  return ''
}

// compactTokenCounts orders the four visible token counters by reading order.
function compactTokenCounts(row: AdminUsageLog) {
  return [
    { label: 'usage.compactTable.tokenInput', value: (row.input_tokens ?? 0).toLocaleString() },
    { label: 'usage.compactTable.tokenOutput', value: (row.output_tokens ?? 0).toLocaleString() },
    { label: 'usage.compactTable.cacheRead', value: (row.cache_read_tokens ?? 0).toLocaleString() },
    {
      label: 'usage.compactTable.cacheWrite',
      value: (row.cache_creation_tokens ?? 0).toLocaleString(),
    },
  ]
}

// cacheBucketSummary keeps TTL amounts on one line; the token panel keeps full counts.
function cacheBucketSummary(row: AdminUsageLog): string {
  return cacheWriteBuckets(row)
    .map((bucket) => bucket.ttl + ' ' + formatCacheTokens(bucket.tokens))
    .join(' · ')
}

// imageUsageCount combines generated images and optional animation frames.
function imageUsageCount(row: AdminUsageLog): string {
  const images = row.image_count + t('usage.imageUnit')
  if (!row.image_frame_count) return images
  return images + ' · ' + row.image_frame_count + t('usage.imageFrameUnit')
}

// currentPageIps deduplicates nonempty addresses from the visible data page before batch
// location lookup uses them.
const currentPageIps = computed(() =>
  Array.from(
    new Set(props.data.map((row) => row.ip_address).filter((ip): ip is string => Boolean(ip))),
  ),
)

// pendingIpCount counts uncached or failed page addresses when the IP toolbar is visible;
// successful entries need no new request.
const pendingIpCount = computed(() => {
  if (!showIpGeoToolbar.value) return 0
  return currentPageIps.value.filter((ip) => {
    const status = getEntry(ip).status
    return status === 'idle' || status === 'error'
  }).length
})

// handleBatchFetchIpGeo requests location data for distinct addresses on the current page.
// A false result emits a failure event for the parent, and completion releases the toolbar
// loading flag.
const handleBatchFetchIpGeo = async () => {
  ipGeoBatchLoading.value = true
  try {
    const ok = await fetchBatch(currentPageIps.value)
    if (!ok) emit('ipGeoBatchFailed')
  } finally {
    ipGeoBatchLoading.value = false
  }
}

// copyUpstreamRequestId copies the upstream identifier through the shared clipboard
// helper and marks it after a successful asynchronous completion. Failed copies
// return before changing the visible identifier, while successful completions
// publish in their completion order.
//
// Each success schedules a timer that clears feedback if its identifier still
// matches, protecting a later copy of a different identifier. Repeated copies of
// the same identifier share that comparison, so an earlier timer can clear the
// newer feedback before its own timer expires.
const copyUpstreamRequestId = async (upstreamRequestId: string) => {
  if (!(await copyToClipboard(upstreamRequestId, t('admin.usage.upstreamRequestIdCopied')))) return
  copiedRequestId.value = upstreamRequestId
  // Delay clearing successful copy feedback, and compare the identifier so a later copy of
  // another row keeps its own indicator.
  window.setTimeout(() => {
    if (copiedRequestId.value === upstreamRequestId) copiedRequestId.value = null
  }, COPY_FEEDBACK_MS)
}

// Each table's focused trigger describes its own floating panel.
const tokenTooltipId = useId()
const costTooltipId = useId()
const timingTooltipId = useId()

// tooltipVisible, tooltipReady, tooltipRef, tooltipPosition, and tooltipData coordinate the
// cost panel: selection and mounting precede measured positioning, then readiness reveals
// its content.
const tooltipVisible = ref(false)
const tooltipReady = ref(false)
const tooltipRef = ref<HTMLElement | null>(null)
const tooltipPosition = ref<TooltipPosition>({ x: 0, y: 0, arrowY: 0, placement: 'overlay' })
const tooltipData = ref<AdminUsageLog | null>(null)
// Standard amounts follow the billing visibility setting. Recorded image dimensions
// remain available separately because they describe the request rather than its price.
const showCostBreakdown = computed(
  () => !isCompact.value || (showStandardCost.value && !isSystemRow(tooltipData.value)),
)

// tokenTooltipVisible, tokenTooltipReady, tokenTooltipRef, tokenTooltipPosition, and
// tokenTooltipData coordinate the token panel: selection and mounting precede measured
// positioning, then readiness reveals its content.
const tokenTooltipVisible = ref(false)
const tokenTooltipReady = ref(false)
const tokenTooltipRef = ref<HTMLElement | null>(null)
const tokenTooltipPosition = ref<TooltipPosition>({ x: 0, y: 0, arrowY: 0, placement: 'overlay' })
const tokenTooltipData = ref<AdminUsageLog | null>(null)

// tokenTooltipCacheWrites uses the same recorded buckets as the usage cell.
const tokenTooltipCacheWrites = computed(() =>
  tokenTooltipData.value ? cacheWriteBuckets(tokenTooltipData.value) : [],
)

// CacheWriteBucket pairs a displayed count with its concrete billed cache lifetime.
interface CacheWriteBucket {
  ttl: '5m' | '30m' | '1h' | '12h' // Concrete billed cache lifetime tag.
  tokens: number // Positive token count for this bucket.
}

// cacheWriteBuckets partitions cache_creation_tokens into strictly one two-tier lifetime
// family (5m/1h or 30m/12h). Untagged or partial counts assign the missing remainder to the
// family's default short bucket and overreported counts cap at total, so displayed buckets
// never expose untagged cache-write counts.
function cacheWriteBuckets(row: AdminUsageLog): CacheWriteBucket[] {
  const total = row.cache_creation_tokens
  if (!(total > 0)) return []

  const hasOpenAIBuckets =
    row.cache_creation_30m_tokens != null || row.cache_creation_12h_tokens != null
  const hasClaudeBuckets =
    (row.cache_creation_5m_tokens ?? 0) > 0 || (row.cache_creation_1h_tokens ?? 0) > 0
  const normalizedPlatform = (row.platform || '').trim().toLowerCase()
  const isOpenAIFamily =
    hasOpenAIBuckets ||
    (!hasClaudeBuckets &&
      (normalizedPlatform === 'openai' ||
        normalizedPlatform === 'grok' ||
        /^(gpt-|chatgpt-|o1|o3|o4|codex)/i.test((row.model || '').trim())))

  const shortTtl = isOpenAIFamily ? '30m' : '5m'
  const longTtl = isOpenAIFamily ? '12h' : '1h'
  let shortTokens = Math.max(
    isOpenAIFamily ? (row.cache_creation_30m_tokens ?? 0) : (row.cache_creation_5m_tokens ?? 0),
    0,
  )
  let longTokens = Math.max(
    isOpenAIFamily ? (row.cache_creation_12h_tokens ?? 0) : (row.cache_creation_1h_tokens ?? 0),
    0,
  )
  const sum = shortTokens + longTokens
  if (sum < total) {
    shortTokens += total - sum
  } else if (sum > total) {
    shortTokens = Math.min(shortTokens, total)
    longTokens = total - shortTokens
  }
  const buckets: CacheWriteBucket[] = []
  if (shortTokens > 0) buckets.push({ ttl: shortTtl, tokens: shortTokens })
  if (longTokens > 0) buckets.push({ ttl: longTtl, tokens: longTokens })
  return buckets
}

// cacheWriteAnomalyKindLabel resolves the compact localized status label for an upstream
// cache-write discrepancy category.
function cacheWriteAnomalyKindLabel(
  kind: NonNullable<AdminUsageLog['cache_write_anomaly']>['kind'],
): string {
  if (kind === 'partial_breakdown') return t('admin.usage.cacheWriteAnomalyKindPartial')
  if (kind === 'missing_total') return t('admin.usage.cacheWriteAnomalyKindMissingTotal')
  return t('admin.usage.cacheWriteAnomalyKindExceeds')
}

// cacheWriteAnomalyRawSplitLabel names the reported short and long TTL pair for the compact
// anomaly diagnostic section inside the token detail tooltip.
function cacheWriteAnomalyRawSplitLabel(
  anomaly: NonNullable<AdminUsageLog['cache_write_anomaly']>,
): string {
  const isOpenAI = anomaly.reported_30m != null || anomaly.reported_12h != null
  return t('admin.usage.cacheWriteAnomalyRawSplit', {
    short: isOpenAI ? '30m' : '5m',
    long: isOpenAI ? '12h' : '1h',
  })
}

// cacheWriteAnomalyRawSplitValue formats the raw upstream short and long TTL counts, keeping
// omitted optional OpenAI pointers distinguishable as '-' rather than zero.
function cacheWriteAnomalyRawSplitValue(
  anomaly: NonNullable<AdminUsageLog['cache_write_anomaly']>,
): string {
  if (anomaly.reported_30m != null || anomaly.reported_12h != null) {
    const shortText = anomaly.reported_30m != null ? anomaly.reported_30m.toLocaleString() : '-'
    const longText = anomaly.reported_12h != null ? anomaly.reported_12h.toLocaleString() : '-'
    return `${shortText} / ${longText}`
  }
  return `${(anomaly.reported_5m ?? 0).toLocaleString()} / ${(anomaly.reported_1h ?? 0).toLocaleString()}`
}

// formatCacheWriteAnomalyText formats administrator cache-write anomaly kind and raw upstream
// counts for inline badge hover titles.
function formatCacheWriteAnomalyText(
  anomaly: NonNullable<AdminUsageLog['cache_write_anomaly']>,
): string {
  const parts: string[] = [`total:${anomaly.reported_total.toLocaleString()}`]
  if (anomaly.reported_30m != null || anomaly.reported_12h != null) {
    parts.push(`30m:${anomaly.reported_30m != null ? anomaly.reported_30m.toLocaleString() : '-'}`)
    parts.push(`12h:${anomaly.reported_12h != null ? anomaly.reported_12h.toLocaleString() : '-'}`)
  } else {
    parts.push(`5m:${(anomaly.reported_5m ?? 0).toLocaleString()}`)
    parts.push(`1h:${(anomaly.reported_1h ?? 0).toLocaleString()}`)
  }
  const raw = parts.join(', ')
  if (anomaly.kind === 'partial_breakdown') {
    return t('admin.usage.cacheWriteAnomalyPartial', { raw })
  }
  if (anomaly.kind === 'missing_total') {
    return t('admin.usage.cacheWriteAnomalyMissingTotal', { raw })
  }
  return t('admin.usage.cacheWriteAnomalyExceeds', { raw })
}

// timingTooltipVisible, timingTooltipReady, timingTooltipRef, timingTooltipPosition, and
// timingTooltipData coordinate the timing panel: selection and mounting precede measured
// positioning, then readiness reveals its content.
const timingTooltipVisible = ref(false)
const timingTooltipReady = ref(false)
const timingTooltipRef = ref<HTMLElement | null>(null)
const timingTooltipPosition = ref<TooltipPosition>({ x: 0, y: 0, arrowY: 0, placement: 'overlay' })
const timingTooltipData = ref<AdminUsageLog | null>(null)
// timingTooltipAnchor identifies the click target exempt from outside-click dismissal,
// while timingTooltipPinned keeps inspection open after pointer exit.
const timingTooltipAnchor = ref<HTMLElement | null>(null)
const timingTooltipPinned = ref(false)

// TooltipPlacement identifies a side panel or a centered viewport-clamped overlay.
type TooltipPlacement = 'left' | 'right' | 'overlay'

// TooltipPosition carries viewport coordinates and arrow geometry for floating detail
// panels.
interface TooltipPosition {
  x: number // viewport horizontal pixel coordinate
  y: number // viewport vertical pixel coordinate
  arrowY: number // vertical arrow pixel offset
  placement: TooltipPlacement // panel side or overlay
}

// Tooltip spacing constants keep side panels clear of their anchors and reserve padding
// inside the visual viewport.
const TOOLTIP_GAP = 8
const TOOLTIP_VIEWPORT_PADDING = 12

// clamp bounds a coordinate and treats an inverted interval as its minimum endpoint.
// Oversized panels therefore keep their leading edge at the viewport padding even when both
// edges cannot fit.
const clamp = (value: number, min: number, max: number): number =>
  Math.min(Math.max(value, min), Math.max(min, max))

// calculateTooltipPosition chooses viewport-relative coordinates from anchor and rendered
// panel dimensions. It prefers the anchor's right side, then its left side, before using an
// overlay centered on the anchor.
//
// Visual viewport offsets account for a shifted visible region, and vertical clamping keeps
// the anchor midpoint as the arrow target where possible. Side arrows retain twelve pixels
// of edge clearance. If the panel is wider than the available bounds, clamp keeps its
// leading edge at the minimum padded coordinate.
const calculateTooltipPosition = (anchorRect: DOMRect, tooltipRect: DOMRect): TooltipPosition => {
  // Use the visible viewport when available so zoom and viewport displacement affect both
  // placement bounds and the reserved edge padding.
  const visualViewport = window.visualViewport
  const viewportLeft = visualViewport?.offsetLeft ?? 0
  const viewportTop = visualViewport?.offsetTop ?? 0
  const viewportWidth = visualViewport?.width ?? window.innerWidth
  const viewportHeight = visualViewport?.height ?? window.innerHeight
  const minX = viewportLeft + TOOLTIP_VIEWPORT_PADDING
  const maxX = viewportLeft + viewportWidth - TOOLTIP_VIEWPORT_PADDING - tooltipRect.width
  const minY = viewportTop + TOOLTIP_VIEWPORT_PADDING
  const maxY = viewportTop + viewportHeight - TOOLTIP_VIEWPORT_PADDING - tooltipRect.height
  const anchorCenterY = anchorRect.top + anchorRect.height / 2
  const y = clamp(anchorCenterY - tooltipRect.height / 2, minY, maxY)

  const rightX = anchorRect.right + TOOLTIP_GAP
  if (rightX + tooltipRect.width <= viewportLeft + viewportWidth - TOOLTIP_VIEWPORT_PADDING) {
    return {
      x: rightX,
      y,
      arrowY: clamp(anchorCenterY - y, 12, tooltipRect.height - 12),
      placement: 'right',
    }
  }

  const leftX = anchorRect.left - TOOLTIP_GAP - tooltipRect.width
  if (leftX >= minX) {
    return {
      x: leftX,
      y,
      arrowY: clamp(anchorCenterY - y, 12, tooltipRect.height - 12),
      placement: 'left',
    }
  }

  return {
    x: clamp(anchorRect.left + anchorRect.width / 2 - tooltipRect.width / 2, minX, maxX),
    y,
    arrowY: 0,
    placement: 'overlay',
  }
}

// getRequestTypeLabel translates the resolved canonical request mode, including live and
// cyber requests. Unknown modes use the unknown label after the shared resolver interprets
// legacy stream fields.
const getRequestTypeLabel = (row: AdminUsageLog): string => {
  const requestType = resolveUsageRequestType(row)
  if (requestType === 'cyber') return t('usage.cyber')
  if (requestType === 'live') return t('usage.live')
  if (requestType === 'ws_v2') return t('usage.ws')
  if (requestType === 'stream') return t('usage.stream')
  if (requestType === 'sync') return t('usage.sync')
  return t('usage.unknown')
}

// getRequestTypeBadgeClass selects colors for the resolved request mode. Unknown modes use
// an amber badge, keeping unsupported or incomplete historical mode values visible as a
// distinct display state.
const getRequestTypeBadgeClass = (row: AdminUsageLog): string => {
  const requestType = resolveUsageRequestType(row)
  if (requestType === 'cyber') return 'bg-red-100 text-red-800 dark:bg-red-900 dark:text-red-200'
  if (requestType === 'live')
    return 'bg-emerald-100 text-emerald-800 dark:bg-emerald-900 dark:text-emerald-200'
  if (requestType === 'ws_v2')
    return 'bg-violet-100 text-violet-800 dark:bg-violet-900 dark:text-violet-200'
  if (requestType === 'stream')
    return 'bg-blue-100 text-blue-800 dark:bg-blue-900 dark:text-blue-200'
  if (requestType === 'sync') return 'bg-gray-100 text-gray-800 dark:bg-dark-700 dark:text-dark-100'
  return 'bg-amber-100 text-amber-800 dark:bg-amber-900 dark:text-amber-200'
}

// formatUserAgent returns the supplied user-agent text for the usage cell.
const formatUserAgent = (ua: string): string => {
  return ua
}

// formatDetailedBalance formats six-decimal charges using the configured balance symbol.
// Missing values use a symbol-prefixed zero string so the detailed cost table keeps
// consistent numeric precision across rows.
const formatDetailedBalance = (value: number | null | undefined): string =>
  formatBalanceAmount(value, {
    fractionDigits: 6,
    fallback: `${balanceUnitSymbol.value}0.000000`,
  })

// formatDetailedUsdAmount formats six-decimal provider amounts using the USD formatter.
// Missing values use a USD-prefixed zero string to match the precision of other provider
// charge breakdown cells.
const formatDetailedUsdAmount = (value: number | null | undefined): string =>
  formatUsdAmount(value, {
    fractionDigits: 6,
    fallback: `${usdUnitSymbol}0.000000`,
  })

// formatDuration renders milliseconds, seconds, or compact minute and hour labels. Missing
// durations use a dash; longer durations round to whole seconds before splitting into the
// displayed larger units.
const formatDuration = (ms: number | null | undefined): string => {
  if (ms == null) return '-'
  if (ms < 1000) return `${ms}ms`
  // Keep hundredth-second precision for durations below one minute.
  if (ms < 60_000) return `${(ms / 1000).toFixed(2)}s`
  // Round to whole seconds before splitting minute and hour labels.
  const totalSec = Math.round(ms / 1000)
  if (totalSec < 3600) return `${Math.floor(totalSec / 60)}m ${totalSec % 60}s`
  return `${Math.floor(totalSec / 3600)}h ${Math.floor((totalSec % 3600) / 60)}m`
}

// showTooltip opens the cost panel for a row and measures it after Vue renders. The anchor
// rectangle is captured before waiting so positioning corresponds to the triggering cell.
//
// After nextTick, the panel must still be visible, reference the same row object, and have
// a mounted element. These checks discard work from an opening superseded by another row or
// a close action. Only a current panel receives its calculated position and becomes ready
// for display.
const showTooltip = async (event: Event, row: AdminUsageLog) => {
  // Capture the cost selection before rendering, then reject hidden or replaced
  // rows before publishing measured position and readiness.
  const target = event.currentTarget as HTMLElement
  const rect = target.getBoundingClientRect()
  tooltipData.value = row
  tooltipReady.value = false
  tooltipVisible.value = true
  await nextTick()

  if (!tooltipVisible.value || tooltipData.value !== row || !tooltipRef.value) return
  tooltipPosition.value = calculateTooltipPosition(rect, tooltipRef.value.getBoundingClientRect())
  tooltipReady.value = true
}

// hideTooltip closes the cost panel and clears its readiness and selected row.
const hideTooltip = () => {
  tooltipVisible.value = false
  tooltipReady.value = false
  tooltipData.value = null
}

// showTokenTooltip opens the token panel for a row and waits for its rendered dimensions.
// It captures the initiating cell rectangle before yielding to the Vue update cycle.
//
// A visibility, row-identity, and element check after nextTick rejects an opening that was
// closed or replaced while rendering. The accepted opening computes its viewport position
// before exposing the ready state, so another row's measurement cannot be published into
// the current token panel.
const showTokenTooltip = async (event: Event, row: AdminUsageLog) => {
  // Capture the token selection before rendering, then reject hidden or replaced
  // rows before publishing measured position and readiness.
  const target = event.currentTarget as HTMLElement
  const rect = target.getBoundingClientRect()
  tokenTooltipData.value = row
  tokenTooltipReady.value = false
  tokenTooltipVisible.value = true
  await nextTick()

  if (!tokenTooltipVisible.value || tokenTooltipData.value !== row || !tokenTooltipRef.value) return
  tokenTooltipPosition.value = calculateTooltipPosition(
    rect,
    tokenTooltipRef.value.getBoundingClientRect(),
  )
  tokenTooltipReady.value = true
}

// hideTokenTooltip closes the token panel and clears its readiness and selected row.
const hideTokenTooltip = () => {
  tokenTooltipVisible.value = false
  tokenTooltipReady.value = false
  tokenTooltipData.value = null
}

// updateTimingTooltip opens timing details only for a row carrying detailed timing
// observations. It records the triggering anchor and row before rendering the panel and
// measuring its dimensions.
//
// After nextTick, visibility, row identity, and the mounted element must still match the
// pending opening. A close or another row selection therefore prevents stale positioning.
// Accepted measurements set the panel coordinates and readiness; the caller separately
// decides whether this timing inspection remains pinned during pointer movement.
const updateTimingTooltip = async (event: Event, row: AdminUsageLog) => {
  if (!row.detailed_timing) return
  // Keep the timing row and anchor together across rendering; visibility and row
  // checks reject obsolete measurements before position and readiness publish.
  const target = event.currentTarget as HTMLElement
  const rect = target.getBoundingClientRect()
  timingTooltipAnchor.value = target
  timingTooltipData.value = row
  timingTooltipReady.value = false
  timingTooltipVisible.value = true
  await nextTick()

  if (!timingTooltipVisible.value || timingTooltipData.value !== row || !timingTooltipRef.value)
    return
  timingTooltipPosition.value = calculateTooltipPosition(
    rect,
    timingTooltipRef.value.getBoundingClientRect(),
  )
  timingTooltipReady.value = true
}

// showTimingTooltip starts the timing-panel opening flow for the supplied usage row.
const showTimingTooltip = (event: Event, row: AdminUsageLog) => {
  void updateTimingTooltip(event, row)
}

// closeTimingTooltip closes timing details and clears the selected row, anchor, readiness,
// and pinned state.
const closeTimingTooltip = () => {
  timingTooltipVisible.value = false
  timingTooltipReady.value = false
  timingTooltipData.value = null
  timingTooltipAnchor.value = null
  timingTooltipPinned.value = false
}

// hideTimingTooltip closes transient timing inspection after pointer exit. A pinned panel
// stays open so touch or click users can inspect its content until a separate close action.
const hideTimingTooltip = () => {
  if (timingTooltipPinned.value) return
  closeTimingTooltip()
}

// toggleTimingTooltip closes an already pinned panel for the same row or pins a new
// opening. It awaits timing-panel positioning after pinning so the click interaction
// survives hover exit.
const toggleTimingTooltip = async (event: MouseEvent, row: AdminUsageLog) => {
  if (timingTooltipVisible.value && timingTooltipPinned.value && timingTooltipData.value === row) {
    closeTimingTooltip()
    return
  }
  timingTooltipPinned.value = true
  await updateTimingTooltip(event, row)
}

// handleTimingTooltipDocumentClick ignores clicks contained by the current timing anchor.
// Every other document click closes the panel, clearing its pinned state and stored row
// through closeTimingTooltip.
const handleTimingTooltipDocumentClick = (event: MouseEvent) => {
  const target = event.target
  if (
    timingTooltipAnchor.value &&
    target instanceof Node &&
    timingTooltipAnchor.value.contains(target)
  )
    return
  closeTimingTooltip()
}

onMounted(() => {
  document.addEventListener('click', handleTimingTooltipDocumentClick)
})

// Remove the document-level timing listener when this table unmounts so later clicks
// cannot operate on its stored anchor state.
onUnmounted(() => {
  document.removeEventListener('click', handleTimingTooltipDocumentClick)
  clearTimeout(userAgentFeedbackTimer)
})
</script>
