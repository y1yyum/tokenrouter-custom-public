package cryptopay

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/TokenFlux/TokenRouter/internal/cryptopay/scanner"
)

type networkSnapshotMemory struct {
	values map[string]networkStatusSnapshot
	err    error
	writes int
}

type slowNetworkStatusStore struct {
	entered chan string
	release chan struct{}
}

func TestDepositNetworkStatusContinuousEvidence(t *testing.T) {
	start := time.Date(2026, 11, 21, 2, 0, 0, 0, time.UTC)
	obs := func(round uint64, seconds int, before, after, head uint64, failed, nearHead bool) scanner.DepositScanObservation {
		return scanner.DepositScanObservation{
			Chain: ChainBSC, Round: round, At: start.Add(time.Duration(seconds) * time.Second),
			ConfirmedHead: head, CursorBefore: before, Cursor: after, CanMeasure: true, ForwardFailed: failed, NearHeadScanned: nearHead, Failed: failed,
		}
	}
	t.Run("fresh consecutive progress and single failure", func(t *testing.T) {
		var s networkScanEvidence
		state, _ := s.observe(obs(1, 0, 100, 105, 105, false, true))
		require.Equal(t, depositStatusUnknown, state)
		state, _ = s.observe(obs(2, 5, 105, 106, 106, false, true))
		require.Equal(t, depositStatusAvailable, state)
		state, _ = s.observe(obs(3, 10, 106, 106, 107, true, false))
		require.Equal(t, depositStatusDelayed, state)
		state, _ = s.observe(obs(4, 15, 106, 107, 108, false, false))
		require.Equal(t, depositStatusAvailable, state, "a one-block backlog is normal scan lag, not a delay")
		state, _ = s.observe(obs(5, 20, 107, 108, 108, false, true))
		require.Equal(t, depositStatusAvailable, state)
		state, _ = s.observe(obs(7, 25, 108, 109, 109, false, true))
		require.Equal(t, depositStatusUnknown, state, "a skipped periodic attempt breaks continuity")
		state, _ = s.observe(obs(8, 30, 88, 99, 111, false, false))
		require.Equal(t, depositStatusUnknown, state, "rewind could be a manual job")
	})
	t.Run("sustained failed coverage with advancing head and no cursor progress", func(t *testing.T) {
		var s networkScanEvidence
		for i, sec := range []int{0, 60, 120, 179, 180} {
			state, proof := s.observe(obs(uint64(i+1), sec, 100, 100, 300+uint64(sec), true, false))
			want := depositStatusDelayed
			if sec == 180 {
				want = depositStatusStalled
				require.Equal(t, start, proof)
			}
			require.Equal(t, want, state, sec)
		}
		state, _ := s.observe(obs(6, 185, 100, 100, 485, true, true))
		require.Equal(t, depositStatusDelayed, state, "near-head scan covers new deposits")
		state, _ = s.observe(obs(7, 190, 100, 110, 490, true, false))
		require.Equal(t, depositStatusDelayed, state, "committed progress ends a stall")
		state, _ = s.observe(obs(8, 310, 110, 111, 491, false, false))
		require.Equal(t, depositStatusUnknown, state, "freshness gap ends evidence")
	})
}

// backlogObservation is one healthy scan pass whose durable cursor sits `lag`
// blocks behind the confirmed head.
func backlogObservation(chain string, round int, lag uint64, at time.Time) scanner.DepositScanObservation {
	const cursor = 10_000
	return scanner.DepositScanObservation{
		Chain: chain, Round: uint64(round), At: at,
		ConfirmedHead: cursor + lag, CursorBefore: cursor, Cursor: cursor,
		CanMeasure: true,
	}
}

