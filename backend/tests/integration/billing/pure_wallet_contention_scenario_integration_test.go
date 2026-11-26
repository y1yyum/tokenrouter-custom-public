//go:build integration

package billing_test

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/TokenFlux/TokenRouter/internal/apikey"
	"github.com/TokenFlux/TokenRouter/internal/billing"
	"github.com/TokenFlux/TokenRouter/internal/gateway/completion"
	"github.com/TokenFlux/TokenRouter/internal/pkg/timezone"
	"github.com/TokenFlux/TokenRouter/internal/provider"
	"github.com/TokenFlux/TokenRouter/internal/routing/capability"
	usagepg "github.com/TokenFlux/TokenRouter/internal/usage/postgres"
)

// pure_wallet_contention_scenario_integration_test.go exercises the personal
// balance path in billing/funds.go and billing/postgres/settlement.go and the
// usage projection in gateway/completion/settlement.go. The same payer receives
// concurrent wallet-only charges alongside identical replays, conflicting raw
// amounts, archived replays, and an independent payer's failed commit.
//
// Each successful return includes the balance after its charge. Ordering by
// that reported balance lets the test reconstruct the wallet's debit sequence
// without assuming which goroutine finishes first. The usage repository stores
// each result's actual cost and allocation to verify that the persisted log
// agrees with its specific charge.

// pureWalletDebit pairs a committed result with its independently expected
// wallet debit in hundred-millionths of a dollar.
type pureWalletDebit struct {
	result      *billing.UsageBillingApplyResult // returned funding and balance
	amountUnits int64                            // expected charge in 1e-8 USD
}

// pureWalletWaitContext signals when settlement first starts observing cancellation.
type pureWalletWaitContext struct {
	context.Context               // wrapped caller cancellation and deadline
	waiting         chan struct{} // signal for the first cancellation wait
	once            sync.Once     // guards the single signal
}

// TestFundsSettlePureWalletContention reconciles each reported wallet transition
// and stored usage allocation while competing requests are being deduplicated.
// A separate payer's commit rejection leaves their claim and funds available for retry.
func TestFundsSettlePureWalletContention(t *testing.T) {
	for _, mode := range []string{billing.APIKeyBillingModeBalance, billing.APIKeyBillingModeAuto} {
		t.Run(mode, func(t *testing.T) {
			runPureWalletContention(t, mode)
		})
	}
}

