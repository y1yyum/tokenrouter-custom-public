package scanner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	_ "modernc.org/sqlite"

	"github.com/TokenFlux/TokenRouter/internal/cryptopay/rpc"
)

type receiptBudgetFixture struct {
	mu       sync.Mutex
	stalled  string
	requests []string
	good     Transfer
}

// Capture the query actually issued by the worker, then evaluate its joins
// and window predicate on isolated in-memory fixtures. Only the PostgreSQL
// cast and interval syntax are translated; the selection logic is unchanged.
func creditedWindowQuery(t *testing.T) string {
	t.Helper()
	var query string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(func(_, actual string) error {
		query = actual
		return nil
	})))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		mock.ExpectClose()
		if err := db.Close(); err != nil {
			t.Errorf("close query capture db: %v", err)
		}
	}()
	mock.ExpectQuery("capture credited window").WillReturnRows(observationRows())
	if err := (&Worker{db: db}).recheckCredited(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	return strings.NewReplacer(
		"d.amount::text", "CAST(d.amount AS TEXT)",
		"amount::text", "CAST(amount AS TEXT)",
		"NOW() - INTERVAL '1 hour'", "?",
	).Replace(query)
}

func TestCreditedRecheckWindowStartsAtOrderCompletion(t *testing.T) {
	query := creditedWindowQuery(t)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	ptr := func(value time.Time) *time.Time { return &value }
	stamp := func(value time.Time) string { return value.Format("2006-01-02 15:04:05") }
	tests := []struct {
		name                 string
		created              time.Time
		completed            *time.Time
		linked, missingOrder bool
		status, reason       string
		want                 bool
	}{
		{"pending three hours then just completed", now.Add(-3 * time.Hour), ptr(now.Add(-time.Minute)), true, false, "credited", "", true},
		{"old completion stays expired despite fresh retry", now.Add(-3 * time.Hour), ptr(now.Add(-2 * time.Hour)), true, false, "credited", "", false},
		{"legacy recent observation", now.Add(-10 * time.Minute), nil, false, false, "credited", "", true},
		{"legacy expired observation despite fresh retry", now.Add(-2 * time.Hour), nil, false, false, "credited", "", false},
		{"null completion falls back to recent creation", now.Add(-10 * time.Minute), nil, true, false, "credited", "", true},
		{"null completion preserves expired fallback", now.Add(-2 * time.Hour), nil, true, false, "credited", "", false},
		{"missing joined order preserves legacy fallback", now.Add(-10 * time.Minute), nil, true, true, "credited", "", true},
		{"unresolved missing receipt survives window", now.Add(-3 * time.Hour), ptr(now.Add(-2 * time.Hour)), true, false, "credited", missingReceiptPrefix + "2", true},
		{"completion at strict window boundary", now.Add(-3 * time.Hour), ptr(now.Add(-time.Hour)), true, false, "credited", "", false},
		{"pending row is not a credited recheck", now.Add(-3 * time.Hour), ptr(now.Add(-time.Minute)), true, false, "pending", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, err := sql.Open("sqlite", ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := db.Close(); err != nil {
					t.Errorf("close fixture db: %v", err)
				}
			}()
			db.SetMaxOpenConns(1)
			for _, schema := range []string{
				`CREATE TABLE payment_orders (id INTEGER PRIMARY KEY, completed_at TEXT, created_at TEXT, status TEXT, amount TEXT)`,
				`CREATE TABLE deposit_transactions (
					id INTEGER PRIMARY KEY, chain TEXT DEFAULT 'bsc', tx_hash TEXT, log_index INTEGER DEFAULT 0,
					from_address TEXT DEFAULT 'from', to_address TEXT DEFAULT 'to', token TEXT DEFAULT 'token',
					amount TEXT DEFAULT '1', block_number INTEGER DEFAULT 101, block_hash TEXT DEFAULT 'block',
					reason TEXT, status TEXT, created_at TEXT, last_attempt_at TEXT, order_id INTEGER,
					native_price_usd TEXT, native_price_at TEXT, native_price_source TEXT,
					provisional INTEGER NOT NULL DEFAULT 0, credited_at TEXT, revoked_at TEXT, revoke_reason TEXT)`,
			} {
				if _, err := db.ExecContext(t.Context(), schema); err != nil {
					t.Fatal(err)
				}
			}
			var orderID, completed any
			if tt.linked {
				orderID = int64(42)
				if tt.completed != nil {
					completed = stamp(*tt.completed)
				}
				if !tt.missingOrder {
					if _, err := db.ExecContext(t.Context(), `INSERT INTO payment_orders(id,completed_at,created_at,status,amount) VALUES(42,?,?,'COMPLETED','67')`, completed, stamp(tt.created)); err != nil {
						t.Fatal(err)
					}
				}
			}
			// Every fixture has a fresh attempt timestamp. That timestamp may
			// rotate the queue, but must never extend the risk window.
			if _, err := db.ExecContext(t.Context(), `INSERT INTO deposit_transactions(tx_hash,reason,status,created_at,last_attempt_at,order_id) VALUES(?,?,?,?,?,?)`,
				tt.name, tt.reason, tt.status, stamp(tt.created), stamp(now), orderID); err != nil {
				t.Fatal(err)
			}
			rows, err := db.QueryContext(t.Context(), query, stamp(now.Add(-time.Hour)))
			if err != nil {
				t.Fatal(err)
			}
			selected, err := readObservations(rows, "credited")
			if err != nil {
				t.Fatal(err)
			}
			if (len(selected) == 1) != tt.want {
				t.Fatalf("selected=%d, want eligible=%v", len(selected), tt.want)
			}
		})
	}
}

