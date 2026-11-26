package ledger

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	sqlite "modernc.org/sqlite"

	"github.com/TokenFlux/TokenRouter/internal/cryptopay/nativeprice"
	"github.com/TokenFlux/TokenRouter/internal/cryptopay/scanner"
)

// markCreditedPattern checks the settlement write's identity, durable grant
// evidence, and block guard across provisional credit and recovery tests.
const markCreditedPattern = "UPDATE deposit_transactions " +
	"SET status = 'credited', provisional = provisional AND \\$4 " +
	"WHERE chain = \\$1 AND tx_hash = \\$2 AND log_index = \\$3 AND status = 'pending' " +
	"AND order_id IS NOT NULL AND credited_at IS NOT NULL " +
	"AND revoked_at IS NULL AND LOWER\\(block_hash\\) = LOWER\\(\\$5\\)"

// failOnceCreditor fails the first fulfillment, then succeeds. It lets a test
// observe the window in which the price was persisted but the balance was not
// yet credited.
type failOnceCreditor struct {
	db       *sql.DB
	calls    int
	requests []CreditRequest
}

type fixedResolver struct {
	userID int64
	ours   bool
}

type countingCreditor struct {
	db       *sql.DB
	calls    int
	requests []CreditRequest
}

// The ledger writes Postgres SQL. sqlite has no NOW(), so the statements under
// test would fail on a function that production provides; registering it keeps
// the production statement text (and therefore the test's fidelity) unchanged.
func init() {
	if err := sqlite.RegisterScalarFunction("NOW", 0,
		func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
			return time.Now().UTC().Format(time.RFC3339Nano), nil
		}); err != nil {
		panic(err)
	}
}

func TestAmountStringRendering(t *testing.T) {
	tests := []struct {
		raw      int64
		decimals int
		want     string
	}{
		{12345678, 6, "12.345678"},
		{1000000, 6, "1"},
		{1, 6, "0.000001"},
		{150, 0, "150"},
		{0, 6, "0"},
	}
	for _, tt := range tests {
		if got := amountString(big.NewInt(tt.raw), tt.decimals); got != tt.want {
			t.Errorf("amountString(%d, %d) = %q, want %q", tt.raw, tt.decimals, got, tt.want)
		}
	}
}

func TestToDecimal(t *testing.T) {
	if got := toDecimal(big.NewInt(12_345_678), 6); got != 12.345678 {
		t.Fatalf("toDecimal = %v, want 12.345678", got)
	}
}

func TestCreditMultiplierRoundsUsingExactDecimal(t *testing.T) {
	amount := decimal.NewFromBigInt(big.NewInt(15_000), -6)
	credited := amount.Mul(decimal.NewFromFloat(6.7)).Round(3)
	if got := credited.StringFixed(3); got != "0.101" {
		t.Fatalf("credit amount = %s, want 0.101", got)
	}
}

// A rejected transfer must carry a machine reason so the UI can explain it.
// The 2026-10-14 0.01 USDT probe landed as a bare "ignored" with a raw-unit
// amount; both halves of that bug are pinned here.
func TestAmountStringUsesTokenDecimals(t *testing.T) {
	// 0.01 USDT on an 18-decimal chain is 10^16 raw. Stored at 18 decimals it
	// renders as 0.01, not 1e16.
	if got := amountString(big.NewInt(10_000_000_000_000_000), 18); got != "0.01" {
		t.Fatalf("18-decimal amount = %q, want 0.01", got)
	}
	// The old bug: the same raw value stored at decimals=0 renders as 1e16.
	if got := amountString(big.NewInt(10_000_000_000_000_000), 0); got != "10000000000000000" {
		t.Fatalf("decimals=0 amount = %q", got)
	}
}

func TestReasonTextCoversThresholds(t *testing.T) {
	if got := ReasonText(ReasonBelowMin, 0.001, 10000); got == "" || got == ReasonBelowMin {
		t.Fatalf("below-min reason not rendered: %q", got)
	}
	if got := ReasonText(ReasonAboveMax, 0.001, 10000); got == "" || got == ReasonAboveMax {
		t.Fatalf("above-max reason not rendered: %q", got)
	}
}