// The bands are chain-time based: the same block count means very different
// delays per network, so a fixed block threshold cannot be correct for all.
func TestDepositNetworkStatusBacklogBands(t *testing.T) {
	start := time.Date(2026, 11, 21, 2, 0, 0, 0, time.UTC)
	secondPass := start.Add(15 * time.Second)
	for _, tc := range []struct {
		name  string
		chain string
		lag   uint64
		want  string
	}{
		{"BSC 59 blocks stays quiet", ChainBSC, 59, depositStatusAvailable},
		{"BSC 100 blocks is the five-minute normal boundary", ChainBSC, 100, depositStatusAvailable},
		{"BSC 101 blocks gets the calm notice", ChainBSC, 101, depositStatusSlow},
		{"BSC 600 blocks is the thirty-minute slow boundary", ChainBSC, 600, depositStatusSlow},
		{"BSC 601 blocks is a real delay", ChainBSC, 601, depositStatusDelayed},
		{"Ethereum 25 blocks is the five-minute boundary", ChainEthereum, 25, depositStatusAvailable},
		{"Ethereum 26 blocks is already slower than normal", ChainEthereum, 26, depositStatusSlow},
		{"the same 100 blocks are twenty minutes on Ethereum", ChainEthereum, 100, depositStatusSlow},
		{"Ethereum 150 blocks is the thirty-minute slow boundary", ChainEthereum, 150, depositStatusSlow},
		{"Ethereum 151 blocks is a real delay", ChainEthereum, 151, depositStatusDelayed},
		{"Arbitrum 1200 blocks is still five minutes", ChainArbitrum, 1200, depositStatusAvailable},
		{"Arbitrum 1201 blocks is slower than normal", ChainArbitrum, 1201, depositStatusSlow},
		{"an unknown chain falls back to the BSC interval", "future-chain", 101, depositStatusSlow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var evidence networkScanEvidence
			state, _ := evidence.observe(backlogObservation(tc.chain, 1, tc.lag, start))
			require.Equal(t, depositStatusUnknown, state, "a single pass has no continuity evidence yet")
			state, proof := evidence.observe(backlogObservation(tc.chain, 2, tc.lag, secondPass))
			require.Equal(t, tc.want, state)
			if tc.want == depositStatusAvailable {
				require.Equal(t, start, proof, "available is proven by the previous healthy pass")
			} else {
				require.Equal(t, secondPass, proof)
			}
		})
	}
}

// A healthy scanner idles a little behind the head all the time. Twelve minutes
// of that must never turn into "delayed", which is the production false positive.
func TestDepositNetworkStatusSteadySmallLagNeverDelayed(t *testing.T) {
	start := time.Date(2026, 11, 21, 2, 0, 0, 0, time.UTC)
	// 60 BSC blocks ≈ 3 minutes: inside the five-minute quiet band.
	const cursor, lag = 10_000, uint64(60)
	var evidence networkScanEvidence
	for round := 1; round <= 25; round++ {
		state, _ := evidence.observe(backlogObservation(ChainBSC, round, lag, start.Add(time.Duration(round-1)*30*time.Second)))
		if round == 1 {
			require.Equal(t, depositStatusUnknown, state)
			continue
		}
		require.Equal(t, depositStatusAvailable, state, "round %d", round)
	}
}

func (m *networkSnapshotMemory) load(_ context.Context, _ []string) (map[string]networkStatusSnapshot, error) {
	return m.values, m.err
}

func (m *networkSnapshotMemory) save(_ context.Context, _ string, _ networkStatusSnapshot, _ time.Duration) error {
	m.writes++
	return m.err
}