func (f *receiptBudgetFixture) pool(t *testing.T) *rpc.Pool {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var input struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if json.NewDecoder(request.Body).Decode(&input) != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		good := f.good
		f.mu.Unlock()
		var result any
		switch input.Method {
		case "eth_chainId":
			result = "0x38"
		case "eth_blockNumber":
			result = "0x6e"
		case "eth_getTransactionReceipt":
			var hash string
			if len(input.Params) != 1 || json.Unmarshal(input.Params[0], &hash) != nil {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			f.mu.Lock()
			f.requests = append(f.requests, hash)
			stall := hash == f.stalled
			f.mu.Unlock()
			if stall {
				<-request.Context().Done()
				return
			}
			result = map[string]any{
				"status": "0x1", "blockNumber": fmt.Sprintf("0x%x", good.BlockNumber),
				"blockHash": good.BlockHash, "transactionHash": good.TxHash, "logs": []evmLog{logFor(good)},
			}
		case "eth_getBlockByNumber":
			var tag string
			if len(input.Params) == 0 || json.Unmarshal(input.Params[0], &tag) != nil {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			if tag == "finalized" || tag == "safe" {
				result = map[string]any{"number": "0x6e"}
			} else {
				result = map[string]any{"number": fmt.Sprintf("0x%x", good.BlockNumber), "hash": good.BlockHash}
			}
		default:
			t.Errorf("unexpected RPC: %s", input.Method)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	}))
	t.Cleanup(server.Close)
	return rpc.NewPoolWithHTTPClient(server.URL, server.Client())
}

