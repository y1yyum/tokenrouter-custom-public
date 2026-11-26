package postgres

import (
	"context"
	"database/sql"
	"io"
	"math"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/TokenFlux/TokenRouter/internal/billing"
)

// The batch query patterns check that original identity arrays feed the claim
// and archive check, while wallet arithmetic appears in exactly one statement.
const (
	usageWalletBatchClaimSQL = `(?s)^INSERT INTO usage_billing_dedup.*unnest.*ORDER BY request_id, api_key_id.*ON CONFLICT.*DO NOTHING`
	usageWalletBatchDebitSQL = `(?s)^UPDATE users\s+SET balance = balance - \$4::numeric.*JOIN usage_billing_dedup_archive.*RETURNING balance::text`
)

// usageWalletPanicContext reproduces an abnormal runner exit during caller selection.
type usageWalletPanicContext struct {
	context.Context // ordinary cancellation behavior except for Err
}

// TestUsageWalletBatchCallerCancellation verifies that a pending cancellation
// exits promptly and that a selected caller cannot cancel the shared debit.
// Neither admitted caller receives a result while the debit is paused.
func TestUsageWalletBatchCallerCancellation(t *testing.T) {
	walletReached := make(chan struct{})
	walletReleased := make(chan struct{})
	matcher := sqlmock.QueryMatcherFunc(func(expected, actual string) error {
		if expected == usageWalletBatchDebitSQL {
			close(walletReached)
			<-walletReleased
		}
		return sqlmock.QueryMatcherRegexp.Match(expected, actual)
	})
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(matcher))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	store := &SettlementStore{db: db}
	store.walletBatches.pending = map[int64][]*usageWalletBatchCall{42: nil}

	mock.ExpectBegin()
	mock.ExpectExec(usageWalletBatchClaimSQL).WithArgs(`{"a","b"}`, `{7,8}`, `{"original a","original b"}`).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectQuery(usageWalletBatchDebitSQL).WithArgs(`{"a","b"}`, `{7,8}`, int64(42), "0.00015626").
		WillReturnRows(sqlmock.NewRows([]string{"balance"}).AddRow("-0.00007813"))
	mock.ExpectCommit()

	// Hold the synthetic runner slot until callers are queued in a controlled order.
	submit := func(ctx context.Context, requestID, fingerprint string, keyID int64, base, money float64, queued int) <-chan usageWalletBatchReply {
		t.Helper()
		reply := make(chan usageWalletBatchReply, 1)
		go func() {
			cmd := &billing.UsageBillingCommand{
				RequestID: requestID, RequestFingerprint: fingerprint, APIKeyID: keyID, UserID: 42,
				APIKeyBillingMode: billing.APIKeyBillingModeBalance, BaseAmountUSD: base,
				BillableAmountUSD: money, BalanceRateMultiplier: 0.000078125,
			}
			result, err := store.Apply(ctx, cmd)
			reply <- usageWalletBatchReply{result: result, err: err}
		}()
		require.Eventually(t, func() bool {
			store.walletBatches.mu.Lock()
			defer store.walletBatches.mu.Unlock()
			return len(store.walletBatches.pending[42]) == queued
		}, time.Second, time.Millisecond)
		return reply
	}

	pendingCtx, cancelPending := context.WithCancel(context.Background())
	t.Cleanup(cancelPending)
	pending := submit(pendingCtx, "pending", "unused", 6, 0, 1, 1)
	cancelPending()
	require.ErrorIs(t, (<-pending).err, context.Canceled)

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	t.Cleanup(cancelFirst)
	first := submit(firstCtx, "a", "original a", 7, 1, 0, 2)
	second := submit(context.Background(), "b", "original b", 8, math.NaN(), 0.000078125, 3)
	go store.flushUsageWallet(42)
	select {
	case <-walletReached:
	case <-time.After(time.Second):
		t.Fatal("wallet debit was not reached")
	}
	cancelFirst()
	require.Empty(t, first)
	require.Empty(t, second)
	close(walletReleased)

	firstReply := <-first
	require.NoError(t, firstReply.err)
	require.True(t, firstReply.result.Applied)
	require.Equal(t, 0.00007813, firstReply.result.BalanceAmountUSD)
	require.Zero(t, *firstReply.result.NewBalance)
	require.Equal(t, 0.00007813, *firstReply.result.EffectiveRateMultiplier)
	secondReply := <-second
	require.NoError(t, secondReply.err)
	require.True(t, secondReply.result.Applied)
	require.Equal(t, 0.00007813, secondReply.result.BalanceAmountUSD)
	require.Equal(t, -0.00007813, *secondReply.result.NewBalance)
	require.Nil(t, secondReply.result.EffectiveRateMultiplier)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestUsageWalletBatchFallbackBoundaries checks rollback on a live duplicate
