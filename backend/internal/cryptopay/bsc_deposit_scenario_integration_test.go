//go:build integration

package cryptopay

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/stretchr/testify/require"

	"github.com/TokenFlux/TokenRouter/ent"
	_ "github.com/TokenFlux/TokenRouter/ent/runtime"
	"github.com/TokenFlux/TokenRouter/internal/billing"
	billingpostgres "github.com/TokenFlux/TokenRouter/internal/billing/postgres"
	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/TokenFlux/TokenRouter/internal/cryptopay/addresses"
	"github.com/TokenFlux/TokenRouter/internal/cryptopay/rpc"
	"github.com/TokenFlux/TokenRouter/internal/cryptopay/scanner"
	"github.com/TokenFlux/TokenRouter/internal/identity"
	identitypostgres "github.com/TokenFlux/TokenRouter/internal/identity/postgres"
	"github.com/TokenFlux/TokenRouter/internal/payment"
	paymenttestkit "github.com/TokenFlux/TokenRouter/internal/payment/testkit"
	"github.com/TokenFlux/TokenRouter/internal/settings"
	settingspostgres "github.com/TokenFlux/TokenRouter/internal/settings/postgres"
	"github.com/TokenFlux/TokenRouter/internal/testutil/postgrescontainer"
)

// bsc_deposit_scenario_integration_test.go exercises ledger/ledger.go,
// scanner/worker.go, and creditor.go with the host payment and billing
// services against migrated PostgreSQL. The loopback node keeps the deposit
// below finality and can replace its canonical block, so the scenario
// observes both the original grant and its compensating entry.

// These fixture values identify the exact BSC event and amount that the scanner
// must connect to the host buyer's wallet history.
const (
	bscScenarioBlock     = 1000
	bscScenarioIndex     = 7
	bscScenarioToken     = "0x55d398326f99059ff775485246999027b3197955"
	bscScenarioTopic     = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
	bscScenarioRawAmount = uint64(2_500_000_000_000_000_000)
	bscScenarioPaid      = 2.5
	bscScenarioCredit    = 6.25
	bscScenarioReorg     = "reorg_noncanonical_block"
	bscScenarioTxHash    = "0x4444444444444444444444444444444444444444444444444444444444444444"
	bscScenarioBlockHash = "0x5555555555555555555555555555555555555555555555555555555555555555"
)

// bscScenarioNode serves one transaction and tracks which evidence the scanner
// requested. The test publishes the recipient before the first scan.
type bscScenarioNode struct {
	recipient       atomic.Pointer[string] // assigned deposit recipient
	reorganized     atomic.Bool            // canonical block has changed
	reportedLogs    atomic.Int64           // inbound log responses
	replacementSeen atomic.Int64           // replacement block responses
}

// bscScenarioBalanceReader projects the host identity repository into billing's
// narrower buyer lookup contract.
type bscScenarioBalanceReader struct {
	users *identitypostgres.UserStore // PostgreSQL account repository
}

// bscScenarioState gathers the persisted financial facts checked after each pass.
type bscScenarioState struct {
	balance       float64 // current buyer balance
	deposits      int     // deposit rows for recipient
	orders        int     // payment orders for buyer
	positiveRows  int     // positive balance entries
	positiveUses  int     // positive usage receipts
	positiveValue float64 // sum of positive entries
	negativeRows  int     // negative balance entries
	negativeUses  int     // negative usage receipts
	negativeValue float64 // sum of negative entries
	revokeAudits  int     // reorg audits for buyer
}

