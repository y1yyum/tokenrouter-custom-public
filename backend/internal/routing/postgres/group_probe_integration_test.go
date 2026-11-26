//go:build integration

package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/TokenFlux/TokenRouter/internal/routing"
	"github.com/TokenFlux/TokenRouter/internal/testutil/postgrescontainer"
)

// group_probe_integration_test.go checks leased configurations and availability history in PostgreSQL.
// The latest probe can lie outside the availability window, and equal start
// times need a deterministic winner. PostgreSQL fixtures separate recent counts
// from latest status and include a group with no history to exercise the
// unknown result.

// TestGroupProbeLatestResults inserts tied recent probes, an older probe, and
// an empty group. It checks ID tie resolution, window totals, an older latest
// status, and an absent latest timestamp.
func TestGroupProbeLatestResults(t *testing.T) {
	db := postgrescontainer.New(t)
	ctx := context.Background()
	ids := make([]int64, 3)
	for i, name := range []string{"probe-recent", "probe-old", "probe-empty"} {
		require.NoError(t, db.QueryRow("INSERT INTO groups(name) VALUES($1) RETURNING id", name).Scan(&ids[i]))
	}
	// Use whole seconds so PostgreSQL timestamp storage keeps the equality
	// assertion exact.
	now := time.Now().UTC().Truncate(time.Second)
	for _, row := range []struct {
		group  int64  // owning fixture group
		status string // stored probe outcome
		// start and finish describe the observation separately: ties on start use
		// insertion ID for latest selection, and the returned timestamp comes from
		// finish.
		start  time.Time // latest ordering timestamp
		finish time.Time // returned completion timestamp
	}{
		{ids[0], "failed", now.Add(-time.Hour), now.Add(-time.Hour)},
		{ids[0], "success", now.Add(-time.Hour), now.Add(-time.Hour + time.Minute)},
		{ids[1], "failed", now.Add(-48 * time.Hour), now.Add(-48*time.Hour + time.Minute)},
	} {
		_, err := db.Exec(`INSERT INTO group_availability_probe_results(group_id,model_id,status,success,started_at,finished_at)
			VALUES($1,'test',$2,$3,$4,$5)`, row.group, row.status, row.status == "success", row.start, row.finish)
		require.NoError(t, err)
	}
	store := NewGroupAvailabilityProbeRepository(db)
	got, err := store.GetSummaryByGroupIDs(ctx, ids, 1, 60, "UTC", now)
	require.NoError(t, err)
	require.Equal(t, "success", got[ids[0]].LastStatus)
	require.True(t, now.Add(-time.Hour+time.Minute).Equal(*got[ids[0]].LastCheckedAt))
	require.EqualValues(t, 2, got[ids[0]].TotalCount)
	require.EqualValues(t, 1, got[ids[0]].SuccessCount)
	require.Equal(t, "failed", got[ids[1]].LastStatus)
	require.Zero(t, got[ids[1]].TotalCount)
	require.Nil(t, got[ids[2]].LastCheckedAt)
}

// TestImportedGroupAvailabilityWeightsRequests places differently sized request
// batches on exact window and bucket boundaries. It checks weighted counts,
// all-failed observations, empty groups, and unavailable individual outcomes.
func TestImportedGroupAvailabilityWeightsRequests(t *testing.T) {
	db := postgrescontainer.New(t)
	now := time.Date(2026, 11, 24, 20, 0, 0, 0, time.UTC)
	start := now.Add(-24 * time.Hour)
	ids := make([]int64, 3)
	for i, name := range []string{"historical-mixed", "historical-failed", "historical-empty"} {
		require.NoError(t, db.QueryRowContext(t.Context(), "INSERT INTO groups(name) VALUES($1) RETURNING id", name).Scan(&ids[i]))
	}
	for _, row := range []struct {
		groupID int64     // destination group identifier
		bucket  time.Time // source UTC minute boundary
		success int64     // successful request count
		failed  int64     // recorded failed requests
		ignored int64     // failures ignored for health scoring
	}{
		{ids[0], start.Add(-time.Minute), 90, 0, 0},
		{ids[0], start, 9, 1, 1},
		{ids[0], start.Add(time.Minute), 0, 1, 0},
		{ids[0], start.Add(time.Hour), 2, 0, 0},
		{ids[0], now, 900, 0, 0},
		{ids[1], start, 0, 3, 0},
	} {
		_, err := db.ExecContext(t.Context(), `
			INSERT INTO marketplace_imported_call_minutes (
				source,group_id,source_group_id,bucket_start,source_platform,model,
				success_requests,error_requests,ignored_error_requests,input_tokens,output_tokens,
				cache_creation_tokens,cache_read_tokens,ttft_sum_ms,ttft_count,duration_sum_ms,duration_count
			) VALUES ('fixture',$1,$1,$2,'openai','test',$3,$4,$5,0,0,0,0,0,0,0,0)
		`, row.groupID, row.bucket, row.success, row.failed, row.ignored)
		require.NoError(t, err)
	}

	store := NewGroupAvailabilityProbeRepository(db)
	got, err := store.GetImportedSummaryByGroupIDs(t.Context(), ids, 1, 60, "UTC", now)
	require.NoError(t, err)
	require.Len(t, got, 2)
	mixed := got[ids[0]]
	require.Equal(t, routing.GroupAvailabilitySourceImportedCalls, mixed.Source)
	require.EqualValues(t, 11, mixed.SuccessCount)
	require.EqualValues(t, 13, mixed.TotalCount)
	require.InDelta(t, 11.0/13.0, *mixed.AvailabilityRate, 1e-9)
	require.EqualValues(t, 9, mixed.Days[0].SuccessCount)
	require.EqualValues(t, 11, mixed.Days[0].TotalCount)
	require.EqualValues(t, 2, mixed.Days[1].TotalCount)
	require.Nil(t, mixed.Days[2].AvailabilityRate)
	require.Empty(t, mixed.LastStatus)
	require.Nil(t, mixed.LastCheckedAt)
	require.NotNil(t, got[ids[1]].AvailabilityRate)
	require.Zero(t, *got[ids[1]].AvailabilityRate)
	require.NotContains(t, got, ids[2])
}

// TestGroupProbeClaimRetainsProtocol leases old and protocol-specific configurations from JSON storage.
// Claimed settings keep the explicit entry while records saved before protocol selection remain automatic.
func TestGroupProbeClaimRetainsProtocol(t *testing.T) {
	db := postgrescontainer.New(t)
	for index, config := range []string{
		`{"enabled":true,"model_id":"alias","prompt":"hi"}`,
		`{"enabled":true,"model_id":"alias","prompt":"hi","protocol":"openai_chat_completions"}`,
	} {
		_, err := db.Exec(`INSERT INTO groups(name, availability_probe_config) VALUES($1, $2::jsonb)`, fmt.Sprintf("probe-%d", index), config)
		require.NoError(t, err)
	}
	now := time.Now().UTC()
	claimed, err := NewGroupAvailabilityProbeRepository(db).ClaimDue(t.Context(), now, now.Add(time.Minute), "test", 10)
	require.NoError(t, err)
	require.Len(t, claimed, 2)
	require.Empty(t, claimed[0].Config.Protocol)
	require.EqualValues(t, "openai_chat_completions", claimed[1].Config.Protocol)
}
