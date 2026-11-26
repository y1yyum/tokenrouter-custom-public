package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lib/pq"
	"github.com/shopspring/decimal"

	"github.com/TokenFlux/TokenRouter/internal/billing"
)

// wallet_batch.go combines concurrent personal wallet debits for the same
// payer. A payer's runner takes up to one batch of already waiting callers
// without an intentional delay. A lone caller uses the ordinary transaction.
// Subscription, team and counter commands also use the ordinary path to
// preserve their independent writes and lock ordering.
//
// The batch inserts original request identities and checks the archive on a
// fresh statement snapshot before updating one wallet row. Each caller gets
// its exact place in that wallet's debit sequence after commit. Before a
// commit attempt, any failed batch leaves callers free to apply their original
// commands individually. A caller can cancel while waiting to be selected.

// Batch limits bound the claim count and transaction time before individual
// callers recover with their original contexts after a definite failure.
const (
	usageWalletBatchSize    = 32
	usageWalletBatchTimeout = 5 * time.Second
)

// usageWalletBatcher owns one queue and runner for each currently active payer.
type usageWalletBatcher struct {
	mu sync.Mutex // protects queue membership and runner ownership
	// pending maps payers to queued calls. An empty entry tells callers that
	// the runner owns a batch and will return to collect later arrivals.
	pending map[int64][]*usageWalletBatchCall // pending calls by payer
}

// usageWalletBatchCall carries one normalized command and its private reply.
type usageWalletBatchCall struct {
	ctx   context.Context              // individual caller context
	cmd   *billing.UsageBillingCommand // normalized command owned by caller
	debit float64                      // individually quantized USD amount
	// taken arbitrates pending cancellation against selection. The winner
	// owns completion, and the runner can access cmd only after winning.
	taken atomic.Bool // selection or cancellation claim
	// reply is buffered so the runner can publish a completed transaction and
	// move on without depending on when the waiting caller is scheduled.
	reply chan usageWalletBatchReply // private settlement reply
}

// usageWalletBatchReply carries a result, a terminal error or a fallback decision.
type usageWalletBatchReply struct {
	result *billing.UsageBillingApplyResult // committed individual wallet result
	err    error                            // terminal individual caller error
}

// usageWalletBatchClaims retains the SQL input arrays in the caller order.
type usageWalletBatchClaims struct {
	requestIDs   []string // original request identity strings
	keyIDs       []int64  // original API Key identity numbers
	fingerprints []string // original request identity fingerprints
}

// usageWalletBatchDebit returns a positive per-request quantized debit only
// for a personal balance-mode command without team, Key or provider effects.
// Automatic funding still needs the subscription lookup under the payer lock.
func usageWalletBatchDebit(cmd *billing.UsageBillingCommand) float64 {
	if cmd.APIKeyBillingMode != billing.APIKeyBillingModeBalance || cmd.UserID <= 0 || cmd.TeamID != nil {
		return 0
	}
	if cmd.ActorUserID != 0 && cmd.ActorUserID != cmd.UserID {
		return 0
	}
	if cmd.APIKeyQuotaCost > 0 || cmd.APIKeyRateLimitCost > 0 || cmd.ProviderQuotaCost > 0 {
		return 0
	}
	amount, _, _ := billing.SubscriptionAllocationRequest(cmd)
	cost := usageBillingWalletCost(cmd, amount)
	if cost <= 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
		return 0
	}
	return billing.QuantizeUsageBillingAmount(cost)
}

// enqueueUsageWallet assigns a normalized command to its payer's runner and
// waits for its own outcome. Cancellation can release a pending caller if the
// runner has not selected it. Once selected, the caller keeps ownership of the
// command until a result, a transaction error or a safe fallback is reported.
// A failed attempt that never tried to commit can reuse the same identity on
// the individual path, which applies the caller's original context.
func (r *SettlementStore) enqueueUsageWallet(ctx context.Context, cmd *billing.UsageBillingCommand, debit float64) (*billing.UsageBillingApplyResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	call := &usageWalletBatchCall{ctx: ctx, cmd: cmd, debit: debit, reply: make(chan usageWalletBatchReply, 1)}
	userID := cmd.UserID
	batches := &r.walletBatches
	batches.mu.Lock()
	if batches.pending == nil {
		batches.pending = make(map[int64][]*usageWalletBatchCall)
	}
	pending, running := batches.pending[userID]
	// Queue membership and runner ownership change under one lock so an arrival cannot miss the retiring runner.
	batches.pending[userID] = append(pending, call)
	batches.mu.Unlock()
	if !running {
		go r.flushUsageWallet(userID)
	}

	var reply usageWalletBatchReply
	select {
	case reply = <-call.reply:
	case <-ctx.Done():
		// A selected call can already be part of a database write, so its outcome still belongs to the runner.
		if call.taken.CompareAndSwap(false, true) {
			return nil, ctx.Err()
		}
		reply = <-call.reply
	}
	if reply.result == nil && reply.err == nil {
		return r.applyUsageBillingWithRetry(ctx, cmd)
	}
	return reply.result, reply.err
}