// TestBSCDepositScanReplayAndReorg exercises a provisional BSC USDT deposit
// through the production scanner and host fulfillment. Replays on the same
// worker and its replacement preserve one grant, and the canonical block
// replacement produces one compensating entry and audit on repeated scans
// and concurrent delivery to the payment creditor.
func TestBSCDepositScanReplayAndReorg(t *testing.T) {
	t.Setenv("CRYPTO_ADMIN_TEST_MODE", "false")
	t.Setenv("CRYPTO_INSTANCE_ENABLED", "true")
	t.Setenv("SUB2API_DISABLE_BACKGROUND_JOBS", "true")

	ctx := t.Context()
	db := postgrescontainer.NewOrExternal(t, os.Getenv("TOKENROUTER_CRYPTO_E2E_DSN"), "crypto_e2e")
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	users := identitypostgres.NewUserStore(client, db)
	buyer := &identity.User{
		Email: "bsc-e2e-buyer@example.invalid", Username: "bsc-e2e-buyer",
		Status: identity.StatusActive, Role: identity.RoleUser,
	}
	require.NoError(t, buyer.SetPassword("local-bsc-fixture-only"))
	require.NoError(t, users.Create(ctx, buyer))
	var database string
	var migrations int
	require.NoError(t, db.QueryRowContext(ctx, "SELECT current_database(), count(*) FROM schema_migrations").Scan(&database, &migrations))
	require.Positive(t, migrations)
	t.Logf("fixture db=%s migrations=%d user_id=%d username=%s email=%s", database, migrations, buyer.ID, buyer.Username, buyer.Email)

	node := &bscScenarioNode{}
	server := httptest.NewServer(node)
	t.Cleanup(server.Close)
	service, creditor := newBSCScenarioService(t, db, client, users, server)
	var invalidations atomic.Int64
	creditor.SetBalanceInvalidator(func(_ context.Context, userID int64) error {
		if userID != buyer.ID {
			t.Errorf("balance invalidation user = %d, want %d", userID, buyer.ID)
		}
		invalidations.Add(1)
		return nil
	})
	recipient, err := service.ResolveDepositAddress(ctx, buyer.ID)
	require.NoError(t, err)
	binding, err := service.addresses.LookupAny(ctx, addresses.FamilyEVM, recipient)
	require.NoError(t, err)
	require.NotNil(t, binding)
	require.Equal(t, buyer.ID, binding.UserID)
	require.Equal(t, "active", binding.Status)
	recipient = strings.ToLower(recipient)
	node.recipient.Store(&recipient)
	t.Logf("address assignment user_id=%d assignment_id=%d address=%s", buyer.ID, binding.ID, recipient)

	initial := readBSCScenarioState(t, db, buyer.ID, recipient, "initial")
	require.Equal(t, bscScenarioState{}, initial)

	firstWorker := service.worker
	require.NotNil(t, firstWorker)
	require.NoError(t, service.RunScan(ctx, bscScenarioBlock, ChainBSC))
	require.Positive(t, node.reportedLogs.Load())
	credited := readBSCScenarioState(t, db, buyer.ID, recipient, "provisional credit")
	require.Equal(t, bscScenarioState{
		balance: initial.balance + bscScenarioCredit, deposits: 1, orders: 1,
		positiveRows: 1, positiveUses: 1, positiveValue: bscScenarioCredit,
	}, credited)
	depositID, orderID := checkBSCScenarioDeposit(t, db, buyer.ID, "credited", "")

	// Rewind into the original event so the duplicate assertion observes a
	// real second JSON-RPC log delivery, even after the durable cursor advanced.
	for _, stage := range []struct {
		name   string // label in the test log
		reload bool   // rebuild worker before replay
	}{
		{name: "same worker replay"},
		{name: "replacement worker replay", reload: true},
	} {
		if stage.reload {
			service.Reload(ctx)
			require.NotNil(t, service.worker)
			require.NotSame(t, firstWorker, service.worker)
		}
		before := node.reportedLogs.Load()
		require.NoError(t, service.RunScan(ctx, bscScenarioBlock, ChainBSC))
		require.Greater(t, node.reportedLogs.Load(), before)
		require.Equal(t, credited, readBSCScenarioState(t, db, buyer.ID, recipient, stage.name))
		gotDeposit, gotOrder := checkBSCScenarioDeposit(t, db, buyer.ID, "credited", "")
		require.Equal(t, depositID, gotDeposit)
		require.Equal(t, orderID, gotOrder)
		t.Logf("%s inbound_log_responses=%d deposit_id=%d order_id=%d", stage.name, node.reportedLogs.Load(), gotDeposit, gotOrder)
	}

	// Keep the old receipt visible while replacing the canonical block at
	// its height. The scanner must establish the discrepancy before revoking.
	node.reorganized.Store(true)
	require.NoError(t, service.worker.RunOnce(ctx))
	require.Positive(t, node.replacementSeen.Load())
	require.EqualValues(t, 1, invalidations.Load())
	reversed := credited
	reversed.balance = initial.balance
	reversed.negativeRows = 1
	reversed.negativeUses = 1
	reversed.negativeValue = -bscScenarioCredit
	reversed.revokeAudits = 1
	require.Equal(t, reversed, readBSCScenarioState(t, db, buyer.ID, recipient, "reorg"))
	gotDeposit, gotOrder := checkBSCScenarioDeposit(t, db, buyer.ID, "reverted", bscScenarioReorg)
	require.Equal(t, depositID, gotDeposit)
	require.Equal(t, orderID, gotOrder)
	t.Logf("reorg canonical_replacement_responses=%d balance_invalidations=%d deposit_id=%d order_id=%d",
		node.replacementSeen.Load(), invalidations.Load(), gotDeposit, gotOrder)

	// Normal scans skip terminal rows. Deliver the original observation and
	// reorg reason to the real creditor as well to exercise its database guard.
	replayBSCScenarioReorg(t, creditor)
	require.EqualValues(t, 1, invalidations.Load())
	t.Logf("concurrent creditor replays balance_invalidations=%d", invalidations.Load())
	require.Equal(t, reversed, readBSCScenarioState(t, db, buyer.ID, recipient, "concurrent creditor replays"))

	for pass := 1; pass <= 2; pass++ {
		require.NoError(t, service.RunScan(ctx, bscScenarioBlock, ChainBSC))
		stage := fmt.Sprintf("reorg replay %d", pass)
		require.Equal(t, reversed, readBSCScenarioState(t, db, buyer.ID, recipient, stage))
		gotDeposit, gotOrder = checkBSCScenarioDeposit(t, db, buyer.ID, "reverted", bscScenarioReorg)
		require.Equal(t, depositID, gotDeposit)
		require.Equal(t, orderID, gotOrder)
	}
	require.EqualValues(t, 1, invalidations.Load())
}

