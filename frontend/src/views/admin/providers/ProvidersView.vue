<template>
  <AppLayout fit-viewport>
    <!-- The desktop viewport bounds the scroll container used by the virtualized provider rows. -->
    <TablePageLayout>
      <template #filters>
        <div class="flex flex-col-reverse gap-2 lg:flex-row lg:items-start lg:justify-between">
          <div class="min-w-0 flex-1">
            <ProviderTableFilters
              v-model:searchQuery="params.search"
              :filters="params"
              :groups="groups"
              @update:filters="(newFilters) => Object.assign(params, newFilters)"
              @change="debouncedReload"
              @update:searchQuery="debouncedReload"
            />
          </div>
          <ProviderTableActions
            class="shrink-0 lg:justify-end"
            :loading="loading"
            @refresh="handleManualRefresh"
            @create="openCreateProvider()"
          >
            <template #after>
              <!-- Auto Refresh Dropdown -->
              <div class="relative" ref="autoRefreshDropdownRef">
                <button
                  ref="autoRefreshButtonRef"
                  @click="toggleAutoRefreshDropdown"
                  class="btn btn-secondary shrink-0 justify-center btn-icon"
                  :title="autoRefreshButtonTitle"
                >
                  <Icon
                    name="clock"
                    size="sm"
                    :class="autoRefreshEnabled ? 'text-primary-500' : ''"
                  />
                </button>
                <MotionTransition name="dropdown-fade">
                  <div
                    v-if="showAutoRefreshDropdown"
                    :inert="!showAutoRefreshDropdown || undefined"
                    class="dropdown fixed z-50 w-56 origin-top-left overflow-hidden py-0"
                    :style="autoRefreshDropdownStyle"
                  >
                    <div class="p-2">
                      <button
                        @click="setAutoRefreshEnabled(!autoRefreshEnabled)"
                        class="dropdown-item-sm justify-between rounded-control"
                      >
                        <span>{{ t('admin.providers.enableAutoRefresh') }}</span>
                        <Icon
                          v-if="autoRefreshEnabled"
                          name="check"
                          size="sm"
                          class="text-primary-500"
                          :animate-on-hover="false"
                        />
                      </button>
                      <div class="my-1 border-t border-gray-100 dark:border-dark-600"></div>
                      <button
                        v-for="sec in autoRefreshIntervals"
                        :key="sec"
                        @click="setAutoRefreshInterval(sec)"
                        class="dropdown-item-sm justify-between rounded-control"
                      >
                        <span>{{ autoRefreshIntervalLabel(sec) }}</span>
                        <Icon
                          v-if="autoRefreshIntervalSeconds === sec"
                          name="check"
                          size="sm"
                          class="text-primary-500"
                          :animate-on-hover="false"
                        />
                      </button>
                    </div>
                  </div>
                </MotionTransition>
              </div>

              <!-- Provider tools dropdown. -->
              <div class="relative" ref="providerToolsDropdownRef">
                <button
                  ref="providerToolsButtonRef"
                  @click="toggleProviderToolsDropdown"
                  class="btn btn-secondary justify-center lg:w-auto lg:px-3 lg:py-1.5 btn-icon"
                  :title="t('admin.providers.moreActions')"
                  :aria-expanded="showProviderToolsDropdown"
                >
                  <Icon name="more" size="sm" class="lg:hidden" />
                  <span class="hidden lg:inline">{{ t('admin.providers.moreActions') }}</span>
                  <Icon
                    name="chevronDown"
                    size="xs"
                    class="ml-1 hidden lg:inline"
                    :animate-on-hover="false"
                  />
                </button>
                <Teleport to="body">
                  <MotionTransition name="dropdown-fade">
                    <div
                      v-if="showProviderToolsDropdown"
                      :inert="!showProviderToolsDropdown || undefined"
                      class="dropdown fixed z-teleport-tooltip origin-top-right overflow-hidden py-0"
                      :style="providerToolsDropdownStyle"
                      @click.stop
                    >
                      <div
                        class="overflow-y-auto p-2"
                        :style="{ maxHeight: `${providerToolsDropdownPosition.maxHeight}px` }"
                      >
                        <div class="px-2 py-2">
                          <div
                            class="text-xs font-semibold uppercase tracking-wide text-gray-400 dark:text-gray-500"
                          >
                            {{ t('admin.providers.dataActions') }}
                          </div>
                        </div>
                        <button
                          class="dropdown-item-sm gap-3 rounded-control"
                          data-test="bulk-edit-filtered"
                          @click="openBulkEditFilteredFromMenu"
                        >
                          <span
                            class="provider-tools-menu-icon bg-primary-50 text-primary-600 dark:bg-primary-900/30 dark:text-primary-300"
                          >
                            <Icon name="edit" size="sm" />
                          </span>
                          <span class="flex-1 text-left">{{
                            t('admin.providers.bulkEditFiltered')
                          }}</span>
                        </button>
                        <button
                          class="dropdown-item-sm gap-3 rounded-control"
                          @click="openSyncFromCrs"
                        >
                          <span
                            class="provider-tools-menu-icon bg-blue-50 text-blue-600 dark:bg-blue-900/30 dark:text-blue-300"
                          >
                            <Icon name="sync" size="sm" />
                          </span>
                          <span class="flex-1 text-left">{{
                            t('admin.providers.syncFromCrs')
                          }}</span>
                        </button>
                        <button
                          class="dropdown-item-sm gap-3 rounded-control"
                          @click="openImportData"
                        >
                          <span
                            class="provider-tools-menu-icon bg-emerald-50 text-emerald-600 dark:bg-emerald-900/30 dark:text-emerald-300"
                          >
                            <Icon name="upload" size="sm" />
                          </span>
                          <span class="flex-1 text-left">{{
                            t('admin.providers.dataImport')
                          }}</span>
                        </button>
                        <button
                          class="dropdown-item-sm gap-3 rounded-control"
                          @click="openExportDataDialogFromMenu"
                        >
                          <span
                            class="provider-tools-menu-icon bg-violet-50 text-violet-600 dark:bg-violet-900/30 dark:text-violet-300"
                          >
                            <Icon name="download" size="sm" />
                          </span>
                          <span class="flex-1 text-left">
                            {{
                              selIds.length
                                ? t('admin.providers.dataExportSelected')
                                : t('admin.providers.dataExport')
                            }}
                          </span>
                          <span
                            v-if="selIds.length"
                            class="rounded-full bg-primary-100 px-2 py-0.5 text-xs font-medium text-primary-700 dark:bg-primary-900/40 dark:text-primary-300"
                          >
                            {{ t('admin.providers.selectedCount', { count: selIds.length }) }}
                          </span>
                        </button>

                        <div class="my-2 border-t border-gray-100 dark:border-dark-600"></div>
                        <div class="px-2 py-2">
                          <div
                            class="text-xs font-semibold uppercase tracking-wide text-gray-400 dark:text-gray-500"
                          >
                            {{ t('admin.providers.toolActions') }}
                          </div>
                        </div>
                        <button
                          class="dropdown-item-sm gap-3 rounded-control"
                          @click="openErrorPassthrough"
                        >
                          <span
                            class="provider-tools-menu-icon bg-amber-50 text-amber-600 dark:bg-amber-900/30 dark:text-amber-300"
                          >
                            <Icon name="shield" size="sm" />
                          </span>
                          <span class="flex-1 text-left">{{
                            t('admin.errorPassthrough.title')
                          }}</span>
                        </button>
                        <button
                          class="dropdown-item-sm gap-3 rounded-control"
                          @click="openTLSFingerprintProfiles"
                        >
                          <span
                            class="provider-tools-menu-icon bg-slate-100 text-slate-600 dark:bg-slate-700 dark:text-slate-200"
                          >
                            <Icon name="lock" size="sm" />
                          </span>
                          <span class="flex-1 text-left">{{
                            t('admin.tlsFingerprintProfiles.title')
                          }}</span>
                        </button>
                        <button
                          class="dropdown-item-sm gap-3 rounded-control"
                          @click="openTLSFingerprintRouters"
                        >
                          <span
                            class="provider-tools-menu-icon bg-cyan-50 text-cyan-600 dark:bg-cyan-900/30 dark:text-cyan-300"
                          >
                            <Icon name="swap" size="sm" />
                          </span>
                          <span class="flex-1 text-left">{{
                            t('admin.tlsFingerprintRouters.title')
                          }}</span>
                        </button>

                        <div class="my-2 border-t border-gray-100 dark:border-dark-600"></div>
                        <div class="px-2 py-2">
                          <div class="flex items-center justify-between gap-3">
                            <span
                              class="text-xs font-semibold uppercase tracking-wide text-gray-400 dark:text-gray-500"
                            >
                              {{ t('admin.providers.viewColumns') }}
                            </span>
                            <Icon name="grid" size="sm" class="text-gray-400" />
                          </div>
                        </div>
                        <div class="grid grid-cols-1 gap-1">
                          <button
                            v-for="col in toggleableColumns"
                            :key="col.key"
                            :data-toggle-column="col.key"
                            :aria-pressed="isColumnVisible(col.key)"
                            @click="toggleColumn(col.key)"
                            class="dropdown-item-sm justify-between rounded-control"
                          >
                            <span class="truncate">{{ col.label }}</span>
                            <Icon
                              v-if="isColumnVisible(col.key)"
                              name="check"
                              size="sm"
                              class="text-primary-500"
                              :animate-on-hover="false"
                            />
                          </button>
                        </div>
                      </div>
                    </div>
                  </MotionTransition>
                </Teleport>
              </div>
            </template>
          </ProviderTableActions>
        </div>
        <div
          v-if="hasPendingListSync"
          class="mt-2 flex items-center justify-between rounded-control border border-amber-200 bg-amber-50 px-3 py-2 text-sm text-amber-800 dark:border-amber-700/40 dark:bg-amber-900/20 dark:text-amber-200"
        >
          <span>{{ t('admin.providers.listPendingSyncHint') }}</span>
          <button class="btn btn-secondary px-2 py-1 text-xs" @click="syncPendingListChanges">
            {{ t('admin.providers.listPendingSyncAction') }}
          </button>
        </div>
        <ProviderBulkActionsBar
          class="mt-2"
          :selected-ids="selIds"
          :usage-loading="bulkUsageLoading"
          :upstream-usage-loading="upstreamUsageBulkLoading"
          :total-results="pagination.total"
          :selecting-all="selectingAllResults"
          :all-results-selected="allResultsSelected"
          @delete="handleBulkDelete"
          @reset-status="handleBulkResetStatus"
          @refresh-token="handleBulkRefreshToken"
          @query-usage="handleBulkQueryUsage"
          @query-upstream-usage="handleBulkQueryUpstreamUsage"
          @edit-selected="openBulkEditSelected"
          @clear="clearSelection"
          @select-page="selectPage"
          @select-all-results="handleSelectAllResults"
          @toggle-schedulable="handleBulkToggleSchedulable"
        />
      </template>
      <template #table>
        <div ref="providerTableRef" class="flex min-h-0 flex-1 flex-col overflow-hidden">
          <DataTable
            density="compact"
            :column-order-storage-key="columnOrderStorageKey"
            :initial-column-order="initialColumnOrder"
            ref="dataTableRef"
            :columns="cols"
            :data="providers"
            :loading="loading"
            row-key="id"
            :server-side-sort="true"
            @sort="handleSort"
            default-sort-key="name"
            default-sort-order="asc"
            :sort-storage-key="PROVIDER_SORT_STORAGE_KEY"
            :estimate-row-height="96"
            :overscan="5"
            :virtualize-threshold="50"
          >
            <template #header-select>
              <input
                type="checkbox"
                class="h-4 w-4 cursor-pointer rounded-compact border-gray-300 text-primary-600 focus:ring-primary-500"
                :checked="allVisibleSelected"
                :aria-label="t('admin.providers.compact.selectPage')"
                @click.stop
                @change="toggleSelectAllVisible($event)"
              />
            </template>
            <template #cell-select="{ row }">
              <input
                type="checkbox"
                :checked="isSelected(row.id)"
                :aria-label="t('admin.providers.compact.selectProvider', { name: row.name })"
                @change="toggleSel(row.id)"
                class="h-4 w-4 cursor-pointer rounded-compact border-gray-300 text-primary-600 focus:ring-primary-500"
              />
            </template>
            <template #cell-identity="{ row }">
              <div class="min-w-0 text-xs leading-4">
                <HelpTooltip
                  v-if="providerHomepageUrl(row)"
                  :content="providerHomepageUrl(row)"
                  width-class="w-max max-w-sm break-all"
                  class="-ml-1 max-w-full self-start"
                >
                  <template #trigger>
                    <a
                      :href="providerHomepageUrl(row)"
                      target="_blank"
                      rel="noopener noreferrer"
                      class="block min-w-0 truncate font-medium text-link"
                      :title="row.name"
                      >{{ row.name }}</a
                    >
                  </template>
                </HelpTooltip>
                <ProviderCellDetails v-else :label="row.name">
                  <template #trigger
                    ><span class="truncate font-medium">{{ row.name }}</span></template
                  >
                  <p class="whitespace-pre-wrap break-words">{{ row.name }}</p>
                </ProviderCellDetails>
                <div class="flex min-w-0 items-center gap-1 text-gray-500 dark:text-gray-400">
                  <span class="truncate font-mono">#{{ row.id }}</span>
                  <button
                    type="button"
                    class="shrink-0 rounded-compact px-0.5 hover:text-primary-600 focus-visible:outline focus-visible:outline-primary-600"
                    :aria-label="t('admin.providers.compact.copyId', { id: row.id })"
                    :title="t('admin.providers.compact.copyId', { id: row.id })"
                    @click="copyToClipboard(String(row.id))"
                    ><Icon name="copy" size="xs"
                  /></button>
                </div>
                <div v-if="providerDisplayEmail(row)" class="min-w-0">
                  <ProviderCellDetails :label="providerDisplayEmail(row)">
                    <template #trigger
                      ><span class="truncate text-gray-500 dark:text-gray-400">{{
                        providerDisplayEmail(row)
                      }}</span></template
                    >
                    <p class="break-all">{{ providerDisplayEmail(row) }}</p>
                    <p v-if="row.parent_chatgpt_account_id" class="break-all">{{
                      row.parent_chatgpt_account_id
                    }}</p>
                  </ProviderCellDetails>
                </div>
                <div v-if="row.notes" class="min-w-0">
                  <ProviderCellDetails :label="t('admin.providers.columns.notes')">
                    <template #trigger>
                      <span
                        class="break-all text-left text-gray-600 dark:text-gray-300"
                        :class="providerDisplayEmail(row) ? 'line-clamp-1' : 'line-clamp-2'"
                        >{{ row.notes }}</span
                      >
                    </template>
                    <p class="whitespace-pre-wrap break-words">{{ row.notes }}</p>
                  </ProviderCellDetails>
                </div>
              </div>
            </template>
            <template #cell-platform_type="{ row }">
              <div class="min-w-0 text-xs leading-4">
                <PlatformTypeBadge
                  class="provider-platform-summary max-w-full align-top"
                  :platform="row.platform"
                  :type="row.type"
                  :auth-mode="getOpenAIAuthMode(row)"
                  :plan-type="getProviderPlanType(row)"
                  :privacy-mode="row.extra?.privacy_mode || row.parent_privacy_mode"
                  :subscription-expires-at="
                    row.credentials?.subscription_expires_at || row.parent_subscription_expires_at
                  "
                />
                <div class="flex min-w-0 items-center gap-1">
                  <span class="min-w-0 truncate" :title="row.proxy?.name">{{
                    row.proxy?.name || t('admin.providers.compact.direct')
                  }}</span>
                  <span v-if="row.proxy?.country_code" class="shrink-0 text-gray-500">{{
                    row.proxy.country_code
                  }}</span>
                  <span
                    v-if="row.proxy?.expires_at"
                    class="min-w-0 truncate rounded-compact px-1"
                    :class="proxyExpiryBadge(row.proxy)"
                    :title="proxyExpiryText(row.proxy)"
                    >{{ proxyExpiryText(row.proxy) }}</span
                  >
                  <ProviderCellDetails :label="t('admin.providers.compact.connectionDetails')">
                    <template #trigger><Icon name="more" size="xs" /></template>
                    <div class="space-y-2">
                      <PlatformTypeBadge
                        :platform="row.platform"
                        :type="row.type"
                        :auth-mode="getOpenAIAuthMode(row)"
                        :plan-type="getProviderPlanType(row)"
                        :privacy-mode="row.extra?.privacy_mode || row.parent_privacy_mode"
                        :subscription-expires-at="
                          row.credentials?.subscription_expires_at ||
                          row.parent_subscription_expires_at
                        "
                      />
                      <div v-if="row.proxy" class="break-words">
                        {{ row.proxy.name }}
                        <span v-if="row.proxy.country_code"> · {{ row.proxy.country_code }}</span>
                        <p v-if="row.proxy.expires_at"
                          >{{ proxyExpiryText(row.proxy) }} ·
                          {{ formatDateTime(row.proxy.expires_at) }}</p
                        >
                      </div>
                      <p v-else>{{ t('admin.providers.compact.direct') }}</p>
                      <p v-if="row.proxy_fallback_origin_id" class="break-words">{{
                        fallbackOriginText(row)
                      }}</p>
                    </div>
                  </ProviderCellDetails>
                </div>
                <div v-if="row.proxy_fallback_origin_id" class="flex min-w-0 items-center gap-1">
                  <span
                    class="truncate rounded-compact bg-amber-100 px-1 text-amber-800 dark:bg-amber-900/30 dark:text-amber-300"
                    :title="fallbackOriginText(row)"
                    >{{ t('admin.providers.fallbackActive') }}</span
                  >
                  <button
                    type="button"
                    class="shrink-0 whitespace-nowrap rounded-compact px-1 text-link"
                    @click="onRevertFallback(row)"
                    >{{ t('admin.providers.revertProxy') }}</button
                  >
                </div>
              </div>
            </template>
            <template #header-status_schedule="{ column }">
              <div class="flex items-center">
                <span>{{ column.label }}</span>
                <HelpTooltip
                  :content="t('admin.providers.schedulerScore.hint')"
                  width-class="w-80"
                />
              </div>
            </template>
            <template #cell-status_schedule="{ row }">
              <ProviderStatusIndicator
                compact
                :provider="row"
                @show-temp-unsched="handleShowTempUnsched"
              >
                <template #controls>
                  <div
                    class="flex min-w-0 items-center gap-1 text-xs"
                    :data-test="'scheduler-score-' + row.id"
                  >
                    <Toggle
                      :model-value="row.schedulable"
                      size="sm"
                      :disabled="togglingSchedulable === row.id"
                      off-tone="soft"
                      :aria-label="t('admin.providers.columns.schedulable')"
                      :title="
                        row.schedulable
                          ? t('admin.providers.schedulableEnabled')
                          : t('admin.providers.schedulableDisabled')
                      "
                      @update:model-value="handleToggleSchedulable(row)"
                    />
                    <template v-if="getSchedulerScoreRows(row).length">
                      <span
                        class="min-w-0 truncate font-mono"
                        :title="formatSchedulerScoreRow(getSchedulerScoreRows(row)[0])"
                        >{{ formatSchedulerScoreRow(getSchedulerScoreRows(row)[0]) }}</span
                      >
                      <ProviderCellDetails :label="t('admin.providers.columns.schedulerScore')">
                        <template #trigger>
                          <span v-if="getSchedulerScoreRows(row).length > 1"
                            >+{{ getSchedulerScoreRows(row).length - 1 }}</span
                          >
                          <Icon v-else name="more" size="xs" />
                        </template>
                        <div class="mb-1 text-gray-500 dark:text-gray-400"
                          >{{ t('admin.providers.schedulerScore.baseShort') }} /
                          {{ t('admin.providers.schedulerScore.stickyShort') }}</div
                        >
                        <ul class="space-y-1 break-words font-mono">
                          <li
                            v-for="score in getSchedulerScoreRows(row)"
                            :key="String(score.group_id)"
                            >{{ formatSchedulerScoreRow(score) }}</li
                          >
                        </ul>
                      </ProviderCellDetails>
                    </template>
                    <span v-else class="text-gray-500">—</span>
                  </div>
                </template>
              </ProviderStatusIndicator>
            </template>
            <template #cell-capacity="{ row }">
              <ProviderCapacityCell compact :provider="row" />
            </template>
            <template #header-usage="{ column }">
              <div class="flex items-center">
                <span>{{ column.label }}</span>
                <HelpTooltip :content="t('admin.providers.usageWindowsHint')" width-class="w-72" />
              </div>
            </template>
            <template #cell-usage="{ row }">
              <ProviderUsageCell
                compact
                :provider="row"
                :today-stats-error="todayStatsError"
                :today-stats="todayStatsByProviderId[String(row.id)] ?? null"
                :today-stats-loading="todayStatsLoading"
                :manual-refresh-token="usageManualRefreshToken"
                :batched-usage="usageBatchByProviderId[String(row.id)] ?? null"
                :batched-usage-error="usageBatchErrorByProviderId[String(row.id)] ?? null"
                :batched-usage-loading="usageBatchLoadingByProviderId[String(row.id)] === true"
                :request-batched-usage="isDesktopViewport ? queueBatchedUsage : null"
                :upstream-usage="upstreamUsageByProviderId[String(row.id)] ?? null"
                :upstream-usage-error="upstreamUsageErrorByProviderId[String(row.id)] ?? null"
                :upstream-usage-loading="upstreamUsageLoadingByProviderId[String(row.id)] === true"
                :request-upstream-usage="requestUpstreamUsage"
                @provider-updated="handleProviderUpdated"
                @usage-loaded="handleProviderUsageLoaded(row.id, $event)"
              />
            </template>
            <template #cell-groups="{ row }">
              <div class="min-w-0 space-y-0.5 text-xs leading-4">
                <ProviderGroupsCell compact :groups="row.groups" />
                <div class="flex min-w-0 items-center gap-2 tabular-nums">
                  <span
                    class="truncate"
                    :title="t('admin.providers.columns.priority') + ' ' + row.priority"
                    >{{ t('admin.providers.compact.priority', { value: row.priority }) }}</span
                  >
                  <span
                    class="truncate"
                    :title="t('admin.providers.columns.billingRateMultiplier')"
                    >{{
                      t('admin.providers.compact.rate', {
                        value: (row.rate_multiplier ?? 1).toFixed(2),
                      })
                    }}</span
                  >
                </div>
              </div>
            </template>
            <template #cell-timestamps="{ row }">
              <div class="min-w-0 text-xs leading-4">
                <div
                  v-for="entry in providerTimestamps(row)"
                  :key="entry.key"
                  class="flex min-w-0 items-center gap-1"
                >
                  <span class="min-w-0 shrink truncate text-gray-500 dark:text-gray-400">{{
                    entry.label
                  }}</span>
                  <span class="min-w-0 truncate tabular-nums" :title="entry.full">{{
                    entry.summary || '—'
                  }}</span>
                </div>
                <div
                  v-if="row.last_used_at || row.created_at || row.expires_at"
                  class="flex min-w-0 items-center gap-1"
                >
                  <span
                    v-if="isExpired(row.expires_at)"
                    class="truncate text-amber-700 dark:text-amber-300"
                    >{{ t('admin.providers.expired') }}</span
                  >
                  <span
                    v-if="row.auto_pause_on_expired && row.expires_at"
                    class="truncate text-emerald-700 dark:text-emerald-300"
                    :title="t('admin.providers.autoPauseOnExpired')"
                    >{{ t('admin.providers.compact.autoPause') }}</span
                  >
                  <ProviderCellDetails :label="t('admin.providers.compact.timestamps')">
                    <template #trigger><Icon name="more" size="xs" /></template>
                    <dl class="space-y-1">
                      <div v-for="entry in providerTimestamps(row)" :key="entry.key">
                        <dt class="text-gray-500 dark:text-gray-400">{{ entry.label }}</dt>
                        <dd class="tabular-nums">{{ entry.full || '—' }}</dd>
                      </div>
                    </dl>
                    <p v-if="row.auto_pause_on_expired && row.expires_at">{{
                      t('admin.providers.autoPauseOnExpired')
                    }}</p>
                  </ProviderCellDetails>
                </div>
              </div>
            </template>
            <template #cell-actions="{ row }">
              <div class="flex items-center gap-1">
                <button
                  @click="handleEdit(row)"
                  class="flex flex-col items-center gap-0.5 rounded-control p-1.5 text-gray-500 transition-colors hover:bg-gray-100 hover:text-primary-600 dark:hover:bg-dark-700 dark:hover:text-primary-400"
                >
                  <Icon name="edit" size="sm" class="h-4 w-4" />
                  <span class="text-xs">{{ t('common.edit') }}</span>
                </button>
                <button
                  @click="openMenu(row, $event)"
                  class="flex flex-col items-center gap-0.5 rounded-control p-1.5 text-gray-500 transition-colors hover:bg-gray-100 hover:text-gray-900 dark:hover:bg-dark-700 dark:hover:text-white"
                >
                  <Icon name="more" size="sm" class="h-4 w-4" />
                  <span class="text-xs">{{ t('common.more') }}</span>
                </button>
              </div>
            </template>
          </DataTable>
        </div>
      </template>
      <template #pagination
        ><Pagination
          v-if="pagination.total > 0"
          :page="pagination.page"
          :total="pagination.total"
          :page-size="pagination.page_size"
          @update:page="handlePageChange"
          @update:pageSize="handlePageSizeChange"
      /></template>
    </TablePageLayout>
    <CreateProviderModal
      :show="activeModal?.kind === 'create'"
      :initial-platform="activeModal?.kind === 'create' ? activeModal.initialPlatform : undefined"
      :proxies="proxies"
      :groups="groups"
      @close="closeActiveModal"
      @created="reload"
    />
    <EditProviderModal
      :show="activeModal?.kind === 'edit'"
      :provider="activeModal?.kind === 'edit' ? activeModal.provider : null"
      :proxies="proxies"
      :groups="groups"
      @close="closeActiveModal"
      @updated="handleProviderUpdated"
    />
    <ReAuthProviderModal
      :show="activeModal?.kind === 'reauth'"
      :provider="activeModal?.kind === 'reauth' ? activeModal.provider : null"
      @close="closeReAuthModal"
      @reauthorized="handleProviderUpdated"
    />
    <ProviderTestModal
      :show="activeModal?.kind === 'test'"
      :provider="activeModal?.kind === 'test' ? activeModal.provider : null"
      :initial-group-id="Number(params.group) || undefined"
      @close="closeTestModal"
    />
    <ProviderStatsModal
      :show="activeModal?.kind === 'stats'"
      :provider="activeModal?.kind === 'stats' ? activeModal.provider : null"
      @close="closeStatsModal"
    />
    <AdvancedSchedulerScoreModal
      :show="activeModal?.kind === 'advancedSchedulerScore'"
      :provider="activeModal?.kind === 'advancedSchedulerScore' ? activeModal.provider : null"
      @close="closeAdvancedSchedulerScoreModal"
    />
    <CodexInviteResetModal
      :show="activeModal?.kind === 'inviteReset'"
      :provider="activeModal?.kind === 'inviteReset' ? activeModal.provider : null"
      @close="closeInviteResetModal"
      @updated="enterAutoRefreshSilentWindow"
    />
    <ScheduledTestsPanel
      :show="activeModal?.kind === 'schedule'"
      :provider-id="activeModal?.kind === 'schedule' ? activeModal.provider.id : null"
      :quality-supported="
        activeModal?.kind === 'schedule' && activeModal.provider.platform === 'openai'
      "
      :model-options="scheduleModelOptions"
      @close="closeSchedulePanel"
    />
    <ProviderActionMenu
      :show="menu.show"
      :provider="menu.acc"
      :position="menu.pos"
      @close="menu.show = false"
      @test="handleTest"
      @stats="handleViewStats"
      @advanced-scheduler-score="handleAdvancedSchedulerScore"
      @schedule="handleSchedule"
      @duplicate="handleDuplicateProvider"
      @reauth="handleReAuth"
      @refresh-token="handleRefresh"
      @recover-state="handleRecoverState"
      @reset-quota="handleResetQuota"
      @set-privacy="handleSetPrivacy"
      @invite-reset="handleInviteReset"
      @create-spark-shadow="handleCreateSparkShadow"
      @delete="handleDelete"
    />
    <SyncFromCrsModal
      :show="activeModal?.kind === 'sync'"
      @close="closeActiveModal"
      @synced="handleExternalProvidersChanged"
    />
    <ImportDataModal
      :show="activeModal?.kind === 'importData'"
      @close="closeActiveModal"
      @imported="handleDataImported"
    />
    <BulkEditProviderModal
      :show="activeModal?.kind === 'bulkEdit'"
      :provider-ids="selIds"
      :selected-providers="visibleSelectedProviders"
      :selected-platforms="selPlatforms"
      :selected-types="selTypes"
      :target="activeModal?.kind === 'bulkEdit' ? activeModal.target : undefined"
      :proxies="proxies"
      :groups="groups"
      @close="closeActiveModal"
      @updated="handleBulkUpdated"
    />
    <TempUnschedStatusModal
      :show="activeModal?.kind === 'tempUnsched'"
      :provider="activeModal?.kind === 'tempUnsched' ? activeModal.provider : null"
      @close="closeActiveModal"
      @reset="handleTempUnschedReset"
    />
    <ConfirmDialog
      :show="activeModal?.kind === 'delete'"
      :title="t('admin.providers.deleteProvider')"
      :message="
        t('admin.providers.deleteConfirm', {
          name: activeModal?.kind === 'delete' ? activeModal.provider.name : undefined,
        })
      "
      :confirm-text="t('common.delete')"
      :cancel-text="t('common.cancel')"
      :danger="true"
      @confirm="confirmDelete"
      @cancel="closeActiveModal"
    />
    <ConfirmDialog
      :show="activeModal?.kind === 'createShadow'"
      :title="t('admin.providers.createSparkShadow')"
      :message="
        t('admin.providers.createSparkShadowConfirm', {
          name: activeModal?.kind === 'createShadow' ? activeModal.provider.name : undefined,
        })
      "
      @confirm="confirmCreateSparkShadow"
      @cancel="closeActiveModal"
    />
    <ConfirmDialog
      :show="activeModal?.kind === 'exportData'"
      :title="t('admin.providers.dataExport')"
      :message="t('admin.providers.dataExportConfirmMessage')"
      :confirm-text="t('admin.providers.dataExportConfirm')"
      :cancel-text="t('common.cancel')"
      @confirm="handleExportData"
      @cancel="closeActiveModal"
    >
      <label class="flex items-center gap-2 text-sm text-gray-700 dark:text-gray-300">
        <input
          type="checkbox"
          class="h-4 w-4 rounded-compact border-gray-300 text-primary-600 focus:ring-primary-500"
          v-model="includeProxyOnExport"
        />
        <span>{{ t('admin.providers.dataExportIncludeProxies') }}</span>
      </label>
    </ConfirmDialog>
    <ErrorPassthroughRulesModal
      :show="activeModal?.kind === 'errorPassthrough'"
      @close="closeActiveModal"
    />
    <TLSFingerprintProfilesModal
      :show="activeModal?.kind === 'tlsProfiles'"
      @close="closeActiveModal"
    />
    <TLSFingerprintRoutersModal
      :show="activeModal?.kind === 'tlsRouters'"
      @close="closeActiveModal"
    />
    <TotpStepUpDialog :controller="providerExportStepUp" />
  </AppLayout>
