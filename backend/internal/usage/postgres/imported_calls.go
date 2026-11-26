package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/TokenFlux/TokenRouter/internal/usage"
)

// imported_calls.go reads raks group totals and minute rows from the import
// table. Existing usage, settlement, and dashboard rollup tables stay outside
// these queries. A pool-backed request reads its group choices, total, and
// page in one snapshot so a concurrent import cannot shift the results within
// that response.

const (
	// importedCallScope binds the same selectors for grouped and paged reads.
	importedCallScope = `
		FROM public.marketplace_imported_call_minutes m
		WHERE m.source = $1
		  AND ($2::timestamptz IS NULL OR m.bucket_start >= $2)
		  AND ($3::timestamptz IS NULL OR m.bucket_start < $3)
		  AND ($4::bigint = 0 OR m.group_id = $4)
		  AND ($5::text = '' OR m.model = $5)`
)

// ListImportedCalls reads raks history with a consistent snapshot for pool
// connections. A store already bound to a caller transaction uses its snapshot.
func (r *Store) ListImportedCalls(ctx context.Context, query usage.ImportedCallQuery) (*usage.ImportedCallReport, error) {
	if r.db == nil || !query.WithMinutes {
		return listImportedCalls(ctx, r.sql, query)
	}

	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin imported call report: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	report, err := listImportedCalls(ctx, tx, query)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("finish imported call report: %w", err)
	}
	return report, nil
}

// listImportedCalls assembles groups and, for the detailed view, the exact
// matching row count and a deterministically ordered page of minute rows.
func listImportedCalls(ctx context.Context, db sqlQueryer, query usage.ImportedCallQuery) (*usage.ImportedCallReport, error) {
	report := &usage.ImportedCallReport{
		Source:   usage.ImportedCallSource,
		Items:    []usage.ImportedCallMinute{},
		Page:     query.Page,
		PageSize: query.PageSize,
	}
	args := []any{report.Source, query.Start, query.End, query.GroupID, query.Model}
	groupArgs := args
	if query.WithMinutes {
		// Populate the filter choices from the entire date range.
		groupArgs = []any{report.Source, query.Start, query.End, int64(0), ""}
	}
	groups, err := listImportedCallGroups(ctx, db, groupArgs)
	if err != nil {
		return nil, err
	}
	report.Groups = groups
	report.Total = int64(len(groups))
	if !query.WithMinutes {
		return report, nil
	}

	if err := scanSingleRow(ctx, db, "SELECT count(*)"+importedCallScope, args, &report.Total); err != nil {
		return nil, fmt.Errorf("count imported call minutes: %w", err)
	}
	// Check the page against the exact count before multiplying its offset.
	if report.Total == 0 || int64(query.Page-1) > (report.Total-1)/int64(query.PageSize) {
		return report, nil
	}
	offset := int64(query.Page-1) * int64(query.PageSize)
	report.Items, err = listImportedCallMinutes(ctx, db, append(args, query.PageSize, offset))
	if err != nil {
		return nil, err
	}
	return report, nil
}

// listImportedCallGroups adds each counter across the selected minutes.
// Reading names after grouping keeps group metadata out of the aggregation.
func listImportedCallGroups(ctx context.Context, db sqlQueryer, args []any) ([]usage.ImportedCallGroup, error) {
	rows, err := db.QueryContext(ctx, `
		WITH totals AS (
			SELECT m.group_id,
			       sum(m.success_requests) AS successes, sum(m.error_requests) AS errors,
			       sum(m.input_tokens) AS input_tokens, sum(m.output_tokens) AS output_tokens,
			       sum(m.cache_creation_tokens) AS cache_creation_tokens, sum(m.cache_read_tokens) AS cache_read_tokens,
			       sum(m.duration_sum_ms) AS duration_sum_ms, sum(m.duration_count) AS duration_count,
			       sum(m.ttft_sum_ms) AS ttft_sum_ms, sum(m.ttft_count) AS ttft_count`+importedCallScope+`
			GROUP BY m.group_id
		)
		SELECT g.id, g.name, t.successes, t.errors, t.input_tokens, t.output_tokens,
		       t.cache_creation_tokens, t.cache_read_tokens,
		       t.duration_sum_ms, t.duration_count, t.ttft_sum_ms, t.ttft_count
		FROM totals t JOIN public.groups g ON g.id = t.group_id
		ORDER BY t.successes + t.errors DESC, g.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("query imported call groups: %w", err)
	}
	defer func() { _ = rows.Close() }()

	groups := []usage.ImportedCallGroup{}
	for rows.Next() {
		var group usage.ImportedCallGroup
		if err := rows.Scan(importedCallGroupScanTargets(&group)...); err != nil {
			return nil, fmt.Errorf("scan imported call group: %w", err)
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

// listImportedCallMinutes preserves stored row identity across pages by using
// every primary-key dimension within the fixed source as a tie-breaker.
func listImportedCallMinutes(ctx context.Context, db sqlQueryer, args []any) ([]usage.ImportedCallMinute, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT g.id, g.name, m.success_requests, m.error_requests, m.input_tokens, m.output_tokens,
		       m.cache_creation_tokens, m.cache_read_tokens,
		       m.duration_sum_ms, m.duration_count, m.ttft_sum_ms, m.ttft_count,
		       m.bucket_start, m.source_platform, m.model
		FROM (SELECT m.*`+importedCallScope+`
			ORDER BY m.bucket_start DESC, m.group_id, m.source_platform, m.model
			LIMIT $6 OFFSET $7
		) m JOIN public.groups g ON g.id = m.group_id
		ORDER BY m.bucket_start DESC, m.group_id, m.source_platform, m.model`, args...)
	if err != nil {
		return nil, fmt.Errorf("query imported call minutes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := []usage.ImportedCallMinute{}
	for rows.Next() {
		var item usage.ImportedCallMinute
		targets := importedCallGroupScanTargets(&item.ImportedCallGroup)
		targets = append(targets, &item.BucketStart, &item.SourcePlatform, &item.Model)
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("scan imported call minute: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// importedCallGroupScanTargets keeps the identity and metrics column order
// shared by the grouped and minute queries.
func importedCallGroupScanTargets(g *usage.ImportedCallGroup) []any {
	return []any{
		&g.GroupID, &g.GroupName, &g.SuccessRequests, &g.ErrorRequests,
		&g.InputTokens, &g.OutputTokens, &g.CacheCreationTokens, &g.CacheReadTokens,
		&g.DurationSumMs, &g.DurationCount, &g.TTFTSumMs, &g.TTFTCount,
	}
}