func TestDepositNetworkStatusFreshSharedPolicy(t *testing.T) {
	const raw = `{"enabled":true,"chains":["bsc","ethereum"]}`
	observed := time.Date(2026, 11, 21, 2, 0, 0, 0, time.UTC)
	policy, ok := decodeNetworkStatusPolicy(raw, observed.Add(-time.Minute))
	require.True(t, ok)
	for _, tc := range []struct {
		name           string
		raw            string
		updatedAt, now time.Time
		fingerprint    string
		state          string
		storeErr       error
		want           string
	}{
		{"fresh available", raw, observed.Add(-time.Minute), observed.Add(time.Second), policy.fingerprint, depositStatusAvailable, nil, depositStatusAvailable},
		{"fresh slow", raw, observed.Add(-time.Minute), observed, policy.fingerprint, depositStatusSlow, nil, depositStatusSlow},
		{"fresh delayed", raw, observed.Add(-time.Minute), observed, policy.fingerprint, depositStatusDelayed, nil, depositStatusDelayed},
		{"fresh confirmed stalled", raw, observed.Add(-4 * time.Minute), observed, policy.fingerprint, depositStatusStalled, nil, depositStatusStalled},
		{"unproven cached stalled", raw, observed.Add(-time.Minute), observed, policy.fingerprint, depositStatusStalled, nil, depositStatusUnknown},
		{"stale at TTL", raw, observed.Add(-time.Minute), observed.Add(depositNetworkTTL), policy.fingerprint, depositStatusStalled, nil, depositStatusUnknown},
		{"future", raw, observed.Add(-time.Minute), observed.Add(-time.Second), policy.fingerprint, depositStatusAvailable, nil, depositStatusUnknown},
		{"previous config fingerprint", raw, observed.Add(-time.Minute), observed, "old", depositStatusStalled, nil, depositStatusUnknown},
		{"same config after edit", raw, observed.Add(-time.Second), observed, policy.fingerprint, depositStatusStalled, nil, depositStatusUnknown},
		{"cache read fails", raw, observed.Add(-time.Minute), observed, policy.fingerprint, depositStatusAvailable, errors.New("cache unavailable"), depositStatusUnknown},
		{"missing config row", "", time.Time{}, observed, policy.fingerprint, depositStatusAvailable, nil, depositStatusUnknown},
		{"missing explicit channel switch", `{"chains":["bsc"]}`, observed.Add(-time.Minute), observed, policy.fingerprint, depositStatusAvailable, nil, depositStatusUnknown},
		{"old cache cannot pause", raw, observed.Add(-time.Minute), observed, policy.fingerprint, depositStatusPaused, nil, depositStatusUnknown},
		{"explicit site pause despite cache failure", `{"enabled":false}`, observed.Add(-time.Minute), observed, "old", depositStatusAvailable, errors.New("cache unavailable"), depositStatusPaused},
		{"explicit chain removed", `{"enabled":true,"chains":["ethereum"]}`, observed.Add(-time.Minute), observed, "old", depositStatusAvailable, nil, depositStatusPaused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			q := mock.ExpectQuery("SELECT value, updated_at FROM settings WHERE key").WithArgs(SettingKeyCryptoConfig)
			if tc.raw == "" {
				q.WillReturnError(sql.ErrNoRows)
			} else {
				q.WillReturnRows(sqlmock.NewRows([]string{"value", "updated_at"}).AddRow(tc.raw, tc.updatedAt))
			}
			mock.ExpectClose()
			t.Cleanup(func() {
				require.NoError(t, db.Close())
				require.NoError(t, mock.ExpectationsWereMet())
			})
			evidenceAt := observed.Add(-5 * time.Second)
			if tc.name == "fresh confirmed stalled" || tc.name == "stale at TTL" || tc.name == "same config after edit" || tc.name == "previous config fingerprint" {
				evidenceAt = observed.Add(-chainStalledAfter)
			}
			store := &networkSnapshotMemory{err: tc.storeErr, values: map[string]networkStatusSnapshot{
				ChainBSC: {Status: tc.state, ObservedMS: observed.UnixMilli(), EvidenceMS: evidenceAt.UnixMilli(), Fingerprint: tc.fingerprint},
			}}
			s := &Service{db: db, depositNetworkStatus: newDepositNetworkMonitor(store, func() time.Time { return tc.now })}
			report := s.DepositNetworkStatus(t.Context())
			for _, n := range report.Networks {
				if n.Chain == ChainBSC {
					require.Equal(t, tc.want, n.Status)
					if tc.want == depositStatusUnknown {
						require.Nil(t, n.ObservedAt)
						require.Nil(t, n.ExpiresAt)
					} else {
						require.NotNil(t, n.ObservedAt)
						require.NotNil(t, n.ExpiresAt)
						wantObservation := observed
						if tc.want == depositStatusPaused {
							wantObservation = tc.now
						}
						require.Equal(t, wantObservation, *n.ObservedAt)
						require.Equal(t, wantObservation.Add(depositNetworkTTL), *n.ExpiresAt)
					}
				}
			}
			require.Zero(t, store.writes, "a user GET never writes the health cache")
		})
	}
}

func (s slowNetworkStatusStore) load(context.Context, []string) (map[string]networkStatusSnapshot, error) {
	return nil, nil
}

func (s slowNetworkStatusStore) save(_ context.Context, chain string, _ networkStatusSnapshot, _ time.Duration) error {
	s.entered <- chain
	<-s.release // A slow backend can ignore cancellation; the worker must still move.
	return errors.New("simulated write failure")
}

