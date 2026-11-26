//go:build integration

package postgres

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/TokenFlux/TokenRouter/internal/pkg/timezone"
	"github.com/TokenFlux/TokenRouter/internal/routing"
	"github.com/TokenFlux/TokenRouter/internal/usage"
)

// imported_calls_scenario_integration_test.go checks the historical report
// against the migrated schema. The fixture overlaps groups, models, sources,
// and minutes so counts, latency samples, and paging cannot pass by accident.

// TestImportedCallGroupAndMinuteReports checks half-open date bounds, exact
// model filtering, same-minute tie-breakers, and native usage isolation.
func TestImportedCallGroupAndMinuteReports(t *testing.T) {
	client := testEntClient(t)
	alpha := mustCreateGroup(t, client, &routing.Group{Name: "import-alpha-" + uuid.NewString()})
	beta := mustCreateGroup(t, client, &routing.Group{Name: "import-beta-" + uuid.NewString()})
	tx, err := integrationDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	store := NewUsageLogRepositoryWithSQL(client, tx, timezone.NewCalendar(time.UTC))
	start := time.Date(2026, 11, 17, 22, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Minute)
	// Same-minute rows can differ by model or platform. The other source and
	// the end-boundary row intentionally share the selected group and model.
	for _, row := range []struct {
		source, platform, model string
		group                   int64
		minute                  time.Time
		success, failure        int64
		sampleCount             int64
	}{
		{"raks", "openai", "alpha", alpha.ID, start, 8, 1, 4},
		{"raks", "anthropic", "alpha", alpha.ID, start, 2, 0, 1},
		{"raks", "openai", "beta", beta.ID, start.Add(time.Minute), 0, 3, 0},
		{"raks", "openai", "alpha", alpha.ID, end, 5, 2, 1},
		{"other", "openai", "alpha", alpha.ID, start, 6, 4, 1},
	} {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO public.marketplace_imported_call_minutes (
				source, group_id, source_group_id, bucket_start, source_platform, model,
				success_requests, error_requests, ignored_error_requests,
				input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
				duration_sum_ms, duration_count, ttft_sum_ms, ttft_count)
			VALUES ($1, $2, $2, $3, $4, $5, $6::bigint, $7, $7, $6 * 10, $6 * 3, $6 * 2, $6 * 5,
			        $8::bigint * 100, $8, $8 * 25, $8)`,
			row.source, row.group, row.minute, row.platform, row.model, row.success, row.failure, row.sampleCount)
		require.NoError(t, err)
	}

	query := usage.ImportedCallQuery{Start: &start, End: &end, Page: 1, PageSize: 1}
	service := usage.NewUsageService(store)
	report, err := service.ListImportedCalls(t.Context(), query)
	require.NoError(t, err)
	require.Equal(t, "raks", report.Source)
	require.EqualValues(t, 2, report.Total)
	require.Len(t, report.Groups, 2)
	require.Empty(t, report.Items)
	require.Equal(t, usage.ImportedCallGroup{
		GroupID: alpha.ID, GroupName: alpha.Name,
		ImportedCallMetrics: usage.ImportedCallMetrics{
			SuccessRequests: 10, ErrorRequests: 1,
			InputTokens: 100, OutputTokens: 30, CacheCreationTokens: 20, CacheReadTokens: 50,
			DurationSumMs: 500, DurationCount: 5, TTFTSumMs: 125, TTFTCount: 5,
		},
	}, report.Groups[0])
	require.EqualValues(t, 3, report.Groups[1].ErrorRequests)
	require.Zero(t, report.Groups[1].DurationCount)
	require.Zero(t, report.Groups[1].TTFTCount)

	query.WithMinutes, query.GroupID, query.Model = true, alpha.ID, "alpha"
	for page, platform := range []string{"anthropic", "openai"} {
		query.Page = page + 1
		report, err := service.ListImportedCalls(t.Context(), query)
		require.NoError(t, err)
		require.EqualValues(t, 2, report.Total)
		require.Len(t, report.Groups, 2)
		require.Len(t, report.Items, 1)
		require.Equal(t, platform, report.Items[0].SourcePlatform)
		require.True(t, start.Equal(report.Items[0].BucketStart))
	}
	query.Page = 3
	report, err = service.ListImportedCalls(t.Context(), query)
	require.NoError(t, err)
	require.EqualValues(t, 2, report.Total)
	require.Empty(t, report.Items)

	query.Start, query.End = nil, nil
	report, err = service.ListImportedCalls(t.Context(), query)
	require.NoError(t, err)
	require.EqualValues(t, 3, report.Total)
	require.Len(t, report.Items, 1)
	require.True(t, start.Equal(report.Items[0].BucketStart))

	native, err := store.GetStatsWithFilters(t.Context(), UsageLogFilters{
		GroupID:   alpha.ID,
		StartTime: &start,
		EndTime:   &end,
	})
	require.NoError(t, err)
	require.Zero(t, native.TotalRequests)
	require.Zero(t, native.TotalTokens)
	require.Zero(t, native.TotalActualCost)
}