// replayBSCScenarioReorg submits the scanner's already handled evidence to two
// real creditor transactions. Both attempts must report the existing terminal
// resolution without recording a new reversal or requesting manual review.
func replayBSCScenarioReorg(t *testing.T, creditor *DepositCreditor) {
	t.Helper()
	ctx := t.Context()
	target := scanner.DepositReorg{
		Chain: ChainBSC, TxHash: bscScenarioTxHash, LogIndex: bscScenarioIndex,
		BlockHash: bscScenarioBlockHash, BlockNumber: bscScenarioBlock, Reason: bscScenarioReorg,
	}
	var attempts [2]struct {
		outcome scanner.DepositReorgOutcome
		err     error
	}
	var workers sync.WaitGroup
	start := make(chan struct{})
	for i := range attempts {
		workers.Go(func() {
			<-start
			attempts[i].outcome, attempts[i].err = creditor.ReconcileDepositReorg(ctx, target)
		})
	}
	// Release both contenders through one gate and wait before checking results,
	// so the test owns each database transaction until it finishes.
	close(start)
	workers.Wait()
	for i, attempt := range attempts {
		t.Logf("creditor replay %d handled=%t revoked=%t manual_review=%t", i+1,
			attempt.outcome.Handled, attempt.outcome.Revoked, attempt.outcome.ManualReview)
		require.NoError(t, attempt.err)
		require.Equal(t, scanner.DepositReorgOutcome{Handled: true}, attempt.outcome)
	}
}

// GetByID reads the buyer from PostgreSQL for the host redemption service.
func (r bscScenarioBalanceReader) GetByID(ctx context.Context, id int64) (*billing.UserSummary, error) {
	user, err := r.users.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return &billing.UserSummary{ID: user.ID, Balance: user.Balance}, nil
}