func (f *receiptBudgetFixture) requestOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func TestPendingReconcileSlowFirstReceiptRotatesAndCreditsNextRow(t *testing.T) {
	good, slow := testTransfer(), testTransfer()
	slow.TxHash = "0x" + strings.Repeat("66", 32)
	fixture := &receiptBudgetFixture{stalled: slow.TxHash, good: good}
	worker, mock, handler := workerFixture(t, newRPCFixture(good))
	worker.pools["bsc"] = fixture.pool(t)
	// The first row is older. A blocked receipt used to consume the entire
	// pass, cancel its UPDATE too, and remain first on every subsequent pass.
	mock.ExpectQuery(`(?s)WHERE status = 'pending'.*ORDER BY COALESCE\(last_attempt_at, created_at\), id.*LIMIT 50`).
		WillReturnRows(observationRows().
			AddRow(slow.Chain, slow.TxHash, slow.LogIndex, slow.From, slow.To, slow.Token, "1", slow.BlockNumber, slow.BlockHash, "", false).
			AddRow(good.Chain, good.TxHash, good.LogIndex, good.From, good.To, good.Token, "1", good.BlockNumber, good.BlockHash, "", false))
	expectObservationUpdate(mock, slow, "pending", "", "pending", "").
		WillDelayFor(20 * time.Millisecond).WillReturnResult(sqlmock.NewResult(0, 1))
	expectObservationUpdate(mock, good, "pending", "", "pending", "").WillReturnResult(sqlmock.NewResult(0, 1))
	started := time.Now()
	if err := worker.retryPending(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("timed-out receipt failure was hidden", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatal("one receipt consumed the whole reconciliation budget", elapsed)
	}
	if len(handler.processed) != 1 || handler.processed[0].TxHash != good.TxHash || len(handler.pending) != 0 {
		t.Fatal("a timed-out observation was credited or prevented the verified next payment", handler.processed)
	}
	if got := fixture.requestOrder(); len(got) != 2 || got[0] != slow.TxHash || got[1] != good.TxHash {
		t.Fatal("both rows did not receive real receipt verification", got)
	}
	// The persisted attempt makes this older never-attempted row eligible
	// before the stalled row on the next ordered DB page. No status changed
	// for the stalled row and no payment was fabricated just to rotate it.
	third := good
	third.TxHash = "0x" + strings.Repeat("77", 32)
	fixture.mu.Lock()
	fixture.good = third
	fixture.mu.Unlock()
	mock.ExpectQuery(`(?s)WHERE status = 'pending'.*ORDER BY COALESCE\(last_attempt_at, created_at\), id.*LIMIT 50`).
		WillReturnRows(observationRows().AddRow(third.Chain, third.TxHash, third.LogIndex, third.From, third.To, third.Token, "1", third.BlockNumber, third.BlockHash, "", false))
	expectObservationUpdate(mock, third, "pending", "", "pending", "").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := worker.retryPending(t.Context()); err != nil {
		t.Fatal("next ordered page could not make progress", err)
	}
	if len(handler.processed) != 2 || handler.processed[1].TxHash != third.TxHash {
		t.Fatal("rotation dropped the next real payment", handler.processed)
	}
}

func TestCreditedReconcileTimeoutReservesCASWithoutReorgOrCredit(t *testing.T) {
	transfer := testTransfer()
	fixture := &receiptBudgetFixture{stalled: transfer.TxHash, good: transfer}
	worker, mock, handler := workerFixture(t, newRPCFixture(transfer))
	worker.pools["bsc"] = fixture.pool(t)
	notifications := 0
	worker.SetReorgNotifier(func(string, string) { notifications++ })
	const oldReason = missingReceiptPrefix + "2"
	expectObservation(mock, transfer, "1", oldReason)
	expectObservationUpdate(mock, transfer, "credited", oldReason, "credited", missingReceiptPrefix+"0").
		WillDelayFor(20 * time.Millisecond).WillReturnResult(sqlmock.NewResult(0, 1))
	// A shorter caller deadline exercises the end-of-pass reserve, instead
	// of only the ordinary two-second row cap. The CAS must still complete.
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := worker.recheckCredited(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("inconclusive timeout was not returned", err)
	}
	if err := ctx.Err(); err != nil {
		t.Fatal("RPC consumed the persistence reserve", err)
	}
	if notifications != 0 || len(handler.processed) != 0 || len(handler.pending) != 0 {
		t.Fatal("timeout caused a false reorg notification or balance operation")
	}
	if got := fixture.requestOrder(); len(got) != 1 || got[0] != transfer.TxHash {
		t.Fatal("a fresh check time was written without actually attempting the receipt", got)
	}
}

func TestReconcileVerifiedPaymentStillRequiresObservationCAS(t *testing.T) {
	transfer := testTransfer()
	worker, mock, handler := workerFixture(t, newRPCFixture(transfer))
	expectObservation(mock, transfer, "1", "")
	expectObservationUpdate(mock, transfer, "pending", "", "pending", "").WillReturnResult(sqlmock.NewResult(0, 0))
	if err := worker.retryPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(handler.processed) != 0 || len(handler.pending) != 0 {
		t.Fatal("a changed observation was fulfilled after a failed compare-and-swap")
	}
}

func TestReconcileChainProbeKeepsHealthyPeerDespiteStalledEndpoint(t *testing.T) {
	transfer := testTransfer()
	fixture := newRPCFixture(transfer)
	worker, mock, handler := workerFixture(t, fixture)
	release := make(chan struct{})
	stalled := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		select {
		case <-request.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); stalled.Close() })
	healthy := worker.pools["bsc"]
	worker.pools["bsc"] = healthy.WithEndpoints(stalled.URL + "," + strings.Join(healthy.Endpoints(), ","))
	expectObservation(mock, transfer, "1", "")
	expectObservationUpdate(mock, transfer, "pending", "", "pending", "").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := worker.retryPending(t.Context()); err != nil {
		t.Fatal("per-receipt budget discarded a healthy chain endpoint", err)
	}
	if len(handler.processed) != 1 || handler.processed[0].TxHash != transfer.TxHash || fixture.callCount("eth_getTransactionReceipt") != 1 {
		t.Fatal("verified healthy peer did not perform the real receipt check and fulfillment")
	}
}