// nativeDepositDB is the smallest Postgres-compatible schema the ledger's
// statements touch, in sqlite. It deliberately keeps the production unique
// index: idempotency for a native deposit is the same constraint that protects
// a token deposit, not a second bookkeeping mechanism.
//
// amount is TEXT rather than NUMERIC because sqlite's NUMERIC affinity converts
// to a float and would silently lose the 18th decimal. Production uses
// NUMERIC(38,18), whose exactness is covered by the Postgres integration test.
func nativeDepositDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for _, statement := range []string{
		`CREATE TABLE deposit_transactions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			chain TEXT, tx_hash TEXT, log_index INTEGER,
			from_address TEXT, to_address TEXT, token TEXT, amount TEXT,
			block_number INTEGER, block_hash TEXT, confirmations INTEGER DEFAULT 0,
			status TEXT DEFAULT 'pending', order_id INTEGER, reason TEXT,
			created_at TEXT DEFAULT CURRENT_TIMESTAMP, last_attempt_at TEXT,
			native_price_usd TEXT, native_price_at TIMESTAMP, native_price_source TEXT,
			provisional INTEGER NOT NULL DEFAULT 0, credited_at TIMESTAMP, revoked_at TIMESTAMP, revoke_reason TEXT,
			UNIQUE(chain, tx_hash, log_index))`,
		`CREATE TABLE payment_orders (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			out_trade_no TEXT, user_id INTEGER, amount NUMERIC)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func nativeLedgerTransfer(amount *big.Int) scanner.Transfer {
	return scanner.Transfer{
		Chain: "ethereum", TxHash: "0xnativetx", LogIndex: scanner.NativeLogIndex,
		From: "0xsender", To: "0xdeposit", Token: scanner.NativeToken, Native: true,
		Amount: amount, BlockNumber: 100, BlockHash: "0xblock",
	}
}

// One mined transaction must credit exactly once, however many scan passes
// observe it. The 18-decimal amount must survive the NUMERIC round trip too:
// a credit built from a mis-scaled amount would be off by 1e12.
func TestNativeDepositIsCreditedOncePerTransaction(t *testing.T) {
	db := nativeDepositDB(t)
	creditor := &countingCreditor{db: db}
	l := New(db, creditor, fixedResolver{userID: 42, ours: true}, 0.001, 10000, 6.7)
	l.SetNativePricer(fixedPricer(10))

	raw, ok := new(big.Int).SetString("1234567890123456789", 10) // 1.234567890123456789 ETH
	if !ok {
		t.Fatal("bad fixture")
	}
	transfer := nativeLedgerTransfer(raw)

	credited, err := l.Process(t.Context(), transfer, 18)
	if err != nil {
		t.Fatalf("first native credit failed: %v", err)
	}
	if !credited || creditor.calls != 1 {
		t.Fatalf("first observation credited=%v calls=%d, want true/1", credited, creditor.calls)
	}
	// A later scan pass sees the same transaction again.
	credited, err = l.Process(t.Context(), transfer, 18)
	if err != nil {
		t.Fatalf("repeat native observation failed: %v", err)
	}
	if credited || creditor.calls != 1 {
		t.Fatalf("repeat observation credited twice: credited=%v calls=%d", credited, creditor.calls)
	}

	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM deposit_transactions`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("deposit rows = %d, want exactly 1", rows)
	}
	var token, amount, status string
	var logIndex int
	if err := db.QueryRow(`SELECT token, amount, status, log_index FROM deposit_transactions`).Scan(&token, &amount, &status, &logIndex); err != nil {
		t.Fatal(err)
	}
	if token != scanner.NativeToken || logIndex != scanner.NativeLogIndex || status != "credited" {
		t.Fatalf("stored native row = token:%q log_index:%d status:%q", token, logIndex, status)
	}
	if amount != "1.234567890123456789" {
		t.Fatalf("stored amount = %q, want the exact 18-decimal value", amount)
	}
	if got := creditor.requests[0].CreditUSDT; got <= 0 {
		t.Fatalf("credited balance = %v", got)
	}
	if creditor.requests[0].Decimals != 18 {
		t.Fatalf("credit request decimals = %d, want 18", creditor.requests[0].Decimals)
	}
}

// A pending observation written before finality must not duplicate the row when
// the same transfer is recorded again, and the unique index must keep a native
// deposit distinct from a token deposit in the same transaction.
func TestNativePendingObservationIsIdempotent(t *testing.T) {
	db := nativeDepositDB(t)
	creditor := &countingCreditor{db: db}
	l := New(db, creditor, fixedResolver{userID: 42, ours: true}, 0.001, 10000, 6.7)
	l.SetNativePricer(fixedPricer(10))

	transfer := nativeLedgerTransfer(big.NewInt(1_000_000_000_000_000_000))
	for range 3 {
		if err := l.ProcessPending(t.Context(), transfer, 18); err != nil {
			t.Fatalf("record pending native observation: %v", err)
		}
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM deposit_transactions`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("pending rows = %d, want 1", rows)
	}

	// A token deposit mined in the same transaction is a different row: the
	// native sentinel owns the synthetic log index, the log owns its own.
	tokenTransfer := transfer
	tokenTransfer.Token = "0xdac17f958d2ee523a2206206994597c13d831ec7"
	tokenTransfer.Native = false
	tokenTransfer.LogIndex = 4
	tokenTransfer.Amount = big.NewInt(10_000_000)
	if err := l.ProcessPending(t.Context(), tokenTransfer, 6); err != nil {
		t.Fatalf("record token observation beside the native one: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM deposit_transactions`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("rows = %d, want the native and token deposits to coexist", rows)
	}
}

// A native deposit below the credit minimum must be recorded with the
// token-independent reason the UI renders, never silently dropped.
func TestNativeDepositBelowMinimumIsIgnoredNotDropped(t *testing.T) {
	db := nativeDepositDB(t)
	creditor := &countingCreditor{db: db}
	l := New(db, creditor, fixedResolver{userID: 42, ours: true}, 0.001, 10000, 6.7)
	l.SetNativePricer(fixedPricer(10))

	// 1 wei is far below the 0.001 minimum.
	credited, err := l.Process(t.Context(), nativeLedgerTransfer(big.NewInt(1)), 18)
	if err != nil {
		t.Fatalf("below-minimum native deposit failed: %v", err)
	}
	if credited || creditor.calls != 0 {
		t.Fatal("a dust native deposit was credited")
	}
	var status, reason string
	if err := db.QueryRow(`SELECT status, reason FROM deposit_transactions`).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "ignored" || reason != ReasonBelowMin {
		t.Fatalf("dust native deposit = status:%q reason:%q", status, reason)
	}
}