// flushUsageWallet drains one payer until its queue is empty. Each selection
// claims up to the batch limit and discards calls canceled before selection.
// Group attempts use their own timeout so a caller cannot cancel another
// selected caller's write. Replies are buffered so completed callers cannot
// hold up the payer's next debit.
func (r *SettlementStore) flushUsageWallet(userID int64) {
	var calls []*usageWalletBatchCall
	retired := false
	// If the runner exits unexpectedly, selected callers need a visible error and pending callers need a safe route to individual settlement.
	defer func() {
		if retired {
			return
		}
		err := fmt.Errorf("usage wallet batch runner stopped: %v", recover())
		r.abortUsageWalletRunner(userID, calls, err)
	}()

	for {
		var running bool
		calls, running = r.walletBatches.takeUsageWalletCalls(userID)
		if !running {
			retired = true
			return
		}
		r.settleUsageWalletCalls(userID, calls)
	}
}

// settleUsageWalletCalls releases canceled selections and chooses the individual
// path for a lone caller or a shared transaction for multiple callers. It
// delivers a per-call outcome before the runner takes another batch.
func (r *SettlementStore) settleUsageWalletCalls(userID int64, calls []*usageWalletBatchCall) {
	active := calls[:0]
	for _, call := range calls {
		if err := call.ctx.Err(); err != nil {
			call.reply <- usageWalletBatchReply{err: err}
			continue
		}
		active = append(active, call)
	}
	switch len(active) {
	case 0:
		return
	case 1:
		call := active[0]
		result, err := r.applyUsageBillingWithRetry(call.ctx, call.cmd)
		call.reply <- usageWalletBatchReply{result: result, err: err}
	default:
		results, err := r.applyUsageWalletBatch(userID, active)
		for i, call := range active {
			reply := usageWalletBatchReply{err: err}
			if results != nil {
				reply.result = results[i]
			}
			call.reply <- reply
		}
	}
}

// abortUsageWalletRunner releases a failed runner's payer slot and every
// waiting caller. Selected outcomes may be uncertain and receive the error.
// Calls still in the queue have never been written by this runner, so they
// can fall back safely without waiting for a new runner.
func (r *SettlementStore) abortUsageWalletRunner(userID int64, active []*usageWalletBatchCall, err error) {
	batches := &r.walletBatches
	batches.mu.Lock()
	pending := batches.pending[userID]
	delete(batches.pending, userID)
	batches.mu.Unlock()

	// An active caller might already have a reply. Keep cleanup independent of whether that caller has received it.
	for _, call := range active {
		select {
		case call.reply <- usageWalletBatchReply{err: err}:
		default:
		}
	}
	for _, call := range pending {
		if call.taken.CompareAndSwap(false, true) {
			call.reply <- usageWalletBatchReply{}
		}
	}
}

// takeUsageWalletCalls detaches a bounded prefix and claims its live callers.
// An empty queue retires the current runner under the same lock used to add
// calls. If pending cancellation won a call, this method can discard its
// entry without accessing the caller-owned command.
func (b *usageWalletBatcher) takeUsageWalletCalls(userID int64) ([]*usageWalletBatchCall, bool) {
	b.mu.Lock()
	pending := b.pending[userID]
	if len(pending) == 0 {
		delete(b.pending, userID)
		b.mu.Unlock()
		return nil, false
	}
	count := min(len(pending), usageWalletBatchSize)
	calls := make([]*usageWalletBatchCall, 0, count)
	for _, call := range pending[:count] {
		if call.taken.CompareAndSwap(false, true) {
			calls = append(calls, call)
		}
	}
	clear(pending[:count])
	b.pending[userID] = pending[count:]
	b.mu.Unlock()
	return calls, true
}