func TestDepositNetworkStatusSlowCacheDoesNotBlockScanner(t *testing.T) {
	store := slowNetworkStatusStore{entered: make(chan string, 4), release: make(chan struct{})}
	defer close(store.release)
	now := time.Now()
	observer := newDepositNetworkMonitor(store, func() time.Time { return now }).observer("config")
	observed := func(chain string, round uint64) {
		observer(scanner.DepositScanObservation{Chain: chain, At: now, Round: round})
	}
	writeStarted := func() string {
		t.Helper()
		select {
		case chain := <-store.entered:
			return chain
		case <-time.After(time.Second):
			t.Fatal("independent cache write did not start")
			return ""
		}
	}
	initial := make(chan struct{})
	go func() {
		observed(ChainBSC, 1)
		close(initial)
	}()
	require.Equal(t, ChainBSC, writeStarted())
	select {
	case <-initial:
	case <-time.After(time.Second):
		t.Fatal("scan waited for Redis")
	}
	observed(ChainBSC, 2)
	observed(ChainEthereum, 2)
	require.Equal(t, ChainEthereum, writeStarted(), "slow chain must not queue another write or block other chains")
}

func TestDepositNetworkStatusRedisRejectsLateWriterAndExpires(t *testing.T) {
	redisServer := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	store := redisNetworkStatusStore{rdb: rdb}
	newer := networkStatusSnapshot{Status: depositStatusDelayed, ObservedMS: 200}
	older := networkStatusSnapshot{Status: depositStatusStalled, ObservedMS: 100}
	require.NoError(t, store.save(t.Context(), ChainBSC, newer, depositNetworkTTL))
	require.NoError(t, store.save(t.Context(), ChainBSC, older, depositNetworkTTL))
	got, err := store.load(t.Context(), []string{ChainBSC, ChainEthereum})
	require.NoError(t, err)
	require.Equal(t, map[string]networkStatusSnapshot{ChainBSC: newer}, got)
	redisServer.FastForward(depositNetworkTTL)
	got, err = store.load(t.Context(), []string{ChainBSC})
	require.NoError(t, err)
	require.Empty(t, got)
}

// The published ETA has exactly one definition; these cases pin the formula and
// the clamp boundaries so a UI change cannot silently invent a different one.
func TestDepositETAFormula(t *testing.T) {
	int64Ptr := func(v int64) *int64 { return &v }
	for _, tc := range []struct {
		name          string
		chain         string
		confirmations int64
		instant       bool
		backlog       *int64
		want          *int64
	}{
		{"instant, caught up pays one scan period", ChainBSC, 3, true, int64Ptr(0), int64Ptr(5)},
		{"instant, backlog adds chain time", ChainBSC, 3, true, int64Ptr(10), int64Ptr(15)},
		{"final BSC needs depth × 3s", ChainBSC, 3, false, int64Ptr(0), int64Ptr(14)},
		{"final Ethereum needs depth × 12s", ChainEthereum, 64, false, int64Ptr(0), int64Ptr(773)},
		{"instant Ethereum ignores depth", ChainEthereum, 64, true, int64Ptr(0), int64Ptr(5)},
		{"floor is five seconds", "unknown-chain", 0, true, int64Ptr(0), int64Ptr(5)},
		{"ceiling is thirty minutes", ChainEthereum, 1000, false, int64Ptr(0), int64Ptr(1800)},
		{"no backlog measurement means null", ChainBSC, 3, true, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := depositETA(tc.chain, tc.confirmations, tc.instant, tc.backlog)
			if tc.want == nil {
				require.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			require.Equal(t, *tc.want, *got)
		})
	}
}

// Backlog is measured only from evidence this pass can prove; a missing cursor,
// an unmeasurable chain or a rewind must not produce a fabricated number.
func TestDepositBacklogSecondsRequiresEvidence(t *testing.T) {
	cursor := uint64(10_000)
	base := scanner.DepositScanObservation{Chain: ChainBSC, ConfirmedHead: cursor + 100, CursorBefore: cursor, Cursor: cursor, CanMeasure: true}
	seconds, ok := depositBacklogSeconds(base)
	require.True(t, ok)
	require.Equal(t, int64(300), seconds, "100 BSC blocks at 3s")

	for name, observation := range map[string]scanner.DepositScanObservation{
		"unmeasurable": {Chain: ChainBSC, ConfirmedHead: cursor + 100, Cursor: cursor},
		"no cursor":    {Chain: ChainBSC, ConfirmedHead: cursor + 100, CanMeasure: true},
		"no head":      {Chain: ChainBSC, Cursor: cursor, CanMeasure: true},
		"rewind":       {Chain: ChainBSC, ConfirmedHead: cursor, Cursor: cursor + 100, CanMeasure: true},
	} {
		_, ok := depositBacklogSeconds(observation)
		require.False(t, ok, name)
	}
}