// TestFundsSettlePureWalletQueuedCharges holds a wallet row while more callers
// enter settlement. A canceled pending call must leave no claim, and the other
// calls retain their individual debits and post-charge balances.
func TestFundsSettlePureWalletQueuedCharges(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	client := testEntClient(t)
	payerID := newContentionPayer(t)
	keyID := mustCreateApiKey(t, client, &apikey.APIKey{UserID: payerID, Key: uuid.NewString()}).ID
	providerID := mustCreateProvider(t, client, &provider.Record{
		Name: "wallet-queued-" + uuid.NewString(), Type: capability.ProviderTypeAPIKey,
	}).ID
	input := pureWalletInput(payerID, keyID, providerID, billing.APIKeyBillingModeBalance)
	pool := newContentionPool(t)
	funds := billing.NewFunds(newSettlementFixture(pool))
	writer := usagepg.NewUsageLogRepositoryWithSQL(client, directUsageExecutor{integrationDB}, timezone.NewCalendar(time.UTC))

	var settlementPID, blockerPID int
	require.NoError(t, pool.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&settlementPID))
	blocker, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	var workers sync.WaitGroup
	// Release the row and cancel workers before joining if an assertion fails,
	// so the fixture never tears down while a settlement call holds its connection.
	defer func() {
		err := blocker.Rollback()
		cancel()
		workers.Wait()
		if err != sql.ErrTxDone {
			require.NoError(t, err)
		}
	}()
	require.NoError(t, blocker.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID))
	_, err = blocker.ExecContext(ctx, "SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE", payerID)
	require.NoError(t, err)

	commands := []billing.UsageBillingCommand{
		pureWalletCommand(input, uuid.NewString(), 0.125000005),
		pureWalletCommand(input, uuid.NewString(), 0.250000005),
		pureWalletCommand(input, uuid.NewString(), 0.010000004),
		pureWalletCommand(input, uuid.NewString(), 0.030000005),
	}
	outcomes := make([]usageBillingApplyOutcome, len(commands))
	// Confirm the first call is blocked before starting the others. Their
	// commands then reach settlement while no wallet update can finish.
	workers.Go(func() {
		outcomes[0].result, outcomes[0].err = funds.Settle(ctx, &commands[0])
	})
	waitForContentionBlock(t, ctx, settlementPID, blockerPID)
	for i := 1; i < len(commands); i++ {
		waiting := &pureWalletWaitContext{Context: ctx, waiting: make(chan struct{})}
		// Observe caller entry before starting the next one, so releasing the
		// blocker later leaves several distinct, valid debits ready together.
		workers.Go(func() {
			outcomes[i].result, outcomes[i].err = funds.Settle(waiting, &commands[i])
		})
		select {
		case <-waiting.waiting:
		case <-ctx.Done():
			t.Fatal("wallet caller never entered settlement")
		}
	}

	canceled := pureWalletCommand(input, uuid.NewString(), 0.50)
	pendingCtx, cancelPending := context.WithCancel(ctx)
	defer cancelPending()
	pending := &pureWalletWaitContext{Context: pendingCtx, waiting: make(chan struct{})}
	pendingDone := make(chan usageBillingApplyOutcome, 1)
	// Cancel only after the caller reaches its wait and while the first debit
	// still owns the settlement connection or the payer's active runner.
	workers.Go(func() {
		result, err := funds.Settle(pending, &canceled)
		pendingDone <- usageBillingApplyOutcome{result: result, err: err}
	})
	select {
	case <-pending.waiting:
	case <-ctx.Done():
		t.Fatal("cancelable wallet caller never entered settlement")
	}
	cancelPending()
	select {
	case outcome := <-pendingDone:
		require.ErrorIs(t, outcome.err, context.Canceled)
		require.Nil(t, outcome.result)
	case <-ctx.Done():
		t.Fatal("pending wallet caller did not return on cancellation")
	}

	// Release the wallet only after the canceled caller has returned, so any
	// claim it leaves behind is attributable to a cancellation arbitration error.
	require.NoError(t, blocker.Commit())
	workers.Wait()
	wantUnits := [4]int64{12_500_001, 25_000_001, 1_000_000, 3_000_001}
	var debits []pureWalletDebit
	for i, outcome := range outcomes {
		require.NoError(t, outcome.err)
		require.NotNil(t, outcome.result)
		require.True(t, outcome.result.Applied)
		debit := pureWalletDebit{result: outcome.result, amountUnits: wantUnits[i]}
		assertPureWalletDebit(t, ctx, writer, commands[i], debit)
		assertContentionClaim(t, ctx, keyID, commands[i].RequestID, commands[i].RequestFingerprint, "")
		debits = append(debits, debit)
	}
	assertContentionClaim(t, ctx, keyID, canceled.RequestID, "", "")
	assertPureWalletBalances(t, payerID, debits)

	// Report persisted transaction identities so candidate runs can confirm
	// the queued calls exercised a combined write as well as the public contract.
	lastRequests := []string{commands[1].RequestID, commands[2].RequestID, commands[3].RequestID}
	var txCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT xmin::text) FROM usage_billing_dedup
		WHERE api_key_id = $1 AND request_id = ANY($2)
	`, keyID, pq.Array(lastRequests)).Scan(&txCount))
	t.Logf("%d queued wallet claims committed in %d transaction(s)", len(lastRequests), txCount)
}

// runPureWalletContention runs a personal settlement batch in the selected
// Key mode. The payer has no subscriptions, so both the explicit balance
// and default automatic modes draw the full charge from the wallet.
func runPureWalletContention(t *testing.T, mode string) {
	t.Helper()
	ctx := t.Context()
	client := testEntClient(t)
	payerID, failingPayerID := newContentionPayer(t), newContentionPayer(t)
	keyID := mustCreateApiKey(t, client, &apikey.APIKey{UserID: payerID, Key: "sk-wallet-" + uuid.NewString()}).ID
	failingKeyID := mustCreateApiKey(t, client, &apikey.APIKey{UserID: failingPayerID, Key: "sk-wallet-" + uuid.NewString()}).ID
	require.NoError(t, client.APIKey.UpdateOneID(keyID).SetBillingMode(mode).Exec(ctx))
	require.NoError(t, client.APIKey.UpdateOneID(failingKeyID).SetBillingMode(mode).Exec(ctx))
	providerID := mustCreateProvider(t, client, &provider.Record{
		Name: "wallet-only-" + uuid.NewString(),
		Type: capability.ProviderTypeAPIKey,
	}).ID
	funds := billing.NewFunds(newSettlementFixture(integrationDB))
	writer := usagepg.NewUsageLogRepositoryWithSQL(client, directUsageExecutor{integrationDB}, timezone.NewCalendar(time.UTC))
	input := pureWalletInput(payerID, keyID, providerID, mode)

	archived := pureWalletCommand(input, uuid.NewString(), 0.250000005)
	settled := archived
	result, err := funds.Settle(ctx, &settled)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Applied)
	require.Len(t, settled.RequestFingerprint, 64)
	archivedDebit := pureWalletDebit{result: result, amountUnits: 25_000_001}
	assertPureWalletDebit(t, ctx, writer, settled, archivedDebit)
	cutoff := ageContentionClaim(t, settled)
	require.NoError(t, newAggregationFixture(integrationDB).CleanupUsageBillingDedup(ctx, cutoff))

	// Each pair differs before rounding and has the same eight-place debit.
	rawCharges := [][2]float64{
		{0.010000005, 0.010000006},
		{0.030000004, 0.030000003},
		{0.170000005, 0.170000006},
		{0.000026400, 0.000026401},
	}
	wantUnits := []int64{1_000_001, 3_000_000, 17_000_001, 2_640}
	var commands []billing.UsageBillingCommand
	// Four rounds give sixteen fresh identities on the same payer.
	for range 4 {
		for _, charges := range rawCharges {
			requestID := uuid.NewString()
			cmd := pureWalletCommand(input, requestID, charges[0])
			conflict := pureWalletCommand(input, requestID, charges[1])
			commands = append(commands, cmd, conflict, cmd)
		}
	}
	archivedConflict := pureWalletCommand(input, archived.RequestID, 0.250000006)
	failingInput := pureWalletInput(failingPayerID, failingKeyID, providerID, mode)
	failing := pureWalletCommand(failingInput, uuid.NewString(), 0.125)
	installContentionCommitFault(t, failingPayerID)
	freshCount := len(commands)
	commands = append(commands, archived, archivedConflict, failing)
	outcomes := settleConcurrently(t, funds, commands)

	debits := []pureWalletDebit{archivedDebit}
	for i := 0; i < freshCount; i += 3 {
		winner := assertPureWalletReplayGroup(t, commands[i:i+3], outcomes[i:i+3])
		cmd := commands[i+winner]
		amountUnits := wantUnits[(i/3)%len(wantUnits)]
		debit := pureWalletDebit{result: outcomes[i+winner].result, amountUnits: amountUnits}
		assertPureWalletDebit(t, ctx, writer, cmd, debit)
		assertContentionClaim(t, ctx, keyID, cmd.RequestID, cmd.RequestFingerprint, "")
		debits = append(debits, debit)
	}
	require.NoError(t, outcomes[freshCount].err)
	require.Equal(t, &billing.UsageBillingApplyResult{}, outcomes[freshCount].result)
	require.ErrorIs(t, outcomes[freshCount+1].err, billing.ErrUsageBillingRequestConflict)
	require.Nil(t, outcomes[freshCount+1].result)
	assertContentionClaim(t, ctx, keyID, archived.RequestID, "", settled.RequestFingerprint)

	require.ErrorContains(t, outcomes[freshCount+2].err, "injected billing commit failure")
	require.Nil(t, outcomes[freshCount+2].result)
	assertContentionClaim(t, ctx, failingKeyID, failing.RequestID, "", "")
	assertContentionBalance(t, ctx, failingPayerID, "5.00000000")
	assertPureWalletBalances(t, payerID, debits)
	assertContentionKey(t, ctx, keyID, "0.00000000", billing.StatusActive)
	assertContentionProvider(t, ctx, providerID, "0.00000000", 0)

	removeContentionCommitFault(t)
	// Retry only the failed payer's original identity. The healthy payer's
	// reported debit sequence must still reconcile after this independent commit.
	result, err = funds.Settle(ctx, &failing)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Applied)
	require.NotNil(t, result.NewBalance)
	require.Equal(t, 4.875, *result.NewBalance)
	assertPureWalletDebit(t, ctx, writer, failing, pureWalletDebit{result: result, amountUnits: 12_500_000})
	require.Len(t, failing.RequestFingerprint, 64)
	assertContentionClaim(t, ctx, failingKeyID, failing.RequestID, failing.RequestFingerprint, "")
	assertContentionBalance(t, ctx, failingPayerID, "4.87500000")
	assertPureWalletBalances(t, payerID, debits)
}

// pureWalletInput captures an unmetered personal Key and its executing provider.
// The Key supports quota updates but has no configured quota or rolling limit.
// The provider can count usage when configured and has no limits here.
func pureWalletInput(payerID, keyID, providerID int64, mode string) completion.SettlementInput {
	return completion.SettlementInput{
		User: &completion.PayerSnapshot{ID: payerID},
		APIKey: &completion.KeySnapshot{
			ID: keyID, BillingMode: mode,
		},
		Provider: &completion.ProviderSnapshot{
			ID: providerID, Type: capability.ProviderTypeAPIKey, QuotaEligible: true,
		},
		BalanceRateMultiplier:  1,
		ProviderRateMultiplier: 1,
		QuotaUpdates:           true,
	}
}

// pureWalletCommand uses the gateway command builder to capture raw usage cost
// and its fingerprint. The input is copied so each request sets pricing facts
// independently while the payer, Key, and provider snapshots are shared.
func pureWalletCommand(input completion.SettlementInput, requestID string, rawCharge float64) billing.UsageBillingCommand {
	input.Cost = &completion.CostBreakdown{TotalCost: rawCharge, ActualCost: rawCharge}
	return *completion.BuildCommand(requestID, &completion.UsageLog{Model: "wallet-contention"}, &input)
}

// assertPureWalletReplayGroup classifies a conflicting pair and an identical
// copy by their actual winner. Callers pass a single identity at a time
// and retain the winning index for log and wallet verification.
func assertPureWalletReplayGroup(t *testing.T, commands []billing.UsageBillingCommand, outcomes []usageBillingApplyOutcome) int {
	t.Helper()
	var applied []int
	for i, outcome := range outcomes {
		if outcome.result != nil && outcome.result.Applied {
			applied = append(applied, i)
		}
	}
	require.Len(t, applied, 1, "request %s", commands[0].RequestID)
	winner := applied[0]
	require.Len(t, commands[winner].RequestFingerprint, 64)
	for i, outcome := range outcomes {
		switch {
		case i == winner:
			require.NoError(t, outcome.err)
		case commands[i].BaseAmountUSD == commands[winner].BaseAmountUSD:
			require.NoError(t, outcome.err)
			require.Equal(t, &billing.UsageBillingApplyResult{}, outcome.result)
		default:
			require.ErrorIs(t, outcome.err, billing.ErrUsageBillingRequestConflict)
			require.Nil(t, outcome.result)
		}
	}
	return winner
}

// assertPureWalletDebit checks one applied result and persists its production
// usage projection. The caller supplies integer monetary units so the SQL
// comparison can detect a ninth decimal preserved in either stored amount.
func assertPureWalletDebit(t *testing.T, ctx context.Context, writer *usagepg.Store, cmd billing.UsageBillingCommand, debit pureWalletDebit) {
	t.Helper()
	// Convert the fixture's integer monetary units to the result's dollar unit.
	want := float64(debit.amountUnits) / 100_000_000
	require.NotNil(t, debit.result.NewBalance)
	require.Equal(t, want, debit.result.BalanceAmountUSD)
	require.Equal(t, []billing.BillingAllocation{{Type: billing.BillingAllocationTypeBalance, AmountUSD: want}}, debit.result.BillingAllocations)
	log := &completion.UsageLog{
		UserID: cmd.UserID, APIKeyID: cmd.APIKeyID, ProviderID: cmd.ProviderID,
		RequestID: uuid.NewString(), BillingKey: cmd.RequestID, Model: cmd.Model,
		ActualCost: cmd.BillableAmountUSD, CreatedAt: time.Now().UTC(),
	}
	// Project and persist the actual funding result so queryable costs and
	// allocation metadata can be compared with the expected amount for this request.
	completion.ApplyResultToLog(log, debit.result)
	inserted, err := writer.Create(ctx, log)
	require.NoError(t, err)
	require.True(t, inserted)
	var actualCost, walletAmount, allocationAmount, allocationType string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT actual_cost::text, balance_amount_usd::text,
		       billing_allocations->0->>'amount_usd', billing_allocations->0->>'type'
		FROM usage_logs WHERE billing_key = $1 AND api_key_id = $2
	`, cmd.RequestID, cmd.APIKeyID).Scan(&actualCost, &walletAmount, &allocationAmount, &allocationType))
	wantDecimal := pureWalletDecimal(debit.amountUnits)
	require.Equal(t, wantDecimal+"00", actualCost)
	require.Equal(t, wantDecimal+"00", walletAmount)
	require.Equal(t, strings.TrimRight(wantDecimal, "0"), allocationAmount)
	require.Equal(t, string(billing.BillingAllocationTypeBalance), allocationType)
}