// or archive hit. A PostgreSQL commit rejection permits fallback, while lost
// replies and unknown SQLSTATE outcomes preserve the original error.
func TestUsageWalletBatchFallbackBoundaries(t *testing.T) {
	calls := []*usageWalletBatchCall{
		{cmd: &billing.UsageBillingCommand{RequestID: "a", APIKeyID: 7, RequestFingerprint: "fa"}, debit: 1.0},
		{cmd: &billing.UsageBillingCommand{RequestID: "b", APIKeyID: 7, RequestFingerprint: "fb"}, debit: 2.0},
	}
	for _, test := range []struct {
		name       string
		inserted   int64
		archiveHit bool
		commitErr  error
		wantErr    error
	}{
		{name: "live_duplicate", inserted: 1},
		{name: "archive_hit", inserted: 2, archiveHit: true},
		{name: "deferred_check_rejection", inserted: 2, commitErr: &pq.Error{Code: "23514"}},
		{name: "serialization_failure", inserted: 2, commitErr: &pq.Error{Code: "40001"}},
		{name: "unknown_server_outcome", inserted: 2, commitErr: &pq.Error{Code: "40003"}, wantErr: new(pq.Error)},
		{name: "lost_commit_reply", inserted: 2, commitErr: io.ErrUnexpectedEOF, wantErr: io.ErrUnexpectedEOF},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			mock.ExpectBegin()
			mock.ExpectExec(usageWalletBatchClaimSQL).WithArgs(`{"a","b"}`, `{7,7}`, `{"fa","fb"}`).
				WillReturnResult(sqlmock.NewResult(0, test.inserted))
			if test.inserted == 1 {
				mock.ExpectRollback()
			} else {
				wallet := mock.ExpectQuery(usageWalletBatchDebitSQL).WithArgs(`{"a","b"}`, `{7,7}`, int64(42), "3")
				if test.archiveHit {
					wallet.WillReturnError(sql.ErrNoRows)
					mock.ExpectRollback()
				} else {
					wallet.WillReturnRows(sqlmock.NewRows([]string{"balance"}).AddRow("7.00000000"))
					mock.ExpectCommit().WillReturnError(test.commitErr)
				}
			}

			result, err := (&SettlementStore{db: db}).applyUsageWalletBatch(42, calls)
			require.Nil(t, result)
			if test.wantErr != nil {
				require.ErrorIs(t, err, test.commitErr)
			} else {
				require.NoError(t, err)
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

// TestUsageWalletBatchEligibility keeps subscription, team and counted work
// on the individual path. Zero, negative and NaN base amounts use legacy
// wallet pricing and leave the effective multiplier absent.
func TestUsageWalletBatchEligibility(t *testing.T) {
	for _, base := range []float64{0, -1, math.NaN()} {
		cmd := &billing.UsageBillingCommand{
			UserID: 42, APIKeyBillingMode: billing.APIKeyBillingModeBalance, BaseAmountUSD: base, BillableAmountUSD: 1,
		}
		require.Equal(t, 1.0, usageWalletBatchDebit(cmd))
		result := &billing.UsageBillingApplyResult{}
		recordUsageBillingEffectiveRate(cmd, result, 1)
		require.Nil(t, result.EffectiveRateMultiplier)
	}
	teamID := int64(2)
	for _, ineligible := range []billing.UsageBillingCommand{
		{APIKeyBillingMode: billing.APIKeyBillingModeAuto},
		{APIKeyBillingMode: billing.APIKeyBillingModeSubscription},
		{APIKeyBillingMode: billing.APIKeyBillingModeBalance, TeamID: &teamID},
		{APIKeyBillingMode: billing.APIKeyBillingModeBalance, ActorUserID: 7},
		{APIKeyBillingMode: billing.APIKeyBillingModeBalance, APIKeyQuotaCost: 1},
		{APIKeyBillingMode: billing.APIKeyBillingModeBalance, APIKeyRateLimitCost: 1},
		{APIKeyBillingMode: billing.APIKeyBillingModeBalance, ProviderQuotaCost: 1},
	} {
		ineligible.UserID = 42
		ineligible.BillableAmountUSD = 1
		require.Zero(t, usageWalletBatchDebit(&ineligible), "%+v", ineligible)
	}
}

// TestUsageWalletBatchRunnerFailureReleasesPayer makes a selected caller's
// context panic. The runner returns a visible error, releases pending work
// for safe fallback and clears the payer slot for later calls.
func TestUsageWalletBatchRunnerFailureReleasesPayer(t *testing.T) {
	calls := make([]*usageWalletBatchCall, usageWalletBatchSize+1)
	for i := range calls {
		calls[i] = &usageWalletBatchCall{ctx: context.Background(), reply: make(chan usageWalletBatchReply, 1)}
	}
	calls[0].ctx = usageWalletPanicContext{Context: context.Background()}
	store := &SettlementStore{}
	store.walletBatches.pending = map[int64][]*usageWalletBatchCall{42: append([]*usageWalletBatchCall(nil), calls...)}
	store.flushUsageWallet(42)

	for _, selected := range calls[:usageWalletBatchSize] {
		require.Len(t, selected.reply, 1)
		require.ErrorContains(t, (<-selected.reply).err, "runner stopped")
	}
	require.Len(t, calls[usageWalletBatchSize].reply, 1)
	require.Equal(t, usageWalletBatchReply{}, <-calls[usageWalletBatchSize].reply)
	require.Empty(t, store.walletBatches.pending)
}

// Err simulates an unexpected panic while checking the caller's cancellation.
func (usageWalletPanicContext) Err() error {
	panic("fixture context panic")
}