func (c *failOnceCreditor) CreditDeposit(ctx context.Context, req CreditRequest) error {
	c.calls++
	c.requests = append(c.requests, req)
	if c.calls == 1 {
		return errors.New("fulfillment temporarily unavailable")
	}
	return recordGrantFixture(ctx, c.db, req)
}

func TestNativeDepositCreditsAtMarketPrice(t *testing.T) {
	db := nativeDepositDB(t)
	creditor := &countingCreditor{db: db}
	l := New(db, creditor, fixedResolver{userID: 42, ours: true}, 0.001, 10000, 6.7)
	pricer := &stubPricer{price: decimal.NewFromInt(2000)}
	l.SetNativePricer(pricer)

	// 1 ETH at $2000 with the existing 6.7 multiplier => 13400 balance units.
	credited, err := l.Process(t.Context(), nativeLedgerTransfer(big.NewInt(1_000_000_000_000_000_000)), 18)
	require.NoError(t, err)
	require.True(t, credited)
	require.Equal(t, 1, creditor.calls)
	require.InDelta(t, 13400, creditor.requests[0].CreditUSDT, 1e-9)
	require.Equal(t, "2000", creditor.requests[0].NativePriceUSD)
	require.Equal(t, "stub", creditor.requests[0].NativePriceSource)
	require.False(t, creditor.requests[0].NativePriceAt.IsZero())

	var price, source string
	var pricedAt time.Time
	require.NoError(t, db.QueryRow(`SELECT CAST(native_price_usd AS TEXT), native_price_at, native_price_source
		FROM deposit_transactions`).Scan(&price, &pricedAt, &source))
	require.Equal(t, "2000", price)
	require.Equal(t, "stub", source)
	require.False(t, pricedAt.IsZero(), "the deposit row must answer when the price was observed")
}

// A feed outage must leave the deposit pending and never call the creditor.
// The next pass, once the feed answers, must credit it.
func TestNativePriceFailureLeavesDepositRetryable(t *testing.T) {
	db := nativeDepositDB(t)
	creditor := &countingCreditor{db: db}
	l := New(db, creditor, fixedResolver{userID: 42, ours: true}, 0.001, 10000, 6.7)
	pricer := &stubPricer{err: errTestPrice}
	l.SetNativePricer(pricer)
	transfer := nativeLedgerTransfer(big.NewInt(1_000_000_000_000_000_000))

	credited, err := l.Process(t.Context(), transfer, 18)
	require.ErrorIs(t, err, errTestPrice)
	require.False(t, credited)
	require.Zero(t, creditor.calls, "no balance may be credited without a price")

	var status string
	var price sql.NullString
	require.NoError(t, db.QueryRow(`SELECT status, native_price_usd FROM deposit_transactions`).Scan(&status, &price))
	require.Equal(t, "pending", status)
	require.False(t, price.Valid, "a failed fetch must not record a price")

	// The next pass gets a quote and settles the same deposit.
	pricer.err = nil
	pricer.price = decimal.NewFromInt(2000)
	credited, err = l.Process(t.Context(), transfer, 18)
	require.NoError(t, err)
	require.True(t, credited)
	require.Equal(t, 1, creditor.calls)
	require.InDelta(t, 13400, creditor.requests[0].CreditUSDT, 1e-9)
}