// The network-status row carries instant_credit, required_confirmations and the
// ETA derived from the same fresh snapshot the status uses.
func TestDepositNetworkStatusPublishesETA(t *testing.T) {
	int64Ptr := func(v int64) *int64 { return &v }
	observed := time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name         string
		raw          string
		backlogKnown bool
		backlogSec   int64
		wantEta      *int64
		instant      bool
		wantRequired int64
	}{
		{
			name:         "instant network pays backlog plus one pass",
			raw:          `{"enabled":true,"chains":["bsc"]}`,
			backlogKnown: true, backlogSec: 10, instant: true, wantEta: int64Ptr(15), wantRequired: 3,
		},
		{
			name:         "stored switch off falls back to confirmation depth",
			raw:          `{"enabled":true,"chains":["bsc"],"instant_credit_enabled":false}`,
			backlogKnown: true, backlogSec: 10, instant: false, wantEta: int64Ptr(24), wantRequired: 3,
		},
		{
			name:         "missing backlog measurement publishes null ETA",
			raw:          `{"enabled":true,"chains":["bsc"]}`,
			backlogKnown: false, instant: true, wantEta: nil, wantRequired: 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy, ok := decodeNetworkStatusPolicy(tc.raw, observed.Add(-time.Minute))
			require.True(t, ok)
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			mock.ExpectQuery("SELECT value, updated_at FROM settings WHERE key").
				WithArgs(SettingKeyCryptoConfig).
				WillReturnRows(sqlmock.NewRows([]string{"value", "updated_at"}).AddRow(tc.raw, observed.Add(-time.Minute)))
			mock.ExpectClose()
			t.Cleanup(func() {
				require.NoError(t, db.Close())
				require.NoError(t, mock.ExpectationsWereMet())
			})
			store := &networkSnapshotMemory{values: map[string]networkStatusSnapshot{
				ChainBSC: {
					Status: depositStatusAvailable, ObservedMS: observed.UnixMilli(),
					EvidenceMS: observed.Add(-time.Second).UnixMilli(), Fingerprint: policy.fingerprint,
					BacklogSec: tc.backlogSec, BacklogKnown: tc.backlogKnown,
				},
			}}
			s := &Service{db: db, depositNetworkStatus: newDepositNetworkMonitor(store, func() time.Time { return observed.Add(time.Second) })}
			report := s.DepositNetworkStatus(context.Background())
			var row DepositNetworkStatusRow
			for _, candidate := range report.Networks {
				if candidate.Chain == ChainBSC {
					row = candidate
				}
			}
			require.Equal(t, tc.instant, row.InstantCredit)
			require.NotNil(t, row.RequiredConfirmations)
			require.Equal(t, tc.wantRequired, *row.RequiredConfirmations)
			if tc.backlogKnown {
				require.NotNil(t, row.BacklogSeconds)
				require.Equal(t, tc.backlogSec, *row.BacklogSeconds)
			} else {
				require.Nil(t, row.BacklogSeconds)
			}
			if tc.wantEta == nil {
				require.Nil(t, row.EtaSeconds)
			} else {
				require.NotNil(t, row.EtaSeconds)
				require.Equal(t, *tc.wantEta, *row.EtaSeconds)
			}
		})
	}
}

// A paused network keeps its configuration visible but publishes no measurement.
func TestDepositNetworkStatusPausedRowsHaveNoETA(t *testing.T) {
	observed := time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC)
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	mock.ExpectQuery("SELECT value, updated_at FROM settings WHERE key").
		WithArgs(SettingKeyCryptoConfig).
		WillReturnRows(sqlmock.NewRows([]string{"value", "updated_at"}).AddRow(`{"enabled":false}`, observed.Add(-time.Minute)))
	mock.ExpectClose()
	t.Cleanup(func() {
		require.NoError(t, db.Close())
		require.NoError(t, mock.ExpectationsWereMet())
	})
	s := &Service{db: db, depositNetworkStatus: newDepositNetworkMonitor(&networkSnapshotMemory{}, func() time.Time { return observed })}
	report := s.DepositNetworkStatus(context.Background())
	for _, row := range report.Networks {
		require.Equal(t, depositStatusPaused, row.Status)
		require.Nil(t, row.BacklogSeconds)
		require.Nil(t, row.EtaSeconds)
		require.NotNil(t, row.RequiredConfirmations, "configured depth stays visible while paused")
	}
}