// applyUsageWalletBatch writes distinct claims and one combined balance change.
// It prepares decimal totals before opening the transaction and reconstructs
// individual results after releasing the payer lock at commit. The batch
// uses a bounded transaction context independent of its callers. Empty
// results and error trigger individual fallback before Commit or after a
// definite server rejection. An uncertain commit error reaches each caller
// for replay using the same original identity.
func (r *SettlementStore) applyUsageWalletBatch(userID int64, calls []*usageWalletBatchCall) ([]*billing.UsageBillingApplyResult, error) {
	claims := usageWalletBatchClaims{
		requestIDs:   make([]string, len(calls)),
		keyIDs:       make([]int64, len(calls)),
		fingerprints: make([]string, len(calls)),
	}
	debits := make([]decimal.Decimal, len(calls))
	total := decimal.Zero
	for i, call := range calls {
		claims.requestIDs[i] = call.cmd.RequestID
		claims.keyIDs[i] = call.cmd.APIKeyID
		claims.fingerprints[i] = call.cmd.RequestFingerprint
		debits[i] = decimal.NewFromFloat(call.debit)
		total = total.Add(debits[i])
	}
	totalText := total.String()

	// The batch owns its deadline. An admitted caller's cancellation cannot terminate work for unrelated callers.
	ctx, cancel := context.WithTimeout(context.Background(), usageWalletBatchTimeout)
	defer cancel()
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, nil
	}
	// Rollback finishes before the runner signals fallback so individual retries can claim the original identities.
	defer func() { _ = tx.Rollback() }()
	finalBalance, err := writeUsageWalletBatch(ctx, tx, userID, claims, totalText)
	if err != nil {
		return nil, nil
	}
	// Every individual result depends on the same commit, including the precise post-debit balance it reports.
	if err := tx.Commit(); err != nil {
		if usageWalletCommitRejected(err) {
			return nil, nil
		}
		return nil, err
	}
	return usageWalletBatchResults(calls, debits, finalBalance)
}

// usageWalletCommitRejected recognizes server errors that establish that the
// SQL transaction rolled back. An unknown-completion state or transport error
// leaves the outcome uncertain and must reach the caller without fallback.
func usageWalletCommitRejected(err error) bool {
	var pgErr *pq.Error
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code.Class() == "23" || (pgErr.Code.Class() == "40" && pgErr.Code != "40003")
}

// writeUsageWalletBatch inserts original identity and fingerprint pairs in a
// consistent order, then uses a second statement to check every archived key
// and debit the wallet once. If any identity is already present, the caller
// rolls back the batch and individual settlement performs the original replay
// or conflict checks. The fresh archive snapshot is required because a live
// insert can wait while retention moves an older charge out of the live table.
func writeUsageWalletBatch(ctx context.Context, tx *sql.Tx, userID int64, claims usageWalletBatchClaims, total string) (string, error) {
	// The same key order across instances avoids reversing unique-index waits when their batches share identities.
	claimed, err := tx.ExecContext(ctx, `
		INSERT INTO usage_billing_dedup (request_id, api_key_id, request_fingerprint)
		SELECT request_id, api_key_id, request_fingerprint
		FROM unnest($1::text[], $2::bigint[], $3::text[]) AS claims(request_id, api_key_id, request_fingerprint)
		ORDER BY request_id, api_key_id
		ON CONFLICT (request_id, api_key_id) DO NOTHING
	`, pq.Array(claims.requestIDs), pq.Array(claims.keyIDs), pq.Array(claims.fingerprints))
	if err != nil {
		return "", err
	}
	inserted, err := claimed.RowsAffected()
	if err != nil {
		return "", err
	}
	if inserted != int64(len(claims.requestIDs)) {
		return "", billing.ErrUsageBillingRequestConflict
	}

	// A stored NaN cannot produce an ordered decimal balance sequence, so the individual path handles it.
	var finalBalanceText string
	err = tx.QueryRowContext(ctx, `
		UPDATE users
		SET balance = balance - $4::numeric, updated_at = NOW()
		WHERE id = $3 AND deleted_at IS NULL AND balance <> 'NaN'::numeric AND NOT EXISTS (
			SELECT 1
			FROM unnest($1::text[], $2::bigint[]) AS claims(request_id, api_key_id)
			JOIN usage_billing_dedup_archive USING (request_id, api_key_id)
		)
		RETURNING balance::text
	`, pq.Array(claims.requestIDs), pq.Array(claims.keyIDs), userID, total).Scan(&finalBalanceText)
	return finalBalanceText, err
}

// usageWalletBatchResults reconstructs balances after commit, retaining
// decimal precision through the last addition. An unexpected parse error
// reports a committed transaction with unavailable results, preventing
// automatic fallback after the actual wallet write has committed.
func usageWalletBatchResults(calls []*usageWalletBatchCall, debits []decimal.Decimal, finalBalanceText string) ([]*billing.UsageBillingApplyResult, error) {
	finalBalance, err := decimal.NewFromString(finalBalanceText)
	if err != nil {
		return nil, err
	}

	results := make([]*billing.UsageBillingApplyResult, len(calls))
	// Walk backward from the stored balance so each caller observes its own debit, even when the batch crosses into debt.
	for i := len(calls) - 1; i >= 0; i-- {
		result := &billing.UsageBillingApplyResult{Applied: true}
		recordUsageBillingWallet(result, finalBalance.InexactFloat64(), calls[i].debit)
		recordUsageBillingEffectiveRate(calls[i].cmd, result, calls[i].debit)
		results[i] = result
		finalBalance = finalBalance.Add(debits[i])
	}
	return results, nil
}