func deadLetterObservationFixture() depositObservation {
	return depositObservation{
		chain: "bsc", txHash: "0xabc", logIndex: 1,
		from: "0x" + strings.Repeat("22", 20), to: "0x" + strings.Repeat("33", 20),
		token: splitToken, amount: "1", blockNumber: 101,
		blockHash: "0x" + strings.Repeat("55", 32), status: "pending",
	}
}

// A pending deposit whose chain or token was removed from the configuration can
// never be verified again. It must leave the retry rotation with a durable
// reason instead of being rotated forever with an unchanged status.
func TestPendingObservationIsDeadLetteredWhenConfigurationNoLongerCoversIt(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		chains []ChainConfig
		pools  map[string]*rpc.Pool
	}{
		{
			name:   "chain removed",
			reason: deadLetterChainRemoved,
			// No chain, no pool: nothing can verify this observation again.
		},
		{
			name:   "token no longer watched",
			reason: deadLetterTokenRemoved,
			chains: []ChainConfig{{Chain: "bsc", Tokens: []string{"0x" + strings.Repeat("99", 20)}}},
			pools:  map[string]*rpc.Pool{"bsc": rpc.NewPoolWithHTTPClient("http://127.0.0.1:1", nil)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			w := &Worker{db: db, chains: tt.chains, pools: tt.pools}
			observation := deadLetterObservationFixture()
			mock.ExpectExec(`UPDATE deposit_transactions\s+SET status = 'dead_letter', reason = \$1, last_attempt_at = NOW\(\)\s+WHERE chain = \$2 AND tx_hash = \$3 AND log_index = \$4\s+AND status = 'pending' AND reason = \$5`).
				WithArgs(tt.reason, observation.chain, observation.txHash, observation.logIndex, observation.reason).
				WillReturnResult(sqlmock.NewResult(0, 1))

			if err := w.reconcile(t.Context(), []depositObservation{observation}); err != nil {
				t.Fatalf("dead-lettering failed: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A row that a concurrent credit already resolved must keep its status: the
// compare-and-set refuses it and the pass is not an error.
func TestDeadLetterDoesNotOverwriteAConcurrentlyResolvedDeposit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	w := &Worker{db: db}
	observation := deadLetterObservationFixture()
	mock.ExpectExec(`SET status = 'dead_letter'`).
		WithArgs(deadLetterChainRemoved, observation.chain, observation.txHash, observation.logIndex, observation.reason).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := w.reconcile(t.Context(), []depositObservation{observation}); err != nil {
		t.Fatalf("losing the compare-and-set must not fail the pass: %v", err)
	}
}

// Credited rows are already final. They keep rotating so the risk window keeps
// being sampled; only a pending row is dead-lettered.
func TestCreditedObservationKeepsRotatingWhenChainIsGone(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	w := &Worker{db: db}
	observation := deadLetterObservationFixture()
	observation.status = "credited"
	mock.ExpectExec(`SET status = \$1, reason = \$2, last_attempt_at = NOW\(\)`).
		WithArgs("credited", "", observation.chain, observation.txHash, observation.logIndex, "credited", "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := w.reconcile(t.Context(), []depositObservation{observation}); err != nil {
		t.Fatalf("credited rotation failed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A chain that is still configured but has no usable RPC pool (endpoint not
// configured yet, or a failed reload) is recoverable. The row keeps rotating so
// a real deposit is not written off by a fixable operator problem.
func TestMissingPoolKeepsAPendingObservationRetryable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	observation := deadLetterObservationFixture()
	w := &Worker{db: db, chains: []ChainConfig{{
		Chain: "bsc", Tokens: []string{splitToken},
		Decimals: map[string]int{splitToken: 6},
	}}}
	mock.ExpectExec(`SET status = \$1, reason = \$2, last_attempt_at = NOW\(\)`).
		WithArgs("pending", "", observation.chain, observation.txHash, observation.logIndex, "pending", "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := w.reconcile(t.Context(), []depositObservation{observation}); err != nil {
		t.Fatalf("missing pool must not dead-letter the deposit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