// ServeHTTP returns BSC evidence for the production RPC client. A stale receipt
// retains the original hash after replacement, while logs and the canonical
// block lookup reflect the new chain.
func (n *bscScenarioNode) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     json.RawMessage   `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON-RPC request", http.StatusBadRequest)
		return
	}
	block := fmt.Sprintf("0x%x", bscScenarioBlock)
	recipient := n.recipient.Load()
	if recipient == nil {
		http.Error(w, "deposit recipient has not been assigned", http.StatusServiceUnavailable)
		return
	}
	recipientTopic := "0x" + strings.Repeat("0", 24) + strings.TrimPrefix(*recipient, "0x")
	event := map[string]any{
		"address": bscScenarioToken, "topics": []string{bscScenarioTopic, "0x" + strings.Repeat("0", 24) + strings.Repeat("22", 20), recipientTopic},
		"data": fmt.Sprintf("0x%064x", bscScenarioRawAmount), "blockNumber": block,
		"transactionHash": bscScenarioTxHash, "logIndex": fmt.Sprintf("0x%x", bscScenarioIndex), "blockHash": bscScenarioBlockHash,
	}
	var result any
	switch req.Method {
	case "eth_chainId":
		result = "0x38"
	case "eth_blockNumber":
		// Depth admits scanning while finalized still trails.
		result = fmt.Sprintf("0x%x", bscScenarioBlock+5)
	case "eth_getLogs":
		var filter struct {
			FromBlock string     `json:"fromBlock"`
			ToBlock   string     `json:"toBlock"`
			Address   []string   `json:"address"`
			Topics    [][]string `json:"topics"`
		}
		if len(req.Params) != 1 || json.Unmarshal(req.Params[0], &filter) != nil {
			http.Error(w, "invalid eth_getLogs filter", http.StatusBadRequest)
			return
		}
		first, firstErr := strconv.ParseUint(strings.TrimPrefix(filter.FromBlock, "0x"), 16, 64)
		last, lastErr := strconv.ParseUint(strings.TrimPrefix(filter.ToBlock, "0x"), 16, 64)
		if firstErr != nil || lastErr != nil || first > last {
			http.Error(w, "invalid eth_getLogs range", http.StatusBadRequest)
			return
		}
		logs := []any{}
		if !n.reorganized.Load() && first <= bscScenarioBlock && bscScenarioBlock <= last &&
			slices.Contains(filter.Address, bscScenarioToken) && len(filter.Topics) == 3 &&
			slices.Contains(filter.Topics[0], bscScenarioTopic) && slices.Contains(filter.Topics[2], recipientTopic) {
			logs = append(logs, event)
			n.reportedLogs.Add(1)
		}
		result = logs
	case "eth_getTransactionReceipt":
		if len(req.Params) != 1 || string(req.Params[0]) != strconv.Quote(bscScenarioTxHash) {
			http.Error(w, "unexpected transaction hash", http.StatusBadRequest)
			return
		}
		result = map[string]any{"status": "0x1", "blockNumber": block, "blockHash": bscScenarioBlockHash, "transactionHash": bscScenarioTxHash, "logs": []any{event}}
	case "eth_getBlockByNumber":
		if len(req.Params) == 2 {
			switch string(req.Params[0]) {
			case `"finalized"`:
				result = map[string]string{"number": fmt.Sprintf("0x%x", bscScenarioBlock-1)}
			case strconv.Quote(block):
				hash := bscScenarioBlockHash
				if n.reorganized.Load() {
					hash = "0x" + strings.Repeat("66", 32)
					n.replacementSeen.Add(1)
				}
				result = map[string]string{"number": block, "hash": hash}
			}
		}
	}
	if result == nil {
		http.Error(w, "unexpected RPC "+req.Method, http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

// checkBSCScenarioDeposit verifies the persisted link between the event and its
// original completed order. It checks the revocation timestamp and reason
// together, and returns both IDs for later identity and replay assertions.
func checkBSCScenarioDeposit(t *testing.T, db *sql.DB, userID int64, status, revokeReason string) (int64, int64) {
	t.Helper()
	var depositID, orderID, gotUserID int64
	var gotStatus, reason, orderStatus, tx, blockHash string
	var blockNumber uint64
	var provisional, credited, revoked bool
	var amount, payAmount float64
	require.NoError(t, db.QueryRowContext(t.Context(), `
		SELECT d.id, o.id, o.user_id, d.status, COALESCE(d.revoke_reason, ''),
		       d.provisional, d.credited_at IS NOT NULL, d.revoked_at IS NOT NULL,
		       o.status, o.amount, o.pay_amount, o.payment_trade_no, d.block_number, d.block_hash
		  FROM deposit_transactions d JOIN payment_orders o ON o.id = d.order_id
		 WHERE d.chain = $1 AND d.tx_hash = $2 AND d.log_index = $3
	`, ChainBSC, bscScenarioTxHash, bscScenarioIndex).Scan(&depositID, &orderID, &gotUserID, &gotStatus, &reason,
		&provisional, &credited, &revoked, &orderStatus, &amount, &payAmount, &tx, &blockNumber, &blockHash))
	require.Equal(t, userID, gotUserID)
	require.Equal(t, status, gotStatus)
	require.Equal(t, revokeReason, reason)
	require.True(t, provisional)
	require.True(t, credited)
	require.Equal(t, revokeReason != "", revoked)
	require.Equal(t, payment.OrderStatusCompleted, orderStatus)
	require.Equal(t, bscScenarioCredit, amount)
	require.Equal(t, bscScenarioPaid, payAmount)
	require.Equal(t, bscScenarioTxHash, tx)
	require.EqualValues(t, bscScenarioBlock, blockNumber)
	require.Equal(t, bscScenarioBlockHash, blockHash)
	t.Logf("deposit_id=%d order_id=%d log_index=%d status=%s provisional=%t revoked=%t reason=%q order_status=%s paid=%.3f credited=%.3f",
		depositID, orderID, bscScenarioIndex, gotStatus, provisional, revoked, reason, orderStatus, payAmount, amount)
	return depositID, orderID
}

// newBSCScenarioService assembles the host's SQL-backed deposit and fulfillment
// components. The loopback transport occupies the BSC pool before reload so
// the normal worker configuration keeps using that transport across rebuilds.
func newBSCScenarioService(t *testing.T, db *sql.DB, client *ent.Client, users *identitypostgres.UserStore, server *httptest.Server) (*Service, *DepositCreditor) {
	t.Helper()
	redeem := billing.NewRedeemService(billingpostgres.NewRedeemCodeRepository(client), bscScenarioBalanceReader{users: users}, nil, nil, nil,
		billingpostgres.NewRedeemMutations(client, billingpostgres.RedeemWriters{Balances: billingpostgres.NewBalanceStore(client)}),
		nil, nil, billing.RedeemRuntime{})
	creditor := NewDepositCreditor(client, paymenttestkit.Fulfillment(client, redeem, nil, nil), db)
	store := NewServiceSettingStore(settings.New(settingspostgres.NewSettingRepository(client)), db)
	addressService := NewAddressService(db)
	addressService.SetFamilies([]string{addresses.FamilyEVM})
	appConfig := &config.Config{Security: config.SecurityConfig{
		SecretEncryptionKey: strings.Repeat("12", 32), SecretEncryptionKeyConfigured: true,
	}}
	service := NewManagedService(db, store, creditor, addressService, appConfig)
	t.Cleanup(service.Close)
	// These public BIP39 vectors bootstrap an isolated wallet. Its derived
	// addresses enter the same persisted ownership check used during scanning.
	require.NoError(t, service.InitializeWallet(t.Context(),
		"abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about",
		"legal winner thank year wave sausage worth useful legal winner thank yellow"))
	service.pools[ChainBSC] = rpc.NewPoolWithHTTPClient(server.URL, server.Client())
	cfg := DefaultCryptoConfig()
	cfg.Enabled = true
	cfg.RPC[ChainBSC] = server.URL
	cfg.NativeScan = NativeScanTxList
	cfg.CreditMultiplier = 2.5
	require.NoError(t, SaveConfig(t.Context(), store, cfg))
	service.Reload(t.Context())
	require.True(t, service.Config().Enabled)
	return service, creditor
}

// readBSCScenarioState reports buyer-wide totals, so duplicate rows cannot hide
// behind an otherwise valid event link. The usage joins count receipts
// independently of the persisted credential rows.
func readBSCScenarioState(t *testing.T, db *sql.DB, userID int64, recipient, stage string) bscScenarioState {
	t.Helper()
	var state bscScenarioState
	require.NoError(t, db.QueryRowContext(t.Context(), `
		SELECT balance,
		       (SELECT count(*) FROM deposit_transactions WHERE chain = 'bsc' AND to_address = $2),
		       (SELECT count(*) FROM payment_orders WHERE user_id = u.id),
		       (SELECT count(*) FROM redeem_codes WHERE used_by = u.id AND value > 0),
		       (SELECT count(*) FROM redeem_code_usages x JOIN redeem_codes r ON r.id = x.redeem_code_id
		         WHERE x.user_id = u.id AND r.value > 0),
		       (SELECT COALESCE(sum(value), 0) FROM redeem_codes WHERE used_by = u.id AND value > 0),
		       (SELECT count(*) FROM redeem_codes WHERE used_by = u.id AND value < 0),
		       (SELECT count(*) FROM redeem_code_usages x JOIN redeem_codes r ON r.id = x.redeem_code_id
		         WHERE x.user_id = u.id AND r.type = 'crypto_revoke' AND r.value < 0),
		       (SELECT COALESCE(sum(value), 0) FROM redeem_codes WHERE used_by = u.id AND value < 0),
		       (SELECT count(*) FROM payment_audit_logs a JOIN payment_orders o ON a.order_id = o.id::text
		         WHERE o.user_id = u.id AND a.action = 'DEPOSIT_PROVISIONAL_REVOKED')
		  FROM users u WHERE u.id = $1
	`, userID, recipient).Scan(&state.balance, &state.deposits, &state.orders,
		&state.positiveRows, &state.positiveUses, &state.positiveValue,
		&state.negativeRows, &state.negativeUses, &state.negativeValue, &state.revokeAudits))
	t.Logf("%s balance=%.3f deposits=%d orders=%d positive_entries=%d positive_receipts=%d positive_amount=%.3f negative_entries=%d negative_receipts=%d negative_amount=%.3f revoke_audits=%d",
		stage, state.balance, state.deposits, state.orders, state.positiveRows, state.positiveUses, state.positiveValue,
		state.negativeRows, state.negativeUses, state.negativeValue, state.revokeAudits)
	return state
}