// assertPureWalletBalances sorts committed debits by their reported balances
// and checks each transition from the known starting wallet. Each charge is
// positive, so an equal or skipped balance cannot fit the expected sequence.
func assertPureWalletBalances(t *testing.T, payerID int64, debits []pureWalletDebit) {
	t.Helper()
	// Concurrent callers can return out of commit order. Sorting by post-charge
	// balance reconstructs the debit sequence for this one strictly decreasing wallet.
	slices.SortFunc(debits, func(a, b pureWalletDebit) int {
		return cmp.Compare(*b.result.NewBalance, *a.result.NewBalance)
	})
	// The fixture starts with five dollars expressed in eight-place units.
	var remaining int64 = 500_000_000
	for _, debit := range debits {
		remaining -= debit.amountUnits
		require.Equal(t, float64(remaining)/100_000_000, *debit.result.NewBalance)
	}
	assertContentionBalance(t, t.Context(), payerID, pureWalletDecimal(remaining))
}

// pureWalletDecimal formats a nonnegative fixture amount as eight-place dollar text.
func pureWalletDecimal(units int64) string {
	// One dollar occupies one hundred million ledger units.
	const unitsPerDollar = 100_000_000
	return fmt.Sprintf("%d.%08d", units/unitsPerDollar, units%unitsPerDollar)
}

// Done reports settlement's first cancellation wait and forwards cancellation.
// Multiple observations retain the same signal and underlying context channel.
func (c *pureWalletWaitContext) Done() <-chan struct{} {
	// Close the signal only once because SQL may observe the same caller context
	// while acquiring a connection and again while waiting for statement results.
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}