</template>

<script setup lang="ts">
import { useProviderList } from './useProviderList'

import { useProviderUpstreamUsage } from './useProviderUpstreamUsage'
import { useProviderUsage } from './useProviderUsage'
import { useProviderSelection, type ProviderBulkEditTarget } from './useProviderSelection'

import MotionTransition from '@/components/common/MotionTransition.vue'
import { ref, reactive, computed, nextTick, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { adminAPI } from '@/api/admin'
import { supportsUpstreamUsageQuery } from '@/utils/upstreamUsage'
import { downloadJsonBlob, formatExportTimestamp } from '@/utils/download'
import { useDenseTablePreferences } from '@/composables/useDenseTablePreferences'
import { useClipboard } from '@/composables/useClipboard'
import { useSwipeSelect, type SwipeSelectVirtualContext } from '@/composables/useSwipeSelect'
import {
  useStepUp,
  isStepUpBlocked,
  isStepUpCancelled,
  stepUpBlockReason,
} from '@/composables/useStepUp'
import TotpStepUpDialog from '@/components/auth/TotpStepUpDialog.vue'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import DataTable from '@/components/common/table/DataTable.vue'
import { getColumnSortOptions, type Column } from '@/components/common/table/types'
import Toggle from '@/components/common/Toggle.vue'
import HelpTooltip from '@/components/common/HelpTooltip.vue'
import Pagination from '@/components/common/Pagination.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import CreateProviderModal from './create/CreateProviderModal.vue'
import EditProviderModal from './edit/EditProviderModal.vue'
import BulkEditProviderModal from './operations/BulkEditProviderModal.vue'
import SyncFromCrsModal from './operations/SyncFromCrsModal.vue'
import TempUnschedStatusModal from './operations/TempUnschedStatusModal.vue'
import ProviderTableActions from '@/views/admin/providers/table/ProviderTableActions.vue'
import ProviderTableFilters from '@/views/admin/providers/table/ProviderTableFilters.vue'
import ProviderBulkActionsBar from '@/views/admin/providers/operations/ProviderBulkActionsBar.vue'
import ProviderActionMenu from '@/views/admin/providers/table/ProviderActionMenu.vue'
import ImportDataModal from '@/views/admin/providers/operations/ImportDataModal.vue'
import ReAuthProviderModal from '@/views/admin/providers/operations/ReAuthProviderModal.vue'
import ProviderTestModal from '@/views/admin/providers/testing/ProviderTestModal.vue'
import ProviderStatsModal from '@/views/admin/providers/operations/ProviderStatsModal.vue'
import AdvancedSchedulerScoreModal from '@/components/admin/provider/AdvancedSchedulerScoreModal.vue'
import CodexInviteResetModal from '@/views/admin/providers/operations/CodexInviteResetModal.vue'
import ScheduledTestsPanel from '@/views/admin/providers/testing/ScheduledTestsPanel.vue'
import type { SelectOption } from '@/components/common/Select.vue'
import ProviderStatusIndicator from '@/views/admin/providers/table/ProviderStatusIndicator.vue'
import ProviderUsageCell from '@/views/admin/providers/table/ProviderUsageCell.vue'
import { resolveAntigravityPlanType } from '@/views/admin/providers/table/providerUsagePresentation'
import ProviderCellDetails from './table/ProviderCellDetails.vue'
import ProviderGroupsCell from '@/views/admin/providers/table/ProviderGroupsCell.vue'
import ProviderCapacityCell from '@/views/admin/providers/table/ProviderCapacityCell.vue'
import PlatformTypeBadge from '@/components/common/PlatformTypeBadge.vue'
import Icon from '@/components/icons/Icon.vue'
import ErrorPassthroughRulesModal from '@/views/admin/providers/operations/ErrorPassthroughRulesModal.vue'
import TLSFingerprintProfilesModal from '@/views/admin/providers/operations/TLSFingerprintProfilesModal.vue'
import TLSFingerprintRoutersModal from '@/views/admin/providers/operations/TLSFingerprintRoutersModal.vue'
import { formatCompactDateTimeParts, formatDateTime, formatRelativeTime } from '@/utils/format'
import { proxyExpiryBadgeClass, proxyExpiryLabelKey } from '@/utils/proxyExpiry'
import { sanitizeUrl } from '@/utils/url'
import { getFloatingPanelPosition } from '@/utils/floatingPanel'
import { MIN_COMFORTABLE_PANEL_HEIGHT } from '@/constants/overlay'
import type {
  Provider,
  ProviderPlatform,
  ProviderSchedulerGroupScore,
} from '@/api/admin/providers.types'
import type { Proxy as ProviderProxy } from '@/api/admin/proxies.types'
import type { AdminGroup } from '@/api/groups.types'
import type { ClaudeModel } from '@/api/models.types'

// ProvidersView.vue assembles the administrator provider table, its dialogs,
// and the actions that change displayed provider state. List loading, checked
// IDs, daily usage, and upstream-query caches each have a controller, while
// the page connects their results to table cells and operation dialogs.
// Provider updates can remove rows from the active filters and leave a manual
// synchronization notice for the remaining result set.
//
// Local changes invalidate upstream observations before patching visible rows.
// Incremental replacements update references held by open dialogs, and refresh
// admission pauses around menus, dialogs, and recent edits. Export runs through
// step-up authorization, and bulk actions retain failed selections when the
// server supplies enough detail for another attempt.

// The translator and application store supply labels and notifications for page
// actions, while domain controllers retain their own request state.
const { t } = useI18n()
const appStore = useAppStore()

// proxies and groups supply modal choices loaded independently after the first
// list request starts, allowing either metadata source to fail separately.
const proxies = ref<ProviderProxy[]>([])
const groups = ref<AdminGroup[]>([])
// providerTableRef and dataTableRef connect swipe selection to the rendered
// table and its virtualized row order.
const providerTableRef = ref<HTMLElement | null>(null)
const dataTableRef = ref<InstanceType<typeof DataTable> | null>(null)
// ProviderActiveModal identifies the single open page dialog and its bound target
// so interval refresh pausing and row replacement track every modal uniformly.
type ProviderActiveModal =
  | { kind: 'create'; initialPlatform?: ProviderPlatform }
  | { kind: 'edit'; provider: Provider }
  | { kind: 'reauth'; provider: Provider }
  | { kind: 'test'; provider: Provider }
  | { kind: 'stats'; provider: Provider }
  | { kind: 'advancedSchedulerScore'; provider: Provider }
  | { kind: 'inviteReset'; provider: Provider }
  | { kind: 'schedule'; provider: Provider }
  | { kind: 'tempUnsched'; provider: Provider }
  | { kind: 'delete'; provider: Provider }
  | { kind: 'createShadow'; provider: Provider }
  | { kind: 'bulkEdit'; target?: ProviderBulkEditTarget }
  | { kind: 'sync' }
  | { kind: 'importData' }
  | { kind: 'exportData' }
  | { kind: 'errorPassthrough' }
  | { kind: 'tlsProfiles' }
  | { kind: 'tlsRouters' }

// activeModal stores the currently displayed dialog and its target record, while
// includeProxyOnExport and scheduleModelOptions hold companion dialog drafts.
const activeModal = ref<ProviderActiveModal | null>(null)
const includeProxyOnExport = ref(true)
const scheduleModelOptions = ref<SelectOption[]>([])

// closeActiveModal dismisses the active page dialog and clears its bound target.
const closeActiveModal = () => {
  activeModal.value = null
}
// togglingSchedulable marks the row whose scheduling request is pending, while
// menu retains the provider and coordinates used by the open action menu.
const togglingSchedulable = ref<number | null>(null)
const menu = reactive<{
  show: boolean // Action menu visibility.
  acc: Provider | null // Selected provider row.
  pos: {
    top: number // px
    left: number // px
  } | null // Viewport pixel coordinates.
}>({ show: false, acc: null, pos: null })
// exportingData blocks duplicate downloads while step-up authorization and the
// provider export request are still pending.
const exportingData = ref(false)
// openCreateProvider opens creation with an optional initial platform
// selection. The platform is stored before the dialog becomes visible, allowing
// platform-specific entry points to initialize the same creation form.
const openCreateProvider = (platform?: ProviderPlatform) => {
  activeModal.value = { kind: 'create', initialPlatform: platform }
}

// The provider-tools and refresh dropdown refs associate each visible toolbar
// panel with its trigger and outside-click container.
const showProviderToolsDropdown = ref(false)
const providerToolsDropdownRef = ref<HTMLElement | null>(null)
const providerToolsButtonRef = ref<HTMLElement | null>(null)
// providerToolsDropdownPosition retains the shared floating-panel result, and
// providerToolsDropdownStyle converts nullable vertical anchors into the CSS
// values consumed by the toolbar.
const providerToolsDropdownPosition = reactive({
  top: null as number | null,
  bottom: null as number | null,
  left: 16,
  width: 320,
  maxHeight: 0,
})
const providerToolsDropdownStyle = computed(() => ({
  top:
    providerToolsDropdownPosition.top == null ? 'auto' : `${providerToolsDropdownPosition.top}px`,
  bottom:
    providerToolsDropdownPosition.bottom == null
      ? 'auto'
      : `${providerToolsDropdownPosition.bottom}px`,
  left: `${providerToolsDropdownPosition.left}px`,
  width: `${providerToolsDropdownPosition.width}px`,
}))
// Migrated visibility is ready before list sorting or column order is restored.
const {
  toggleColumn: togglePersistedColumn,
  isColumnVisible,
  columnOrderStorageKey,
  initialColumnOrder,
} = useDenseTablePreferences('providers')
const { copyToClipboard } = useClipboard()

// allColumns carries the desktop width budget and the server keys available in each header.
const allColumns = computed<Column[]>(() => [
  { key: 'select', label: '', width: '44px' },
  {
    key: 'identity',
    label: t('admin.providers.compact.identity'),
    width: '215px',
    subtitleSortLayout: 'inline',
    sortOptions: [
      { key: 'name', label: t('admin.providers.columns.name') },
      { key: 'id', label: 'ID' },
    ],
  },
  { key: 'platform_type', label: t('admin.providers.columns.platformType'), width: '155px' },
  {
    key: 'status_schedule',
    label: t('admin.providers.compact.statusSchedule'),
    width: '155px',
    subtitleSortLayout: 'subline',
    sortOptions: [
      { key: 'status', label: t('admin.providers.columns.status') },
      { key: 'schedulable', label: t('admin.providers.compact.schedule') },
    ],
  },
  { key: 'capacity', label: t('admin.providers.columns.capacity'), width: '195px' },
  { key: 'usage', label: t('admin.providers.columns.usageWindows'), width: '280px' },
  {
    key: 'groups',
    label: t('admin.providers.compact.groups'),
    width: '195px',
    subtitleSortLayout: 'subline',
    sortOptions: [
      { key: 'priority', label: t('admin.providers.columns.priority') },
      { key: 'rate_multiplier', label: t('admin.providers.compact.rateLabel') },
    ],
  },
  {
    key: 'timestamps',
    label: t('admin.providers.compact.timestamps'),
    width: '165px',
    subtitleSortLayout: 'subline',
    sortOptions: [
      { key: 'last_used_at', label: t('admin.providers.compact.lastUsed') },
      { key: 'created_at', label: t('admin.providers.compact.created') },
      { key: 'expires_at', label: t('admin.providers.compact.expires') },
    ],
  },
  { key: 'actions', label: t('admin.providers.columns.actions'), width: '106px' },
])

// Structural cells and identity remain available even with custom preferences.
const toggleableColumns = computed(() =>
  allColumns.value.filter((column) => !['select', 'identity', 'actions'].includes(column.key)),
)
const cols = computed(() => allColumns.value.filter((column) => isColumnVisible(column.key)))
const visibleSortKeys = computed(
  () => new Set(cols.value.flatMap((column) => getColumnSortOptions(column).map(({ key }) => key))),
)

// Auto refresh settings
// The refresh dropdown stores its trigger, containment root, and computed
// fixed-position style independently of the provider-tools dropdown geometry.
const showAutoRefreshDropdown = ref(false)
const autoRefreshDropdownRef = ref<HTMLElement | null>(null)
const autoRefreshButtonRef = ref<HTMLElement | null>(null)
const autoRefreshDropdownStyle = ref<Record<string, string>>({})
// autoRefreshButtonTitle selects either the plain action label or the active
// countdown label from the list controller state.
const autoRefreshButtonTitle = computed(() => {
  if (!autoRefreshEnabled.value) return t('admin.providers.autoRefresh')
  return t('admin.providers.autoRefreshCountdown', { seconds: autoRefreshCountdown.value })
})

// autoRefreshIntervalLabel returns the translated label for a supported refresh
// duration. Other numeric values use a seconds suffix so an unexpected interval
// can still be displayed by the control.
const autoRefreshIntervalLabel = (sec: number) => {
  if (sec === 5) return t('admin.providers.refreshInterval5s')
  if (sec === 10) return t('admin.providers.refreshInterval10s')
  if (sec === 15) return t('admin.providers.refreshInterval15s')
  if (sec === 30) return t('admin.providers.refreshInterval30s')
  return `${sec}s`
}

// formatSchedulerScore preserves zero and shows a dash for absent or invalid scores.
const formatSchedulerScore = (value: unknown): string => {
  if (value == null || value === '') return '—'
  const num = Number(value)
  if (!Number.isFinite(num)) return '—'
  return num.toFixed(6).replace(/\.?0+$/, '')
}

// formatStickySchedulerScore displays a sticky score or its infinity marker.
// Missing records use a dash, while finite values share the scheduler-score
// formatting used by the surrounding table.
const formatStickySchedulerScore = (score: ProviderSchedulerGroupScore): string => {
  if (!score) return '-'
  if (score.sticky_score_infinity) return '+∞'
  return formatSchedulerScore(score.sticky_score)
}

// getSchedulerScoreRows returns group-specific scheduler scores when any are
// available. Otherwise it wraps the base score as an ungrouped entry, or
// returns an empty list when the provider has no score.
const getSchedulerScoreRows = (provider: Provider): ProviderSchedulerGroupScore[] => {
  const scores = Array.isArray(provider.scheduler_scores) ? provider.scheduler_scores : []
  const groupRows = scores.filter((score) => score.group_id != null)
  if (groupRows.length) return groupRows
  const ungrouped = scores.find((score) => score.group_id == null)
  if (ungrouped) return [ungrouped]
  // Ungrouped providers can expose a base score without group rows, so wrap that result for the same table display.
  if (provider.scheduler_score) {
    return [{ group_id: null, ...provider.scheduler_score }]
  }
  return []
}

// formatSchedulerScoreGroup labels a score with its group name when available.
// Otherwise it displays a numeric group ID or the translated ungrouped label
// for a base score.
const formatSchedulerScoreGroup = (score: ProviderSchedulerGroupScore): string => {
  if ('group_name' in score && score.group_name) return score.group_name
  if ('group_id' in score && score.group_id != null) return `#${score.group_id}`
  return t('admin.providers.schedulerScore.ungrouped')
}

// formatSchedulerScoreRow includes the real group and both scheduling scores.
function formatSchedulerScoreRow(score: ProviderSchedulerGroupScore): string {
  return [
    formatSchedulerScoreGroup(score),
    formatSchedulerScore(score.base_score),
    formatStickySchedulerScore(score),
  ].join(' / ')
}

// clampDropdownLeft keeps a dropdown inside the viewport with a sixteen-pixel
// margin. When the dropdown is wider than the available space, its left edge
// remains at that margin.
const clampDropdownLeft = (left: number, width: number) => {
  const margin = 16
  return Math.min(Math.max(left, margin), Math.max(margin, window.innerWidth - width - margin))
}

// buildTopDropdownStyle positions a fixed dropdown below its trigger and limits
// its height to available viewport space. Missing triggers return an empty
// style; callers choose left or right alignment before horizontal clamping.
const buildTopDropdownStyle = (
  trigger: HTMLElement | null,
  width: number,
  align: 'left' | 'right' = 'right',
): Record<string, string> => {
  if (!trigger) return {}
  const rect = trigger.getBoundingClientRect()
  // The fixed toolbar menu follows its trigger rectangle so scrolling containers cannot clip or displace its viewport-relative placement.
  const rawLeft = align === 'left' ? rect.left : rect.right - width
  const left = clampDropdownLeft(rawLeft, width)
  return {
    top: `${rect.bottom + 8}px`,
    left: `${left}px`,
    maxHeight: `${Math.max(MIN_COMFORTABLE_PANEL_HEIGHT, window.innerHeight - rect.bottom - 24)}px`,
  }
}

// updateTopDropdownPositions recomputes geometry for whichever toolbar
// dropdowns are open. Provider tools use the shared floating-panel placement,
// while the refresh menu stays aligned with its own trigger.
const updateTopDropdownPositions = () => {
  if (showProviderToolsDropdown.value) {
    const trigger = providerToolsButtonRef.value
    if (trigger) {
      Object.assign(
        providerToolsDropdownPosition,
        getFloatingPanelPosition(
          trigger.getBoundingClientRect(),
          document.documentElement.clientWidth || window.innerWidth,
          window.innerHeight,
        ),
      )
    }
  }
  if (showAutoRefreshDropdown.value) {
    autoRefreshDropdownStyle.value = buildTopDropdownStyle(autoRefreshButtonRef.value, 224, 'left')
  }
}

// toggleProviderToolsDropdown toggles provider tools and closes the refresh
// dropdown. It recomputes open-menu geometry after changing visibility so the
// resulting panel uses the current trigger position.
const toggleProviderToolsDropdown = () => {
  showProviderToolsDropdown.value = !showProviderToolsDropdown.value
  showAutoRefreshDropdown.value = false
  updateTopDropdownPositions()
}

// toggleAutoRefreshDropdown toggles refresh settings and closes provider tools.
// It recomputes open-menu geometry after the visibility change so the panel
// follows the current refresh button position.
const toggleAutoRefreshDropdown = () => {
  showAutoRefreshDropdown.value = !showAutoRefreshDropdown.value
  showProviderToolsDropdown.value = false
  updateTopDropdownPositions()
}

// Visibility controls both list projection and the daily statistics lifecycle.
const toggleColumn = (key: string) => {
  const wasHidden = togglePersistedColumn(key)
  if (wasHidden === null) return

  if (key === 'usage') setTodayStatsVisible(wasHidden)

  void reconcileColumnVisibility(key)?.catch((error) => {
    console.error('Failed to reload providers after toggling a column:', error)
  })
}

// The list controller loads rows and invokes these callbacks at reconciliation
// points; callbacks resolve selection, usage, and modal state when requests
// complete.
const {
  PROVIDER_SORT_STORAGE_KEY,
  providers,
  loading,
  params,
  pagination,
  sortState,
  hasPendingListSync,
  autoRefreshIntervals,
  autoRefreshEnabled,
  autoRefreshIntervalSeconds,
  autoRefreshCountdown,
  reconcileColumnVisibility,
  setTodayStatsVisible,
  setAutoRefreshEnabled,
  setAutoRefreshInterval,
  load,
  reload,
  debouncedReload,
  handlePageChange,
  handlePageSizeChange,
  handleSort,
  enterAutoRefreshSilentWindow,
  pauseAutoRefresh,
  resumeAutoRefresh,
} = useProviderList({
  includeSchedulerScore: () => isColumnVisible('status_schedule'),
  isSortKeyVisible: (key) => visibleSortKeys.value.has(key),
  invalidateTodayStats: () => invalidateTodayStats(),
  clearSelection: () => clearSelection(),
  onLoaded: () => hydrateUpstreamUsageCache(),
  refreshTodayStats: () => refreshTodayStatsBatch(),
  onRowMissing: (provider) => invalidateUpstreamUsageCache(provider.id),
  onRowReplaced: (current, next) => {
    if (
      (supportsUpstreamUsageQuery(current) || supportsUpstreamUsageQuery(next)) &&
      upstreamUsageCacheKey(current) !== upstreamUsageCacheKey(next)
    )
      invalidateUpstreamUsageCache(next.id)
    syncProviderRefs(next)
  },
  isRefreshPaused: () =>
    isAnyModalOpen.value ||
    menu.show ||
    showProviderToolsDropdown.value ||
    showAutoRefreshDropdown.value,
})

// The selection controller supplies checked-ID operations and stable bulk
// targets, while swipe selection below translates table gestures into those
// operations.
const {
  selIds,
  allVisibleSelected,
  isSelected,
  setSelectedIds,
  select,
  deselect,
  toggleSel,
  removeSelectedProviders,
  toggleVisible,
  batchUpdate,
  selectingAllResults,
  allResultsSelected,
  clearSelection,
  selectPage,
  selPlatforms,
  selTypes,
  buildBulkEditFilterSnapshot,
  handleSelectAllResults,
  loadBulkEditFilterMetadata,
} = useProviderSelection(providers, pagination, params)
// The upstream controller publishes validated observations and exposes
// invalidation methods used before provider mutations can make cached
// credentials or query settings obsolete.
const {
  upstreamUsageByProviderId,
  upstreamUsageErrorByProviderId,
  upstreamUsageLoadingByProviderId,
  upstreamUsageBulkLoading,
  upstreamUsageCacheKey,
  invalidateUpstreamUsageCache,
  invalidateAllUpstreamUsageCache,
  hydrateUpstreamUsageCache,
  requestUpstreamUsage,
  handleBulkQueryUpstreamUsage,
} = useProviderUpstreamUsage(providers, selIds)
// The usage controller supplies daily metrics, desktop batching, and
// manual-refresh tokens consumed by the provider usage cells.
const {
  todayStatsByProviderId,
  todayStatsLoading,
  todayStatsError,
  usageManualRefreshToken,
  isDesktopViewport,
  usageBatchByProviderId,
  usageBatchErrorByProviderId,
  usageBatchLoadingByProviderId,
  handleProviderUsageLoaded,
  queueBatchedUsage,
  refreshTodayStatsBatch,
  invalidateTodayStats,
  bulkUsageLoading,
  handleBulkQueryUsage,
} = useProviderUsage(providers, selIds, () => isColumnVisible('usage'))

// swipeVirtualContext reads the virtualizer and sorted row order at gesture
// time, so checked IDs follow the displayed rows rather than the unsorted API
// response.
const swipeVirtualContext: SwipeSelectVirtualContext = {
  // getVirtualizer reads the table instance when swipe selection begins.
  getVirtualizer: () => dataTableRef.value?.virtualizer ?? null,
  // getSortedData returns the currently displayed provider order for swipe selection.
  // Before the table instance exists, it returns the loaded rows so early gestures still resolve provider IDs.
  getSortedData: () => dataTableRef.value?.sortedData ?? providers.value,
  // getRowId maps displayed provider records to their persistent selection identity.
  getRowId: (row: any) => row.id,
}

useSwipeSelect(
  providerTableRef,
  {
    isSelected,
    select,
    deselect,
    batchUpdate,
  },
  swipeVirtualContext,
)

// visibleSelectedProviders passes already-loaded current-page rows into bulk
// editing so prefill avoids issuing duplicate per-provider detail requests.
const visibleSelectedProviders = computed(() =>
  providers.value.filter((provider) => isSelected(provider.id)),
)

// isAnyModalOpen reports whether any page dialog is open, pausing automatic
// interval refreshes across every modal kind including shadow confirmation and
// scheduler diagnostics.
const isAnyModalOpen = computed(() => activeModal.value !== null)

// syncProviderRefs replaces matching provider references held by the active
// dialog and action menu while leaving other provider identities untouched.
const syncProviderRefs = (nextProvider: Provider) => {
  if (
    activeModal.value &&
    'provider' in activeModal.value &&
    activeModal.value.provider.id === nextProvider.id
  ) {
    activeModal.value = { ...activeModal.value, provider: nextProvider }
  }
  if (menu.acc?.id === nextProvider.id) menu.acc = nextProvider
}

// handleManualRefresh reloads the provider page and its daily statistics before
// advancing the usage refresh token. A failed load propagates and leaves the
// token unchanged for mounted usage cells.
const handleManualRefresh = async () => {
  await load()
  // Advance after the list and daily statistics finish so mounted usage cells
  // refetch observations against the provider rows accepted by this refresh.
  usageManualRefreshToken.value += 1
}

// closeProviderToolsDropdown hides the provider-tools panel after an action is
// selected.
const closeProviderToolsDropdown = () => {
  showProviderToolsDropdown.value = false
}

// openBulkEditFilteredFromMenu closes provider tools and starts the filtered
// bulk-edit flow.
const openBulkEditFilteredFromMenu = () => {
  closeProviderToolsDropdown()
  void openBulkEditFiltered()
}

// openSyncFromCrs closes provider tools and opens the CRS synchronization
// dialog.
const openSyncFromCrs = () => {
  closeProviderToolsDropdown()
  activeModal.value = { kind: 'sync' }
}

// openImportData closes provider tools and opens the provider data-import
// dialog.
const openImportData = () => {
  closeProviderToolsDropdown()
  activeModal.value = { kind: 'importData' }
}

// openExportDataDialogFromMenu closes provider tools and opens export with its
// defaults.
const openExportDataDialogFromMenu = () => {
  closeProviderToolsDropdown()
  openExportDataDialog()
}

// openErrorPassthrough closes provider tools and opens the error-passthrough
// rule editor.
const openErrorPassthrough = () => {
  closeProviderToolsDropdown()
  activeModal.value = { kind: 'errorPassthrough' }
}

// openTLSFingerprintProfiles closes provider tools and opens the TLS
// fingerprint profile editor.
const openTLSFingerprintProfiles = () => {
  closeProviderToolsDropdown()
  activeModal.value = { kind: 'tlsProfiles' }
}

// openTLSFingerprintRouters closes provider tools and opens the TLS fingerprint
// routing editor.
const openTLSFingerprintRouters = () => {
  closeProviderToolsDropdown()
  activeModal.value = { kind: 'tlsRouters' }
}

// syncPendingListChanges clears the pending-sync notice and reloads the current
// provider page. After a successful load it advances the usage token so mounted
// cells refresh their observations.
const syncPendingListChanges = async () => {
  hasPendingListSync.value = false
  await load()
  // Advance after pending list changes have loaded so mounted usage cells
  // replace their observations using the provider rows from the completed synchronization.
  usageManualRefreshToken.value += 1
}

// The Grok quota age and future-skew thresholds limit which observations can
// support the Heavy subscription label shown in the provider badge.
const GROK_QUOTA_SIGNAL_MAX_AGE_MS = 24 * 60 * 60 * 1000
const GROK_QUOTA_SIGNAL_MAX_FUTURE_SKEW_MS = 5 * 60 * 1000

// firstNonBlankString returns the first string containing non-whitespace
// characters among its inputs. It returns that string exactly as supplied, or
// undefined when no value qualifies for display.
function firstNonBlankString(...values: unknown[]): string | undefined {
  return values.find(
    (value): value is string => typeof value === 'string' && value.trim().length > 0,
  )
}

// normalizeGrokPlanKey converts a string plan label to its comparison form. It
// trims whitespace, lowercases characters, removes whitespace and separators,
// and returns an empty string for non-string values.
function normalizeGrokPlanKey(value: unknown): string {
  if (typeof value !== 'string') return ''
  return value
    .trim()
    .toLowerCase()
    .replace(/[\s_-]+/g, '')
}

// grokPersistedQuotaSnapshot returns the object-shaped usage snapshot when
// available. Otherwise it accepts the older quota snapshot, rejecting arrays
// and missing values so plan inference receives a usable record.
function grokPersistedQuotaSnapshot(extra: Record<string, any>): Record<string, any> | undefined {
  const usage = extra.grok_usage_snapshot
  if (usage && typeof usage === 'object' && !Array.isArray(usage)) {
    return usage as Record<string, any>
  }
  const legacy = extra.grok_quota_snapshot
  if (legacy && typeof legacy === 'object' && !Array.isArray(legacy)) {
    return legacy as Record<string, any>
  }
  return undefined
}

// isGrokQuotaTimestampFresh accepts parseable observations within the
// configured age and future-skew windows. Empty or invalid timestamps return
// false, preventing them from supporting a higher displayed Grok plan.
function isGrokQuotaTimestampFresh(raw: unknown): boolean {
  const value = String(raw || '').trim()
  if (!value) return false
  const observedAt = Date.parse(value)
  if (!Number.isFinite(observedAt)) return false
  const age = Date.now() - observedAt
  return age <= GROK_QUOTA_SIGNAL_MAX_AGE_MS && age >= -GROK_QUOTA_SIGNAL_MAX_FUTURE_SKEW_MS
}

// isGrok45ResponsesQuotaModel recognizes Grok 4.5 model identifiers after
// trimming and removing xAI prefixes. It accepts the exact version and suffixed
// variants while leaving other models ineligible for this plan signal.
function isGrok45ResponsesQuotaModel(model: unknown): boolean {
  const value = String(model || '')
    .trim()
    .toLowerCase()
    .replace(/^(x-ai|xai)\//, '')
  return value === 'grok-4.5' || value.startsWith('grok-4.5-')
}

// grokQuotaLooksHeavy checks whether observed limits reach the Heavy display
// thresholds. A request limit of at least 8300 or token limit of at least
// 53000000 qualifies; missing limits become zero.
function grokQuotaLooksHeavy(snapshot: Record<string, any> | undefined): boolean {
  const req = Number(snapshot?.requests?.limit ?? 0)
  const tok = Number(snapshot?.tokens?.limit ?? 0)
  // Either observed window can support the Heavy badge.
  return req >= 8300 || tok >= 53_000_000
}

// grok45ResponsesPlanIsHeavy combines a quota snapshot with the freshness and
// model rules used for plan display. A fresh persisted Heavy hint qualifies
// directly. Otherwise the snapshot must identify Grok 4.5, have a fresh
// observation timestamp, and reach a Heavy quota threshold.
//
// The observation timestamp prefers the last header sample over the snapshot
// update time. Keeping these conditions together prevents an old quota or
// another model from promoting a broad subscription label to Heavy.
function grok45ResponsesPlanIsHeavy(snapshot: Record<string, any> | undefined): boolean {
  if (!snapshot) return false
  const hint = normalizeGrokPlanKey(snapshot.plan_from_45_responses)
  if (hint === 'supergrokheavy' && isGrokQuotaTimestampFresh(snapshot.plan_from_45_responses_at)) {
    return true
  }
  const observedAt = snapshot.last_headers_seen_at || snapshot.updated_at
  return (
    isGrok45ResponsesQuotaModel(snapshot.model) &&
    isGrokQuotaTimestampFresh(observedAt) &&
    grokQuotaLooksHeavy(snapshot)
  )
}

// getProviderPlanType selects the subscription label displayed for a provider
// row. Non-Grok rows prefer their own plan credential and then the parent plan.
// Grok rows prioritize a specific credential tier, while the broad SuperGrokPro
// credential requires billing or fresh Grok 4.5 quota evidence to choose its
// display label.
//
// When that evidence supports Heavy, the row displays SuperGrok Heavy.
// Otherwise billing, usage snapshots, extra metadata, and parent values supply
// the remaining ordered choices, retaining unknown nonblank plan strings.
function getProviderPlanType(row: any): string | undefined {
  if (!row) return undefined
  if (row.platform === 'grok') {
    const extra = (row.extra || {}) as Record<string, any>
    const billing = extra.grok_billing_snapshot as Record<string, any> | undefined
    const usage = extra.grok_usage_snapshot as Record<string, any> | undefined
    const legacyQuota = extra.grok_quota_snapshot as Record<string, any> | undefined
    const quota = grokPersistedQuotaSnapshot(extra)
    const cred = firstNonBlankString(row.credentials?.subscription_tier)
    const credKey = normalizeGrokPlanKey(cred)
    if (credKey && credKey !== 'supergrokpro') {
      return cred
    }
    if (
      grok45ResponsesPlanIsHeavy(quota) &&
      (credKey === 'supergrokpro' ||
        normalizeGrokPlanKey(billing?.plan) === 'supergrok' ||
        normalizeGrokPlanKey(billing?.plan) === 'supergrokpro')
    ) {
      return 'SuperGrok Heavy'
    }
    if (credKey === 'supergrokpro') {
      return firstNonBlankString(billing?.plan) || 'SuperGrok'
    }
    return firstNonBlankString(
      billing?.plan,
      usage?.subscription_tier,
      legacyQuota?.subscription_tier,
      extra.subscription_tier,
      row.credentials?.plan_type,
      row.parent_plan_type,
    )
  }
  if (row.platform === 'antigravity') {
    return resolveAntigravityPlanType(row, usageBatchByProviderId.value[String(row.id)])
  }
  return firstNonBlankString(row.credentials?.plan_type, row.parent_plan_type)
}

// getOpenAIAuthMode returns a nonblank authentication mode for OpenAI OAuth
// rows. Other platforms, credential types, and missing modes return undefined
// so the badge can omit that detail.
function getOpenAIAuthMode(row: any): string | undefined {
  if (!row || row.platform !== 'openai' || row.type !== 'oauth') return undefined
  const authMode = row.credentials?.auth_mode
  return typeof authMode === 'string' && authMode.trim() ? authMode : undefined
}

// providerDisplayEmail chooses the email shown in the provider name cell.
// Provider extra metadata takes precedence over credentials, then a shadow
// provider can use its parent email before returning an empty string.
function providerDisplayEmail(row: any): string {
  return (
    row.extra?.email_address || row.extra?.email || row.credentials?.email || row.parent_email || ''
  )
}

// providerHomepageUrl returns the origin of a sanitized API-key forwarding
// address. Other credential types, missing addresses, and rejected URLs produce
// an empty link so the cell can omit the homepage action.
function providerHomepageUrl(row: Provider): string {
  if (row.type !== 'apikey' || typeof row.credentials?.base_url !== 'string') return ''
  const baseUrl = sanitizeUrl(row.credentials.base_url)
  return baseUrl ? new URL(baseUrl).origin : ''
}

// handleEdit selects the provider row and opens its editing dialog.
const handleEdit = (a: Provider) => {
  activeModal.value = { kind: 'edit', provider: a }
}
// openMenu opens the action menu for one provider and positions it near the
// trigger or pointer. Narrow viewports center the menu around the trigger and
// prefer space below it; desktop placement follows the pointer within viewport
// padding. Missing triggers use pointer coordinates.
//
// After Vue renders the menu, the function measures its actual height and
// clamps its top position again. The provider-ID check keeps that correction
// associated with the row whose menu was opened, retaining access to lower
// actions.
const openMenu = async (a: Provider, e: MouseEvent) => {
  menu.acc = a

  const target = e.currentTarget as HTMLElement
  if (target) {
    const rect = target.getBoundingClientRect()
    // These dimensions estimate the initial action menu before its
    // provider-specific rows render; the next-tick measurement corrects the
    // final vertical placement.
    const menuWidth = 208
    const menuHeight = 280
    const padding = 8
    const viewportWidth = window.innerWidth
    const viewportHeight = window.innerHeight

    let left: number
    let top: number

    if (viewportWidth < 768) {
      // Center the narrow-screen menu around its trigger.
      left = Math.max(
        padding,
        Math.min(rect.left + rect.width / 2 - menuWidth / 2, viewportWidth - menuWidth - padding),
      )

      // Prefer the space directly below the trigger.
      top = rect.bottom + 4

      // When the estimate exceeds the space below, place the menu above its trigger so lower actions remain reachable.
      if (top + menuHeight > viewportHeight - padding) {
        top = rect.top - menuHeight - 4
        // Clamp tall menus to the top viewport padding.
        if (top < padding) {
          top = padding
        }
      }
    } else {
      left = Math.max(padding, Math.min(e.clientX - menuWidth, viewportWidth - menuWidth - padding))
      top = e.clientY
      if (top + menuHeight > viewportHeight - padding) {
        top = viewportHeight - menuHeight - padding
      }
    }

    menu.pos = { top, left }
  } else {
    menu.pos = { top: e.clientY, left: e.clientX - 200 }
  }

  menu.show = true
  await nextTick()
  // Provider actions vary in count; measure the rendered menu before the final clamp so the bottom deletion action remains reachable.
  const menuElement = document.querySelector<HTMLElement>('.action-menu-content')
  if (menu.show && menu.acc?.id === a.id && menu.pos && menuElement) {
    menu.pos.top = Math.max(
      8,
      Math.min(menu.pos.top, window.innerHeight - menuElement.offsetHeight - 8),
    )
  }
}
// toggleSelectAllVisible applies the header checkbox state to the visible
// provider rows.
const toggleSelectAllVisible = (event: Event) => {
  const target = event.target as HTMLInputElement
  toggleVisible(target.checked)
}
// handleBulkDelete confirms deletion of a captured selected-ID list and
// invalidates its cached upstream observations before the request. Complete
// success clears selection. Partial success retains returned failed IDs, or the
// captured IDs when the response lacks that detail.
//
// The page reloads after either reported outcome so removed providers disappear
// and pagination reflects the server. Transport failures leave the selection
// available for another attempt and display the error. Cache invalidation
// occurs before deletion because credentials may become unavailable during the
// operation.
const handleBulkDelete = async () => {
  const providerIds = [...selIds.value]
  if (!confirm(t('admin.providers.bulkActions.confirmDelete', { count: providerIds.length })))
    return
  try {
    for (const providerID of providerIds) invalidateUpstreamUsageCache(providerID)
    const result = await adminAPI.providers.batchDelete(providerIds)
    if (result.failed > 0) {
      appStore.showError(
        t('admin.providers.bulkActions.partialSuccess', {
          success: result.success,
          failed: result.failed,
        }),
      )
      setSelectedIds(result.failed_ids?.length ? result.failed_ids : providerIds)
    } else {
      appStore.showSuccess(
        t('admin.providers.bulkActions.deleteSuccess', { count: result.success }),
      )
      clearSelection()
    }
    await reload()
  } catch (error) {
    console.error('Failed to bulk delete providers:', error)
    appStore.showError(String(error))
  }
}
// handleBulkResetStatus confirms and clears errors for the current selection.
// Complete success clears selection; partial failure retains it, and the page
// reloads after either reported result to display server state.
const handleBulkResetStatus = async () => {
  if (!confirm(t('common.confirm'))) return
  try {
    const result = await adminAPI.providers.batchClearError(selIds.value)
    if (result.failed > 0) {
      appStore.showError(
        t('admin.providers.bulkActions.partialSuccess', {
          success: result.success,
          failed: result.failed,
        }),
      )
    } else {
      appStore.showSuccess(
        t('admin.providers.bulkActions.resetStatusSuccess', { count: result.success }),
      )
      clearSelection()
    }
    reload()
  } catch (error) {
    console.error('Failed to bulk reset status:', error)
    appStore.showError(String(error))
  }
}
// handleBulkRefreshToken confirms credential refresh and invalidates selected
// upstream observations before requesting it. Complete success clears
// selection; partial failure retains it, and either reported result triggers a
// list reload.
const handleBulkRefreshToken = async () => {
  if (!confirm(t('common.confirm'))) return
  try {
    for (const providerID of selIds.value) invalidateUpstreamUsageCache(providerID)
    const result = await adminAPI.providers.batchRefresh(selIds.value)
    if (result.failed > 0) {
      appStore.showError(
        t('admin.providers.bulkActions.partialSuccess', {
          success: result.success,
          failed: result.failed,
        }),
      )
    } else {
      appStore.showSuccess(
        t('admin.providers.bulkActions.refreshTokenSuccess', { count: result.success }),
      )
      clearSelection()
    }
    reload()
  } catch (error) {
    console.error('Failed to bulk refresh token:', error)
    appStore.showError(String(error))
  }
}

// updateSchedulableInList replaces matching visible rows with the supplied
// scheduling flag. Empty IDs do nothing, and rows outside the target set retain
// their object identity and existing runtime fields.
const updateSchedulableInList = (providerIds: number[], schedulable: boolean) => {
  if (providerIds.length === 0) return
  const idSet = new Set(providerIds)
  providers.value = providers.value.map((provider) =>
    idSet.has(provider.id) ? { ...provider, schedulable } : provider,
  )
}
// normalizeBulkSchedulableResult interprets the response detail available for a
// bulk scheduling update. Explicit success and failure ID arrays take
// precedence, followed by per-provider result entries. Count-only responses
// identify all requested IDs as successful only when the success count equals
// the complete request and failure is zero.
//
// Other count-only responses retain their counts without guessing which rows
// changed. The caller uses the separate ID and count flags to decide whether it
// can patch rows, clear selection, or request fresh server state.
const normalizeBulkSchedulableResult = (
  result: {
    success?: number // Successful provider count.
    failed?: number // Failed provider count.
    success_ids?: number[] // Identified successful providers.
    failed_ids?: number[] // Identified failed providers.
    results?: Array<{
      provider_id: number // Returned provider ID.
      success: boolean // Individual operation result.
    }> // Per-provider operation outcomes.
  },
  providerIds: number[],
) => {
  const responseSuccessIds = Array.isArray(result.success_ids) ? result.success_ids : []
  const responseFailedIds = Array.isArray(result.failed_ids) ? result.failed_ids : []
  if (responseSuccessIds.length > 0 || responseFailedIds.length > 0) {
    return {
      successIds: responseSuccessIds,
      failedIds: responseFailedIds,
      successCount: typeof result.success === 'number' ? result.success : responseSuccessIds.length,
      failedCount: typeof result.failed === 'number' ? result.failed : responseFailedIds.length,
      hasIds: true,
      hasCounts: true,
    }
  }

  const results = Array.isArray(result.results) ? result.results : []
  if (results.length > 0) {
    const successIds = results.filter((item) => item.success).map((item) => item.provider_id)
    const failedIds = results.filter((item) => !item.success).map((item) => item.provider_id)
    return {
      successIds,
      failedIds,
      successCount: typeof result.success === 'number' ? result.success : successIds.length,
      failedCount: typeof result.failed === 'number' ? result.failed : failedIds.length,
      hasIds: true,
      hasCounts: true,
    }
  }

  const hasExplicitCounts = typeof result.success === 'number' || typeof result.failed === 'number'
  const successCount = typeof result.success === 'number' ? result.success : 0
  const failedCount = typeof result.failed === 'number' ? result.failed : 0
  if (
    hasExplicitCounts &&
    failedCount === 0 &&
    successCount === providerIds.length &&
    providerIds.length > 0
  ) {
    return {
      successIds: providerIds,
      failedIds: [],
      successCount,
      failedCount,
      hasIds: true,
      hasCounts: true,
    }
  }

  return {
    successIds: [],
    failedIds: [],
    successCount,
    failedCount,
    hasIds: false,
    hasCounts: hasExplicitCounts,
  }
}
// handleBulkToggleSchedulable updates scheduling for a captured selection and
// reconciles the response detail. Known successful IDs are patched locally.
// Unknown outcomes retain the selection and trigger a reload, while partial
// failures retain failed IDs when available.
//
// A fully successful identified result clears selection and shows the
// corresponding enabled or disabled message. Count-only partial results cannot
// identify rows to patch or retain individually, so the original IDs remain
// selected for inspection or retry after the administrator sees the outcome.
const handleBulkToggleSchedulable = async (schedulable: boolean) => {
  const providerIds = [...selIds.value]
  try {
    const result = await adminAPI.providers.bulkUpdate(providerIds, { schedulable })
    const { successIds, failedIds, successCount, failedCount, hasIds, hasCounts } =
      normalizeBulkSchedulableResult(result, providerIds)
    if (!hasIds && !hasCounts) {
      appStore.showError(t('admin.providers.bulkSchedulableResultUnknown'))
      setSelectedIds(providerIds)
      load().catch((error) => {
        console.error('Failed to refresh providers:', error)
      })
      return
    }
    if (successIds.length > 0) {
      updateSchedulableInList(successIds, schedulable)
    }
    if (successCount > 0 && failedCount === 0) {
      const message = schedulable
        ? t('admin.providers.bulkSchedulableEnabled', { count: successCount })
        : t('admin.providers.bulkSchedulableDisabled', { count: successCount })
      appStore.showSuccess(message)
    }
    if (failedCount > 0) {
      const message =
        hasCounts || hasIds
          ? t('admin.providers.bulkSchedulablePartial', {
              success: successCount,
              failed: failedCount,
            })
          : t('admin.providers.bulkSchedulableResultUnknown')
      appStore.showError(message)
      setSelectedIds(failedIds.length > 0 ? failedIds : providerIds)
    } else {
      if (hasIds) clearSelection()
      else setSelectedIds(providerIds)
    }
  } catch (error) {
    console.error('Failed to bulk toggle schedulable:', error)
    appStore.showError(t('common.error'))
  }
}

// openBulkEditSelected copies checked IDs and visible-row platform metadata
// into a selected bulk-edit target. It stores that target before opening the
// dialog so later selection changes do not mutate the request scope.
const openBulkEditSelected = () => {
  activeModal.value = {
    kind: 'bulkEdit',
    target: {
      mode: 'selected',
      providerIds: [...selIds.value],
      selectedPlatforms: [...selPlatforms.value],
      selectedTypes: [...selTypes.value],
    },
  }
}

// openBulkEditFiltered captures current filters and fetches their provider
// metadata before opening bulk editing. The target contains the preview count
// and platform/type sets from that request, independent of later filter
// changes.
const openBulkEditFiltered = async () => {
  const filters = buildBulkEditFilterSnapshot()
  const metadata = await loadBulkEditFilterMetadata(filters)
  activeModal.value = {
    kind: 'bulkEdit',
    target: {
      mode: 'filtered',
      filters,
      ...metadata,
    },
  }
}

// handleBulkUpdated invalidates upstream observations, closes bulk editing,
// clears its target and selection, then reloads rows. This ordering prevents
// bulk-changed credentials from restoring cached observations during the
// subsequent load.
const handleBulkUpdated = () => {
  invalidateAllUpstreamUsageCache()
  if (activeModal.value?.kind === 'bulkEdit') {
    activeModal.value = null
  }
  clearSelection()
  reload()
}
// handleExternalProvidersChanged invalidates cached upstream observations
// before reloading rows after an external provider operation. The new list can
// then hydrate observations against the returned provider identities and
// revisions.
const handleExternalProvidersChanged = () => {
  invalidateAllUpstreamUsageCache()
  reload()
}
// handleDataImported invalidates upstream observations and closes import before
// reloading providers. Imported credentials or query settings therefore receive
// cache identities from the refreshed list before observations can be restored.
const handleDataImported = () => {
  invalidateAllUpstreamUsageCache()
  if (activeModal.value?.kind === 'importData') {
    activeModal.value = null
  }
  reload()
}
// These sentinel values distinguish an absent group or privacy mode from an
// unrestricted filter when applying local provider updates.
const PROVIDER_UNGROUPED_GROUP_QUERY_VALUE = 'ungrouped'
const PROVIDER_PRIVACY_MODE_UNSET_QUERY_VALUE = '__unset__'
// buildProviderQueryFilters copies current filters and sorting for export and
// row matching. Empty filter values become empty strings, retaining the query
// conventions used by the provider list API.
const buildProviderQueryFilters = () => ({
  platform: params.platform || '',
  type: params.type || '',
  status: params.status || '',
  group: params.group || '',
  privacy_mode: params.privacy_mode || '',
  search: params.search || '',
  sort_by: sortState.sort_by,
  sort_order: sortState.sort_order,
})
// providerMatchesCurrentFilters tests an updated row against the currently
// selected provider filters. Status matching combines persisted status,
// scheduling permission, rate-limit expiry, and temporary restrictions at the
// time of evaluation. Group matching distinguishes the ungrouped marker from a
// numeric group ID.
//
// Privacy matching likewise distinguishes unset values from a named mode, and
// search compares trimmed lowercase text with the provider name. The caller
// removes a locally updated row when these rules exclude it and leaves
// full-page replacement to a later synchronization.
const providerMatchesCurrentFilters = (provider: Provider) => {
  const filters = buildProviderQueryFilters()
  if (filters.platform && provider.platform !== filters.platform) return false
  if (filters.type && provider.type !== filters.type) return false
  if (filters.status) {
    const now = Date.now()
    const rateLimitResetAt = provider.rate_limit_reset_at
      ? new Date(provider.rate_limit_reset_at).getTime()
      : Number.NaN
    const isRateLimited = Number.isFinite(rateLimitResetAt) && rateLimitResetAt > now
    const tempUnschedUntil = provider.temp_unschedulable_until
      ? new Date(provider.temp_unschedulable_until).getTime()
      : Number.NaN
    const isTempUnschedulable = Number.isFinite(tempUnschedUntil) && tempUnschedUntil > now

    if (filters.status === 'active') {
      if (
        provider.status !== 'active' ||
        isRateLimited ||
        isTempUnschedulable ||
        !provider.schedulable
      )
        return false
    } else if (filters.status === 'rate_limited') {
      if (provider.status !== 'active' || !isRateLimited || isTempUnschedulable) return false
    } else if (filters.status === 'temp_unschedulable') {
      if (provider.status !== 'active' || !isTempUnschedulable || isRateLimited) return false
    } else if (filters.status === 'unschedulable') {
      if (
        provider.status !== 'active' ||
        provider.schedulable ||
        isRateLimited ||
        isTempUnschedulable
      )
        return false
    } else if (provider.status !== filters.status) {
      return false
    }
  }
  if (filters.group) {
    const groupIds = provider.group_ids ?? provider.groups?.map((group) => group.id) ?? []
    if (filters.group === PROVIDER_UNGROUPED_GROUP_QUERY_VALUE) {
      if (groupIds.length > 0) return false
    } else if (!groupIds.includes(Number(filters.group))) {
      return false
    }
  }
  const privacyMode =
    typeof provider.extra?.privacy_mode === 'string' ? provider.extra.privacy_mode : ''
  if (filters.privacy_mode) {
    if (filters.privacy_mode === PROVIDER_PRIVACY_MODE_UNSET_QUERY_VALUE) {
      if (privacyMode.trim() !== '') return false
    } else if (privacyMode !== filters.privacy_mode) {
      return false
    }
  }
  const search = String(filters.search || '')
    .trim()
    .toLowerCase()
  if (search && !provider.name.toLowerCase().includes(search)) return false
  return true
}
// mergeRuntimeFields prepares a partial provider update for local row replacement
// by retaining displayed concurrency, window cost, and active-session observations
// when the update omits them or supplies null. Defined replacement values,
// including zero, take precedence over the displayed observations.
//
// patchProviderInList supplies the row with the same provider ID and uses the
// merged result for filter matching before replacement. Every other field comes
// from the update, so callers still need the provider configuration expected by
// the list rather than an arbitrary sparse patch.
const mergeRuntimeFields = (oldProvider: Provider, updatedProvider: Provider): Provider => ({
  ...updatedProvider,
  current_concurrency: updatedProvider.current_concurrency ?? oldProvider.current_concurrency,
  current_window_cost: updatedProvider.current_window_cost ?? oldProvider.current_window_cost,
  active_sessions: updatedProvider.active_sessions ?? oldProvider.active_sessions,
})

// syncPaginationAfterLocalRemoval decrements the visible result total and
// clamps the current page to its remaining range. It marks nonempty results for
// manual synchronization so removing a changed row does not immediately replace
// the page.
const syncPaginationAfterLocalRemoval = () => {
  const nextTotal = Math.max(0, pagination.total - 1)
  pagination.total = nextTotal
  pagination.pages = nextTotal > 0 ? Math.ceil(nextTotal / pagination.page_size) : 0

  const maxPage = Math.max(1, pagination.pages || 1)

  if (pagination.page > maxPage) {
    pagination.page = maxPage
  }
  // Keep remaining rows in place after a local removal; the pending notice lets the administrator choose when to refill the page.
  hasPendingListSync.value = nextTotal > 0
}

// patchProviderInList applies an updated provider to its currently displayed
// row while retaining omitted runtime observations. An update outside the
// current page is ignored. When the merged provider no longer matches filters,
// it is removed, pagination is adjusted, selection is cleared for that ID, and
// its action menu closes.
//
// Matching updates replace one row in a copied array and refresh dialog
// references. The function leaves full-page synchronization to the existing
// page controls, preserving unaffected rows and their mounted cell state.
const patchProviderInList = (updatedProvider: Provider) => {
  const index = providers.value.findIndex((provider) => provider.id === updatedProvider.id)
  if (index === -1) return
  const mergedProvider = mergeRuntimeFields(providers.value[index], updatedProvider)
  if (!providerMatchesCurrentFilters(mergedProvider)) {
    providers.value = providers.value.filter((provider) => provider.id !== mergedProvider.id)
    syncPaginationAfterLocalRemoval()
    removeSelectedProviders([mergedProvider.id])
    if (menu.acc?.id === mergedProvider.id) {
      menu.show = false
      menu.acc = null
    }
    return
  }
  const nextProviders = [...providers.value]
  nextProviders[index] = mergedProvider
  providers.value = nextProviders
  syncProviderRefs(mergedProvider)
}
// handleProviderUpdated invalidates an updated provider observation before
// patching its visible row. It then starts the silent refresh window so
// automatic polling does not immediately overwrite the local update.
const handleProviderUpdated = (updatedProvider: Provider) => {
  invalidateUpstreamUsageCache(updatedProvider.id)
  patchProviderInList(updatedProvider)
  enterAutoRefreshSilentWindow()
}
// openExportDataDialog opens provider export configuration with proxy inclusion
// enabled. Every invocation resets that preference, so reopening the dialog
// starts from the inclusion default even after a previous export disabled it.
const openExportDataDialog = () => {
  includeProxyOnExport.value = true
  activeModal.value = { kind: 'exportData' }
}
// handleExportData downloads the selected providers or current filtered result
// after step-up authorization. It prevents duplicate starts, obtains the export
// payload, creates a JSON download, and releases its object URL after
// triggering the link. Backend-reported skipped shadows produce a warning
// instead of the ordinary success message.
//
// Step-up cancellation closes silently, blocked authorization produces its
// specific message, and other failures display the returned error. The
// exporting flag and dialog visibility are cleared in finally for every
// completion path.
const handleExportData = async () => {
  if (exportingData.value) return
  exportingData.value = true
  try {
    const dataPayload = await providerExportStepUp.run(() =>
      adminAPI.providers.exportData(
        selIds.value.length > 0
          ? { ids: selIds.value, includeProxies: includeProxyOnExport.value }
          : {
              includeProxies: includeProxyOnExport.value,
              filters: buildProviderQueryFilters(),
            },
      ),
    )
    const timestamp = formatExportTimestamp()
    downloadJsonBlob(dataPayload, `tokenrouter-provider-${timestamp}.json`)
    // The backend omits Spark shadows from credential exports because their
    // parent supplies credentials; report how many selected providers are
    // absent from the downloaded payload.
    if (dataPayload.skipped_shadows && dataPayload.skipped_shadows > 0) {
      appStore.showWarning(
        t('admin.providers.dataExportedSkippedShadows', { count: dataPayload.skipped_shadows }),
      )
    } else {
      appStore.showSuccess(t('admin.providers.dataExported'))
    }
  } catch (error: any) {
    if (isStepUpCancelled(error)) {
      // Cancellation is the administrator declining verification, so close the export flow without displaying an additional request-error notification.
    } else if (isStepUpBlocked(error)) {
      appStore.showError(
        stepUpBlockReason(error) === 'STEP_UP_ADMIN_API_KEY_FORBIDDEN'
          ? t('stepUp.adminApiKeyForbidden')
          : t('stepUp.notEnabled'),
      )
    } else {
      appStore.showError(error?.message || t('admin.providers.dataExportFailed'))
    }
  } finally {
    exportingData.value = false
    if (activeModal.value?.kind === 'exportData') {
      activeModal.value = null
    }
  }
}
// providerExportStepUp owns authorization for sensitive provider exports and
// exposes the verification dialog used by the page template.
const providerExportStepUp = useStepUp()
// closeTestModal closes provider testing and clears its selected row reference.
const closeTestModal = () => {
  if (activeModal.value?.kind === 'test') activeModal.value = null
}
// closeStatsModal closes provider statistics and clears its selected row reference.
const closeStatsModal = () => {
  if (activeModal.value?.kind === 'stats') activeModal.value = null
}
// closeAdvancedSchedulerScoreModal closes scheduler details and clears its target.
const closeAdvancedSchedulerScoreModal = () => {
  if (activeModal.value?.kind === 'advancedSchedulerScore') activeModal.value = null
}
// closeInviteResetModal closes invitation reset and clears its selected provider.
const closeInviteResetModal = () => {
  if (activeModal.value?.kind === 'inviteReset') activeModal.value = null
}
// closeReAuthModal closes reauthorization and clears its selected provider row.
const closeReAuthModal = () => {
  if (activeModal.value?.kind === 'reauth') activeModal.value = null
}
// handleTest selects a provider and opens its request-testing dialog.
const handleTest = (a: Provider) => {
  activeModal.value = { kind: 'test', provider: a }
}
// handleViewStats selects a provider and opens its usage-statistics dialog.
const handleViewStats = (a: Provider) => {
  activeModal.value = { kind: 'stats', provider: a }
}
// handleAdvancedSchedulerScore selects a provider and opens its detailed
// scheduler scores.
const handleAdvancedSchedulerScore = (a: Provider) => {
  activeModal.value = { kind: 'advancedSchedulerScore', provider: a }
}
// handleInviteReset selects a provider and opens its invitation-reset dialog.
const handleInviteReset = (a: Provider) => {
  activeModal.value = { kind: 'inviteReset', provider: a }
}
// handleSchedule opens scheduled testing and fetches model IDs for the selected
// provider. Previous choices are cleared before the request; a failed model
// lookup leaves the panel open with an empty options list.
const handleSchedule = async (a: Provider) => {
  scheduleModelOptions.value = []
  activeModal.value = { kind: 'schedule', provider: a }
  try {
    const models = await adminAPI.providers.getAvailableModels(a.id)
    scheduleModelOptions.value = models.map((m: ClaudeModel) => ({ value: m.id, label: m.id }))
  } catch {
    // Model discovery is optional for opening the schedule panel, so retain the
    // empty choices when that lookup fails.
    scheduleModelOptions.value = []
  }
}
// closeSchedulePanel closes scheduled testing and clears both its provider
// target and model choices.
const closeSchedulePanel = () => {
  if (activeModal.value?.kind === 'schedule') activeModal.value = null
  scheduleModelOptions.value = []
}
// handleReAuth selects a provider and opens its credential reauthorization
// dialog.
const handleReAuth = (a: Provider) => {
  activeModal.value = { kind: 'reauth', provider: a }
}
// duplicatingProviderIDs locks duplicate requests per source provider until
// their API calls settle, allowing independent providers to be copied
// concurrently.
const duplicatingProviderIDs = new Set<number>()
// handleDuplicateProvider duplicates one provider while preventing another
// request for the same ID. Success reports the returned name and reloads the
// table; finally releases the ID lock after success or failure.
const handleDuplicateProvider = async (a: Provider) => {
  if (duplicatingProviderIDs.has(a.id)) return
  duplicatingProviderIDs.add(a.id)
  try {
    const duplicate = await adminAPI.providers.duplicate(a.id)
    appStore.showSuccess(t('admin.providers.duplicateSuccess', { name: duplicate.name }))
    reload()
  } catch (error: any) {
    console.error('Failed to duplicate provider:', error)
    appStore.showError(error?.message || t('admin.providers.duplicateFailed'))
  } finally {
    duplicatingProviderIDs.delete(a.id)
  }
}
// handleRefresh invalidates cached upstream usage before refreshing one
// provider credential. Success patches the row and starts the silent refresh
// window; a request failure is logged for diagnosis.
const handleRefresh = async (a: Provider) => {
  try {
    invalidateUpstreamUsageCache(a.id)
    const updated = await adminAPI.providers.refreshCredentials(a.id)
    patchProviderInList(updated)
    enterAutoRefreshSilentWindow()
  } catch (error) {
    console.error('Failed to refresh credentials:', error)
  }
}
// handleRecoverState requests provider recovery and patches the returned
// visible row. It starts the silent refresh window and reports success, while
// failures retain the current row and show the returned message.
const handleRecoverState = async (a: Provider) => {
  try {
    const updated = await adminAPI.providers.recoverState(a.id)
    patchProviderInList(updated)
    enterAutoRefreshSilentWindow()
    appStore.showSuccess(t('admin.providers.recoverStateSuccess'))
  } catch (error: any) {
    console.error('Failed to recover provider state:', error)
    appStore.showError(error?.message || t('admin.providers.recoverStateFailed'))
  }
}
// handleResetQuota resets one provider quota and patches the returned row.
// Success starts the silent refresh window and reports completion; request
// failures are logged while retaining the displayed row.
const handleResetQuota = async (a: Provider) => {
  try {
    const updated = await adminAPI.providers.resetProviderQuota(a.id)
    patchProviderInList(updated)
    enterAutoRefreshSilentWindow()
    appStore.showSuccess(t('common.success'))
  } catch (error) {
    console.error('Failed to reset quota:', error)
  }
}
// privacyResultMessageKey maps returned provider privacy state to a
// notification kind and translation key. OpenAI distinguishes training disabled
// and Cloudflare blockage, while Antigravity requires its own completed privacy
// marker.
const privacyResultMessageKey = (
  provider: Provider,
): {
  type: 'success' | 'error' // Notification display kind.
  key: string // Notification translation key.
} => {
  const mode = typeof provider.extra?.privacy_mode === 'string' ? provider.extra.privacy_mode : ''
  if (provider.platform === 'openai') {
    switch (mode) {
      case 'training_off':
        return { type: 'success', key: 'admin.providers.privacyTrainingOff' }
      case 'training_set_cf_blocked':
        return { type: 'error', key: 'admin.providers.privacyCfBlocked' }
      default:
        return { type: 'error', key: 'admin.providers.privacyFailed' }
    }
  }
  if (provider.platform === 'antigravity') {
    if (mode === 'privacy_set') {
      return { type: 'success', key: 'admin.providers.privacyAntigravitySet' }
    }
    return { type: 'error', key: 'admin.providers.privacyAntigravityFailed' }
  }
  return { type: 'error', key: 'admin.providers.privacyFailed' }
}
// handleSetPrivacy requests a privacy change and patches the returned provider
// row. It delays automatic refresh and reports the returned privacy state,
// since a successful HTTP request can still contain an unsuccessful privacy
// result.
const handleSetPrivacy = async (a: Provider) => {
  try {
    const updated = await adminAPI.providers.setPrivacy(a.id)
    patchProviderInList(updated)
    enterAutoRefreshSilentWindow()
    const result = privacyResultMessageKey(updated)
    if (result.type === 'success') {
      appStore.showSuccess(t(result.key))
    } else {
      appStore.showError(t(result.key))
    }
  } catch (error: any) {
    console.error('Failed to set privacy:', error)
    appStore.showError(error?.response?.data?.message || t('admin.providers.privacyFailed'))
  }
}
// onRevertFallback restores the provider proxy choice through the API and
// reloads the table after success. Failures show the returned server message
// when available and retain the current page.
const onRevertFallback = async (a: Provider) => {
  try {
    await adminAPI.providers.revertProxyFallback(a.id)
    appStore.showSuccess(t('admin.providers.revertProxySuccess'))
    reload()
  } catch (error: any) {
    console.error('Failed to revert proxy fallback:', error)
    appStore.showError(error?.response?.data?.message || t('admin.providers.revertProxyFailed'))
  }
}
// handleCreateSparkShadow selects a parent provider and opens shadow-creation
// confirmation.
const handleCreateSparkShadow = (a: Provider) => {
  activeModal.value = { kind: 'createShadow', provider: a }
}
// confirmCreateSparkShadow creates a named Spark shadow for the selected parent
// provider. Missing selection does nothing; success clears the dialog target
// and reloads rows, while failure leaves confirmation available for retry.
const confirmCreateSparkShadow = async () => {
  if (activeModal.value?.kind !== 'createShadow') return
  const a = activeModal.value.provider
  try {
    await adminAPI.providers.createSparkShadow(a.id, { name: `${a.name} (Spark)` })
    activeModal.value = null
    appStore.showSuccess(t('admin.providers.createSparkShadowSuccess'))
    reload()
  } catch (error: any) {
    console.error('Failed to create spark shadow:', error)
    appStore.showError(
      error?.response?.data?.message || t('admin.providers.createSparkShadowFailed'),
    )
  }
}
// handleDelete selects a provider and opens its deletion confirmation dialog.
const handleDelete = (a: Provider) => {
  activeModal.value = { kind: 'delete', provider: a }
}
// confirmDelete invalidates the selected provider cache before requesting
// deletion. Success clears the dialog target and reloads rows; missing targets
// do nothing and failures leave the current confirmation state available.
const confirmDelete = async () => {
  if (activeModal.value?.kind !== 'delete') return
  const providerID = activeModal.value.provider.id
  try {
    invalidateUpstreamUsageCache(providerID)
    await adminAPI.providers.delete(providerID)
    activeModal.value = null
    reload()
  } catch (error) {
    console.error('Failed to delete provider:', error)
  }
}
// handleToggleSchedulable requests the opposite scheduling state for one
// provider and exposes its loading ID. Success patches the returned flag, or
// the requested flag when omitted, then delays automatic refresh; finally
// clears loading.
const handleToggleSchedulable = async (a: Provider) => {
  const nextSchedulable = !a.schedulable
  togglingSchedulable.value = a.id
  try {
    const updated = await adminAPI.providers.setSchedulable(a.id, nextSchedulable)
    updateSchedulableInList([a.id], updated?.schedulable ?? nextSchedulable)
    enterAutoRefreshSilentWindow()
  } catch (error) {
    console.error('Failed to toggle schedulable:', error)
    appStore.showError(t('admin.providers.failedToToggleSchedulable'))
  } finally {
    togglingSchedulable.value = null
  }
}
// handleShowTempUnsched selects a provider and opens its
// temporary-unavailability status dialog.
const handleShowTempUnsched = (a: Provider) => {
  activeModal.value = { kind: 'tempUnsched', provider: a }
}
// handleTempUnschedReset closes temporary-status inspection and patches the
// returned provider row. It clears the dialog target first, then starts the
// silent refresh window to retain the local reset result.
const handleTempUnschedReset = async (updated: Provider) => {
  if (activeModal.value?.kind === 'tempUnsched') {
    activeModal.value = null
  }
  patchProviderInList(updated)
  enterAutoRefreshSilentWindow()
}
// providerTimestamps keeps the compact values and the full local times together.
function providerTimestamps(row: Provider) {
  const lastUsed = formatCompactDateTimeParts(row.last_used_at)
  const created = formatCompactDateTimeParts(row.created_at)
  const expires = formatCompactDateTimeParts(
    row.expires_at ? new Date(row.expires_at * 1000) : null,
  )
  return [
    {
      key: 'last_used_at',
      label: t('admin.providers.compact.lastUsed'),
      summary: lastUsed.full ? formatRelativeTime(row.last_used_at) : '',
      full: lastUsed.full,
    },
    {
      key: 'created_at',
      label: t('admin.providers.compact.created'),
      summary: created.full ? created.date + ' ' + created.time.slice(0, 5) : '',
      full: created.full,
    },
    {
      key: 'expires_at',
      label: t('admin.providers.compact.expires'),
      summary: expires.full ? expires.date + ' ' + expires.time.slice(0, 5) : '',
      full: expires.full,
    },
  ]
}

// fallbackOriginText retains the origin ID when the API supplies no proxy name.
function fallbackOriginText(row: Provider): string {
  return t('admin.providers.fallbackActiveTip', {
    origin: row.proxy_fallback_origin_name || '#' + row.proxy_fallback_origin_id,
  })
}

// isExpired compares an epoch-seconds expiry against the current browser time.
// Missing or zero values report false; supplied values are converted to
// milliseconds before comparing them with Date.now.
const isExpired = (value: number | null) => {
  if (!value) return false
  // Compare API expiry seconds in the browser clock's millisecond unit.
  return value * 1000 <= Date.now()
}
// proxyExpiryBadge delegates proxy expiry and status to the shared badge-class
// rules.
const proxyExpiryBadge = (p: ProviderProxy): string => proxyExpiryBadgeClass(p.expires_at, p.status)
// proxyExpiryText translates the shared proxy-expiry label and optional
// formatting parameters.
const proxyExpiryText = (p: ProviderProxy): string => {
  const { key, params } = proxyExpiryLabelKey(p.expires_at, p.status)
  return params ? t(key, params) : t(key)
}

// handleScroll closes the row action menu when scrolling changes its anchor
// position. Open toolbar dropdowns remain visible and receive updated geometry
// from their current trigger rectangles.
const handleScroll = () => {
  menu.show = false
  updateTopDropdownPositions()
}

// handleViewportResize updates open toolbar dropdown positions for the current
// viewport dimensions.
const handleViewportResize = () => {
  updateTopDropdownPositions()
}

// handleClickOutside closes each toolbar dropdown whose container does not
// contain the click target. Containment is checked independently so interacting
// inside one panel does not keep another outside panel open.
const handleClickOutside = (event: MouseEvent) => {
  const target = event.target as HTMLElement
  if (providerToolsDropdownRef.value && !providerToolsDropdownRef.value.contains(target)) {
    showProviderToolsDropdown.value = false
  }
  if (autoRefreshDropdownRef.value && !autoRefreshDropdownRef.value.contains(target)) {
    showAutoRefreshDropdown.value = false
  }
}

// Start the list independently, then wait for proxy and group metadata before
// installing page listeners and resuming interval refresh.
onMounted(async () => {
  load()
  const [proxiesResult, groupsResult] = await Promise.allSettled([
    adminAPI.proxies.getAll(),
    adminAPI.groups.getAllIncludingInactive(),
  ])
  if (proxiesResult.status === 'fulfilled') {
    proxies.value = proxiesResult.value
  } else {
    console.error('Failed to load proxies:', proxiesResult.reason)
  }
  if (groupsResult.status === 'fulfilled') {
    groups.value = groupsResult.value
  } else {
    console.error('Failed to load groups:', groupsResult.reason)
  }
  window.addEventListener('scroll', handleScroll, true)
  window.addEventListener('resize', handleViewportResize)
  document.addEventListener('click', handleClickOutside)

  if (autoRefreshEnabled.value) {
    autoRefreshCountdown.value = autoRefreshIntervalSeconds.value
    resumeAutoRefresh()
  } else {
    pauseAutoRefresh()
  }
})

// Remove listeners installed for this page so leaving the provider route cannot
// reposition menus or process clicks on a later page.
onUnmounted(() => {
  window.removeEventListener('scroll', handleScroll, true)
  window.removeEventListener('resize', handleViewportResize)
  document.removeEventListener('click', handleClickOutside)
})
</script>

<style scoped>
.provider-tools-menu-icon {
  @apply inline-flex h-8 w-8 flex-shrink-0 items-center justify-center rounded-control;
}
</style>

<style scoped>
.provider-platform-summary :deep(div > span) {
  min-width: 0;
  padding-block: 1px;
  white-space: nowrap;
}

.provider-platform-summary :deep(.leading-tight[title]) {
  display: none;
}

.provider-platform-summary :deep(.self-baseline) {
  overflow: hidden;
  text-overflow: ellipsis;
}
</style>
