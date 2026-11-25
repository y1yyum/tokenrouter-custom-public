// types.ts defines the column configuration consumed by DataTable and its column
// ordering composable. Column keys connect row values, named cell slots, sorting
// controls, and the browser preference that records movable column order. A
// composite header can bind those controls to separate data sort keys.

// Column describes one rendered table column and its optional cell formatting
// behavior.
export interface Column {
  key: string // Row property and slot identifier.
  label: string // Displayed column header text.
  /** Preferred CSS width. Compact sizing uses positive pixel widths as its baseline. */
  width?: string
  sortable?: boolean // Enable sorting by the column key.
  /** Sort options use data keys and visible labels. A nonempty list overrides sortable. */
  sortOptions?: { key: string; label: string }[]
  /** Inline shows option labels in place of the title. Subline keeps the title above them. */
  subtitleSortLayout?: 'inline' | 'subline'
  class?: string // Header and cell classes.
  formatter?: (value: any, row: any) => string // Default cell text formatter.
}

// getColumnSortOptions resolves the available sort controls for a visible
// column. A server-backed parent can use the same options before its first
// request to reject stored keys that no visible column offers.
export function getColumnSortOptions(column: Column) {
  if (column.sortOptions?.length) return column.sortOptions
  return column.sortable ? [{ key: column.key, label: column.label }] : []
}
