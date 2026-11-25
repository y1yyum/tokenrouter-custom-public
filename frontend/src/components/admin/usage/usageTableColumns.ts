import type { Column } from '@/components/common/table/types'

// usageTableColumns.ts defines the shared usage layout. All roles keep the same
// column keys so a team-context change can reuse the user's saved preferences.

// UsageTableRole determines attribution and administrative diagnostics in compact rows.
export type UsageTableRole = 'admin' | 'teamOwner' | 'personal'

// getUsageTableColumns gives each role its width budget and maps the display columns
// to the two sort keys accepted by the usage endpoints.
export function getUsageTableColumns(role: UsageTableRole, t: (key: string) => string): Column[] {
  const isAdmin = role === 'admin'
  const model = t('usage.compactTable.modelSpec')
  const time = t('usage.time')
  return [
    { key: 'time', label: time, width: '90px', sortOptions: [{ key: 'created_at', label: time }] },
    {
      key: 'caller',
      label: t(`usage.compactTable.caller.${role}`),
      width: isAdmin ? '195px' : '175px',
    },
    {
      key: 'route',
      label: t(isAdmin ? 'usage.compactTable.route' : 'usage.endpoint'),
      width: isAdmin ? '175px' : '135px',
    },
    {
      key: 'model_spec',
      label: model,
      width: isAdmin ? '225px' : '245px',
      sortOptions: [{ key: 'model', label: model }],
    },
    { key: 'tokens', label: t('usage.tokens'), width: isAdmin ? '175px' : '185px' },
    { key: 'cost', label: t('usage.cost'), width: isAdmin ? '145px' : '135px' },
    {
      key: 'performance',
      label: t('usage.compactTable.performance'),
      width: isAdmin ? '150px' : '155px',
    },
    {
      key: 'request_trace',
      label: t('usage.compactTable.requestTrace'),
      width: isAdmin ? '160px' : '150px',
    },
    {
      key: 'client_network',
      label: t('usage.compactTable.clientNetwork'),
      width: isAdmin ? '195px' : '215px',
    },
  ]
}