// A quote older than the freshness ceiling is as unusable as no quote.
func TestStaleNativePriceIsNotCredited(t *testing.T) {
	db := nativeDepositDB(t)
	creditor := &countingCreditor{db: db}
	l := New(db, creditor, fixedResolver{userID: 42, ours: true}, 0.001, 10000, 6.7)
	l.SetNativePricer(&stubPricer{price: decimal.NewFromInt(2000), at: time.Now().Add(-nativeprice.MaxQuoteAge - time.Minute)})

	credited, err := l.Process(t.Context(), nativeLedgerTransfer(big.NewInt(1_000_000_000_000_000_000)), 18)
	require.ErrorIs(t, err, nativeprice.ErrStale)
	require.False(t, credited)
	require.Zero(t, creditor.calls)

	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM deposit_transactions`).Scan(&status))
	require.Equal(t, "pending", status)
}

// Once a settlement price is recorded, a replay must use it — even if the
// market (and therefore the feed) has moved since.
func TestRepeatedNativeProcessingKeepsTheFirstPrice(t *testing.T) {
	db := nativeDepositDB(t)
	creditor := &failOnceCreditor{db: db}
	l := New(db, creditor, fixedResolver{userID: 42, ours: true}, 0.001, 10000, 6.7)
	pricer := &stubPricer{price: decimal.NewFromInt(2000)}
	l.SetNativePricer(pricer)
	transfer := nativeLedgerTransfer(big.NewInt(1_000_000_000_000_000_000))

	// First attempt persists $2000, then fulfillment fails; the row stays
	// pending with the price already recorded.
	_, err := l.Process(t.Context(), transfer, 18)
	require.Error(t, err)
	require.Equal(t, 1, creditor.calls)
	require.InDelta(t, 13400, creditor.requests[0].CreditUSDT, 1e-9)

	// The market doubles. The replay must not re-price the deposit.
	pricer.price = decimal.NewFromInt(3000)
	credited, err := l.Process(t.Context(), transfer, 18)
	require.NoError(t, err)
	require.True(t, credited)
	require.Equal(t, 2, creditor.calls)
	require.InDelta(t, 13400, creditor.requests[1].CreditUSDT, 1e-9,
		"the replay used a new price instead of the first settlement price")
	require.Equal(t, 1, pricer.calls, "a stored price must not be re-fetched")
	require.Equal(t, "2000", creditor.requests[1].NativePriceUSD)
}

// A price observed but never settled (no order) may expire. Retrying must
// replace it with a fresh quote instead of stranding the deposit forever; the
// idempotency guarantee only binds once an order freezes the amount.
func TestExpiredStoredPriceIsReplacedBeforeSettlement(t *testing.T) {
	db := nativeDepositDB(t)
	creditor := &countingCreditor{db: db}
	l := New(db, creditor, fixedResolver{userID: 42, ours: true}, 0.001, 10000, 6.7)
	transfer := nativeLedgerTransfer(big.NewInt(1_000_000_000_000_000_000))
	require.NoError(t, l.ProcessPending(t.Context(), transfer, 18))
	_, err := db.Exec(`UPDATE deposit_transactions
		SET native_price_usd = '1000', native_price_at = $1, native_price_source = 'old'`, time.Now().Add(-nativeprice.MaxQuoteAge-time.Minute))
	require.NoError(t, err)

	l.SetNativePricer(&stubPricer{price: decimal.NewFromInt(2000)})
	credited, err := l.Process(t.Context(), transfer, 18)
	require.NoError(t, err)
	require.True(t, credited)
	require.InDelta(t, 13400, creditor.requests[0].CreditUSDT, 1e-9)

	var price, source string
	require.NoError(t, db.QueryRow(`SELECT CAST(native_price_usd AS TEXT), native_price_source
		FROM deposit_transactions`).Scan(&price, &source))
	require.Equal(t, "2000", price)
	require.Equal(t, "stub", source)
}

// Balance precision is 3 decimals and native credit truncates: rounding a
// fraction up would mint balance the deposit did not pay for.
func TestNativeCreditTruncatesInsteadOfRoundingUp(t *testing.T) {
	db := nativeDepositDB(t)
	creditor := &countingCreditor{db: db}
	l := New(db, creditor, fixedResolver{userID: 42, ours: true}, 0.001, 10000, 6.7)
	l.SetNativePricer(&stubPricer{price: decimal.NewFromInt(1)})

	// 1.0001 ETH * $1 * 6.7 = 6.70067; truncation yields 6.700, not 6.701.
	credited, err := l.Process(t.Context(), nativeLedgerTransfer(big.NewInt(1_000_100_000_000_000_000)), 18)
	require.NoError(t, err)
	require.True(t, credited)
	require.InDelta(t, 6.7, creditor.requests[0].CreditUSDT, 1e-9)
	require.Less(t, creditor.requests[0].CreditUSDT, 6.701)
}

// Stablecoins already carry a USD amount; they must take the original path and
// never touch the market feed.
func TestStablecoinCreditIgnoresMarketPrice(t *testing.T) {
	db := nativeDepositDB(t)
	creditor := &countingCreditor{db: db}
	l := New(db, creditor, fixedResolver{userID: 42, ours: true}, 0.001, 10000, 6.7)
	pricer := &stubPricer{err: errTestPrice}
	l.SetNativePricer(pricer)

	transfer := scanner.Transfer{
		Chain: "bsc", TxHash: "0xtoken", LogIndex: 3,
		From: "0xsender", To: "0xdeposit",
		Token:  "0x55d398326f99059ff775485246999027b3197955",
		Amount: big.NewInt(10_000_000), BlockNumber: 100, BlockHash: "0xblock",
	}
	credited, err := l.Process(t.Context(), transfer, 6)
	require.NoError(t, err)
	require.True(t, credited)
	require.InDelta(t, 67, creditor.requests[0].CreditUSDT, 1e-9)
	require.Empty(t, creditor.requests[0].NativePriceUSD)
	require.Zero(t, pricer.calls, "the stablecoin path must not consult the price feed")
}

func (r fixedResolver) IsOwnAddress(context.Context, string, string) (bool, error) { return false, nil }

func (r fixedResolver) ResolveDeposit(context.Context, string, string) (int64, bool, error) {
	return r.userID, r.ours, nil
}

func (c *countingCreditor) CreditDeposit(ctx context.Context, req CreditRequest) error {
	c.calls++
	c.requests = append(c.requests, req)
	return recordGrantFixture(ctx, c.db, req)
}

func depositTransfer(raw int64) scanner.Transfer {
	return scanner.Transfer{
		Chain: "bsc", TxHash: "test-transaction", LogIndex: 9,
		From: "sender", To: "deposit", Token: "token", Amount: big.NewInt(raw), BlockNumber: 101, BlockHash: "test-block",
	}
}

func TestRejectedDepositPersistenceErrorsReachCaller(t *testing.T) {
	tests := []struct {
		name           string
		raw            int64
		userID         int64
		reason, amount string
	}{
		{"below minimum", 100_000, 42, ReasonBelowMin, "0.1"},
		{"above maximum", 11_000_000, 42, ReasonAboveMax, "11"},
		{"unassigned or disabled", 1_000_000, 0, ReasonUnassigned, "1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				mock.ExpectClose()
				if err := db.Close(); err != nil {
					t.Errorf("close persistence db: %v", err)
				}
			}()
			creditor := &countingCreditor{}
			l := New(db, creditor, fixedResolver{userID: tt.userID, ours: true}, 1, 10, 1)
			tr := depositTransfer(tt.raw)
			writeErr := errors.New("ignored evidence cannot be persisted")
			expectDepositState(mock, tr, stateRows())
			mock.ExpectExec(`INSERT INTO deposit_transactions`).WithArgs(tr.Chain, tr.TxHash, tr.LogIndex,
				tr.From, tr.To, tr.Token, tt.amount, tr.BlockNumber, tr.BlockHash, "ignored", tt.reason).WillReturnError(writeErr)
			notifications := 0
			l.SetUnmatchedNotifier(func(context.Context, scanner.Transfer, string) { notifications++ })
			credited, err := l.Process(t.Context(), tr, 6)
			if credited || !errors.Is(err, writeErr) {
				t.Fatalf("credited=%v error=%v", credited, err)
			}
			if creditor.calls != 0 || notifications != 0 {
				t.Fatalf("credit calls=%d notifications=%d before persistence", creditor.calls, notifications)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIgnoredNotificationFollowsSuccessfulPersistence(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		mock.ExpectClose()
		if err := db.Close(); err != nil {
			t.Errorf("close notification db: %v", err)
		}
	}()
	l := New(db, &countingCreditor{}, fixedResolver{userID: 42, ours: true}, 1, 10, 1)
	expectDepositState(mock, depositTransfer(100_000), stateRows())
	mock.ExpectExec(`INSERT INTO deposit_transactions`).WillReturnResult(sqlmock.NewResult(1, 1))
	notifications := 0
	l.SetUnmatchedNotifier(func(context.Context, scanner.Transfer, string) {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("notification preceded persistence: %v", err)
		}
		notifications++
	})
	credited, err := l.Process(t.Context(), depositTransfer(100_000), 6)
	if credited || err != nil || notifications != 1 {
		t.Fatalf("credited=%v err=%v notifications=%d", credited, err, notifications)
	}
}

func TestPendingUnassignedAddressRemainsDurableForLaterReconciliation(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "persisted", true: "write fails"}[fail], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				mock.ExpectClose()
				if err := db.Close(); err != nil {
					t.Errorf("close pending db: %v", err)
				}
			}()
			creditor := &countingCreditor{}
			l := New(db, creditor, fixedResolver{ours: true}, 1, 10, 1)
			expectation := mock.ExpectExec(`INSERT INTO deposit_transactions`)
			writeErr := errors.New("pending write failed")
			if fail {
				expectation.WillReturnError(writeErr)
			} else {
				expectation.WillReturnResult(sqlmock.NewResult(1, 1))
			}
			err = l.ProcessPending(t.Context(), depositTransfer(1_000_000), 6)
			if fail && !errors.Is(err, writeErr) || !fail && err != nil {
				t.Fatalf("got %v", err)
			}
			if creditor.calls != 0 {
				t.Fatal("unconfirmed deposit must not credit funds")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// recordGrantFixture marks the durable handoff for SQLite ledger-only tests.
func recordGrantFixture(ctx context.Context, db *sql.DB, req CreditRequest) error {
	if db == nil {
		return nil
	}
	_, tradeNo := OrderIdentity(req.Chain, req.TxHash, req.LogIndex)
	_, err := db.ExecContext(ctx, `UPDATE deposit_transactions
		SET credited_at = CURRENT_TIMESTAMP, provisional = $1,
		    order_id = COALESCE(order_id, (SELECT id FROM payment_orders WHERE out_trade_no = $2), 1)
		WHERE chain = $3 AND tx_hash = $4 AND log_index = $5`,
		req.Provisional, tradeNo, req.Chain, req.TxHash, req.LogIndex)
	return err
}

// newProvisionalLedger builds a ledger whose instant-credit switch is on.
func newProvisionalLedger(t *testing.T, minInstant float64) (*Ledger, sqlmock.Sqlmock, *countingCreditor) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		mock.ExpectClose()
		_ = db.Close()
	})
	creditor := &countingCreditor{}
	l := New(db, creditor, fixedResolver{userID: 42, ours: true}, 1, 10_000, 1)
	l.SetInstantCredit(true, minInstant)
	return l, mock, creditor
}

// TestInstantCreditLandsOncePerIdentity credits the first verified observation.
// A repeated observation of that same transfer must leave the credited balance
// unchanged and avoid a second creditor call.
func TestInstantCreditLandsOncePerIdentity(t *testing.T) {
	l, mock, creditor := newProvisionalLedger(t, 0)
	tr := depositTransfer(10_000_000) // 10 USDT, multiplier 1

	expectDepositState(mock, tr, stateRows()) // no row yet
	mock.ExpectExec(`INSERT INTO deposit_transactions`).
		WithArgs(tr.Chain, tr.TxHash, tr.LogIndex, tr.From, tr.To, tr.Token, "10", tr.BlockNumber, tr.BlockHash).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(markCreditedPattern).
		WithArgs(tr.Chain, tr.TxHash, tr.LogIndex, true, tr.BlockHash).
		WillReturnResult(sqlmock.NewResult(0, 1))

	credited, err := l.ProcessProvisional(t.Context(), tr, 6)
	if err != nil || !credited {
		t.Fatalf("first observation: credited=%v err=%v", credited, err)
	}
	if creditor.calls != 1 {
		t.Fatalf("creditor calls=%d want 1", creditor.calls)
	}

	// A second pass sees the credited row and must return without crediting.
	expectDepositState(mock, tr, stateRows().AddRow("credited", 88, 42, 10.0, "", nil, "", false))
	credited, err = l.ProcessProvisional(t.Context(), tr, 6)
	if err != nil || credited {
		t.Fatalf("repeat observation: credited=%v err=%v", credited, err)
	}
	if creditor.calls != 1 {
		t.Fatalf("creditor calls=%d after repeat, want 1", creditor.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// With the switch off the ledger must behave exactly like the old
// confirmation-gated path: record pending, never credit.
func TestInstantCreditDisabledRecordsPendingOnly(t *testing.T) {
	l, mock, creditor := newProvisionalLedger(t, 0)
	l.SetInstantCredit(false, 0)
	tr := depositTransfer(10_000_000)

	mock.ExpectExec(`INSERT INTO deposit_transactions`).
		WithArgs(tr.Chain, tr.TxHash, tr.LogIndex, tr.From, tr.To, tr.Token, "10", tr.BlockNumber, tr.BlockHash).
		WillReturnResult(sqlmock.NewResult(1, 1))
	credited, err := l.ProcessProvisional(t.Context(), tr, 6)
	if err != nil || credited {
		t.Fatalf("disabled: credited=%v err=%v", credited, err)
	}
	if creditor.calls != 0 {
		t.Fatalf("disabled path credited %d times", creditor.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A deposit below instant_credit_min_amount keeps the confirmation-gated path.
func TestInstantCreditFloorRecordsPendingOnly(t *testing.T) {
	l, mock, creditor := newProvisionalLedger(t, 100) // credited amount would be 10
	tr := depositTransfer(10_000_000)

	expectDepositState(mock, tr, stateRows())
	mock.ExpectExec(`INSERT INTO deposit_transactions`).
		WithArgs(tr.Chain, tr.TxHash, tr.LogIndex, tr.From, tr.To, tr.Token, "10", tr.BlockNumber, tr.BlockHash).
		WillReturnResult(sqlmock.NewResult(1, 1))
	credited, err := l.ProcessProvisional(t.Context(), tr, 6)
	if err != nil || credited {
		t.Fatalf("below floor: credited=%v err=%v", credited, err)
	}
	if creditor.calls != 0 {
		t.Fatalf("below-floor deposit credited %d times", creditor.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A revoked identity is terminal: neither the instant path nor the final path
// may credit it again, so a re-observed/re-mined transaction cannot mint money.
func TestRevokedDepositIsNeverCreditedAgain(t *testing.T) {
	l, mock, creditor := newProvisionalLedger(t, 0)
	tr := depositTransfer(10_000_000)

	for _, call := range []struct {
		name string
		fn   func() (bool, error)
	}{
		{"provisional", func() (bool, error) { return l.ProcessProvisional(t.Context(), tr, 6) }},
		{"final", func() (bool, error) { return l.Process(t.Context(), tr, 6) }},
	} {
		t.Run(call.name, func(t *testing.T) {
			// revocation already wrote status='reverted' + revoked_at.
			expectDepositState(mock, tr, stateRows().AddRow("reverted", 88, 42, 10.0, "", nil, "", false))
			credited, err := call.fn()
			if err != nil || credited {
				t.Fatalf("revoked row credited: credited=%v err=%v", credited, err)
			}
			if creditor.calls != 0 {
				t.Fatalf("revoked row credited %d times", creditor.calls)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestInstantCreditThenFinalCreditAddsBalanceOnce replays an already credited
// provisional observation through the final-credit path. Final processing must
// leave the original grant as the only balance change.
func TestInstantCreditThenFinalCreditAddsBalanceOnce(t *testing.T) {
	l, mock, creditor := newProvisionalLedger(t, 0)
	tr := depositTransfer(10_000_000)

	expectDepositState(mock, tr, stateRows())
	mock.ExpectExec(`INSERT INTO deposit_transactions`).
		WithArgs(tr.Chain, tr.TxHash, tr.LogIndex, tr.From, tr.To, tr.Token, "10", tr.BlockNumber, tr.BlockHash).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(markCreditedPattern).
		WithArgs(tr.Chain, tr.TxHash, tr.LogIndex, true, tr.BlockHash).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if credited, err := l.ProcessProvisional(t.Context(), tr, 6); err != nil || !credited {
		t.Fatalf("instant credit: credited=%v err=%v", credited, err)
	}

	// Finality is reached: the scanner calls Process, which must be a no-op
	// because the row is already credited.
	expectDepositState(mock, tr, stateRows().AddRow("credited", 88, 42, 10.0, "", nil, "", false))
	if credited, err := l.Process(t.Context(), tr, 6); err != nil || credited {
		t.Fatalf("final credit after instant: credited=%v err=%v", credited, err)
	}
	if creditor.calls != 1 {
		t.Fatalf("creditor calls=%d want exactly 1", creditor.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func stateRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"status", "order_id", "user_id", "amount", "native_price_usd", "native_price_at", "native_price_source", "grant_recorded"})
}

func expectDepositState(mock sqlmock.Sqlmock, tr scanner.Transfer, rows *sqlmock.Rows) {
	_, outTradeNo := OrderIdentity(tr.Chain, tr.TxHash, tr.LogIndex)
	mock.ExpectQuery(`(?s)SELECT d.status, COALESCE\(d.order_id, o.id\), o.user_id, o.amount.*LEFT JOIN payment_orders`).
		WithArgs(tr.Chain, tr.TxHash, tr.LogIndex, outTradeNo).WillReturnRows(rows)
}

func TestPendingOrderWithFailedLinkRestoresOriginalTerms(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	for _, schema := range []string{
		`CREATE TABLE payment_orders(id INTEGER PRIMARY KEY,user_id INTEGER,amount REAL,out_trade_no TEXT UNIQUE)`,
		`CREATE TABLE deposit_transactions(chain TEXT,tx_hash TEXT,log_index INTEGER,status TEXT,order_id INTEGER,native_price_usd TEXT,native_price_at TEXT,native_price_source TEXT,block_hash TEXT,provisional INTEGER NOT NULL DEFAULT 0,credited_at TEXT,revoked_at TEXT,revoke_reason TEXT)`,
	} {
		if _, err := db.ExecContext(t.Context(), schema); err != nil {
			t.Fatal(err)
		}
	}
	tr := depositTransfer(10_000_000)
	_, outTradeNo := OrderIdentity(tr.Chain, tr.TxHash, tr.LogIndex)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO payment_orders VALUES(88,42,67,?)`, outTradeNo); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO deposit_transactions VALUES(?,?,?,'pending',NULL,NULL,NULL,'',?,0,NULL,NULL,NULL)`, tr.Chain, tr.TxHash, tr.LogIndex, tr.BlockHash); err != nil {
		t.Fatal(err)
	}
	creditor := &countingCreditor{db: db}
	// No resolver is provided: the already-created order must recover even
	// after the address is disabled and new amount policies reject this sum.
	l := New(db, creditor, nil, 100, 1, 0)
	credited, err := l.Process(t.Context(), tr, 6)
	if err != nil || !credited || creditor.calls != 1 || creditor.requests[0].UserID != 42 || creditor.requests[0].CreditUSDT != 67 {
		t.Fatalf("unlinked recovery: credited=%v err=%v requests=%+v", credited, err, creditor.requests)
	}
}

// TestLinkedPendingRestoresFrozenOrderBeforeMutablePolicies retries a linked
// deposit after current admission rules change. Recovery pays the frozen amount
// to the original owner even when current policy would reject a new deposit.
func TestLinkedPendingRestoresFrozenOrderBeforeMutablePolicies(t *testing.T) {
	for _, setting := range []string{"minimum raised", "maximum lowered", "multiplier changed", "multiplier zero", "multiplier invalid", "address disabled"} {
		t.Run(setting, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			creditor := &countingCreditor{}
			tr := depositTransfer(10_000_000)
			min, max, multiplier := 1.0, 100.0, 6.7
			switch setting {
			case "minimum raised":
				min = 100
			case "maximum lowered":
				max = 1
			case "multiplier changed":
				multiplier = 6
			case "multiplier zero":
				multiplier = 0
			case "multiplier invalid":
				multiplier = math.NaN()
			}
			// Disabled/unassigned now resolves to user zero. Only the linked
			// order's owner and frozen amount may drive recovery.
			l := New(db, creditor, fixedResolver{ours: true}, min, max, multiplier)
			expectDepositState(mock, tr, stateRows().AddRow("pending", 88, 42, 67.0, "", nil, "", false))
			mock.ExpectExec(markCreditedPattern).
				WithArgs(tr.Chain, tr.TxHash, tr.LogIndex, false, tr.BlockHash).
				WillReturnResult(sqlmock.NewResult(0, 1))
			credited, err := l.Process(t.Context(), tr, 6)
			if !credited || err != nil || creditor.calls != 1 {
				t.Fatalf("credited=%v err=%v calls=%d", credited, err, creditor.calls)
			}
			req := creditor.requests[0]
			if req.UserID != 42 || req.CreditUSDT != 67 || req.Amount.Cmp(tr.Amount) != 0 {
				t.Fatalf("original terms changed: %+v", req)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResolvedDepositDoesNotConsultPolicyOrOverwriteState(t *testing.T) {
	for _, status := range []string{"credited", "ignored", "reverted"} {
		t.Run(status, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			creditor := &countingCreditor{}
			tr := depositTransfer(10_000_000)
			l := New(db, creditor, nil, 100, 1, 0) // nil resolver must not be called.
			expectDepositState(mock, tr, stateRows().AddRow(status, 88, 42, 67.0, "", nil, "", false))
			credited, err := l.Process(t.Context(), tr, 6)
			if credited || err != nil || creditor.calls != 0 {
				t.Fatalf("credited=%v err=%v calls=%d", credited, err, creditor.calls)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLinkedPendingWithMissingOrderFailsClosed(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	creditor := &countingCreditor{}
	tr := depositTransfer(10_000_000)
	l := New(db, creditor, nil, 100, 1, 0)
	expectDepositState(mock, tr, stateRows().AddRow("pending", 88, nil, nil, "", nil, "", false))
	mock.ExpectExec(`UPDATE deposit_transactions SET status = 'dead_letter'`).
		WithArgs("dead_letter:credit_evidence", tr.Chain, tr.TxHash, tr.LogIndex, tr.BlockHash).
		WillReturnResult(sqlmock.NewResult(0, 1))
	credited, err := l.Process(t.Context(), tr, 6)
	if credited || err == nil || creditor.calls != 0 {
		t.Fatalf("credited=%v err=%v calls=%d", credited, err, creditor.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestCreditedCASPreservesReorgAndConcurrentCompletion changes the stored
// state before credit publication. A losing publication preserves the other
// writer's state and exposes conflicts that need reconciliation.
func TestCreditedCASPreservesReorgAndConcurrentCompletion(t *testing.T) {
	for _, concurrentStatus := range []string{"reverted", "ignored", "credited"} {
		t.Run(concurrentStatus, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			creditor := &countingCreditor{}
			tr := depositTransfer(10_000_000)
			l := New(db, creditor, nil, 100, 1, 0)
			expectDepositState(mock, tr, stateRows().AddRow("pending", 88, 42, 67.0, "", nil, "", false))
			mock.ExpectExec(markCreditedPattern).
				WithArgs(tr.Chain, tr.TxHash, tr.LogIndex, false, tr.BlockHash).
				WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectQuery(`SELECT status FROM deposit_transactions`).WithArgs(tr.Chain, tr.TxHash, tr.LogIndex).
				WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow(concurrentStatus))
			credited, err := l.Process(t.Context(), tr, 6)
			if credited || (err != nil) != (concurrentStatus == "ignored") {
				t.Fatalf("credited=%v err=%v", credited, err)
			}
			if concurrentStatus == "ignored" && !strings.Contains(err.Error(), "manual reconciliation") {
				t.Fatalf("missing reconciliation error: %v", err)
			}
			if creditor.calls != 1 {
				t.Fatalf("fulfillment calls=%d", creditor.calls)
			}
			// No balance debit, compensating order or unguarded status write.
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestIgnoredCASDoesNotEraseConcurrentlyLinkedOrder(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	creditor := &countingCreditor{}
	tr := depositTransfer(10_000_000)
	l := New(db, creditor, fixedResolver{userID: 42, ours: true}, 100, 1000, 1)
	expectDepositState(mock, tr, stateRows().AddRow("pending", nil, nil, nil, "", nil, "", false))
	mock.ExpectExec(`(?s)INSERT INTO deposit_transactions.*WHERE deposit_transactions.status = 'pending'.*AND deposit_transactions.order_id IS NULL`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	credited, err := l.Process(t.Context(), tr, 6)
	if credited || err == nil || creditor.calls != 0 {
		t.Fatalf("credited=%v err=%v calls=%d", credited, err, creditor.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestFailedCreditedWriteCanResumeLinkedOrder injects a failed status write
// after the creditor returns. A restart of processing uses the same frozen
// amount and attempts publication for the same block.
func TestFailedCreditedWriteCanResumeLinkedOrder(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	creditor := &countingCreditor{}
	tr := depositTransfer(10_000_000)
	l := New(db, creditor, nil, 100, 1, 0)
	writeErr := errors.New("mark credited unavailable")
	for attempt := 0; attempt < 2; attempt++ {
		expectDepositState(mock, tr, stateRows().AddRow("pending", 88, 42, 67.0, "", nil, "", false))
		expectation := mock.ExpectExec(markCreditedPattern).
			WithArgs(tr.Chain, tr.TxHash, tr.LogIndex, false, tr.BlockHash)
		if attempt == 0 {
			expectation.WillReturnError(writeErr)
		} else {
			expectation.WillReturnResult(sqlmock.NewResult(0, 1))
		}
		credited, err := l.Process(t.Context(), tr, 6)
		if attempt == 0 && (credited || !errors.Is(err, writeErr)) {
			t.Fatalf("first credited=%v err=%v", credited, err)
		}
		if attempt == 1 && (!credited || err != nil) {
			t.Fatalf("retry credited=%v err=%v", credited, err)
		}
	}
	if creditor.calls != 2 || creditor.requests[0].CreditUSDT != 67 || creditor.requests[1].CreditUSDT != 67 {
		t.Fatal("retry lost frozen order amount")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
