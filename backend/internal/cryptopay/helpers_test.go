package cryptopay

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"

	dbent "github.com/TokenFlux/TokenRouter/ent"
	"github.com/TokenFlux/TokenRouter/ent/enttest"
	_ "github.com/TokenFlux/TokenRouter/ent/runtime"
	"github.com/TokenFlux/TokenRouter/internal/cryptopay/ledger"
	"github.com/TokenFlux/TokenRouter/internal/cryptopay/rpc"
	"github.com/TokenFlux/TokenRouter/internal/cryptopay/scanner"
)

const (
	confirmTransferTopic = "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
	confirmUSDT          = "0x55d398326f99059ff775485246999027b3197955"
	confirmUSDC          = "0x8ac76a51cc950d9822d68b83fe1ad97b32cd580d"
)

var (
	confirmFrom     = "0x" + strings.Repeat("22", 20)
	confirmTo       = "0x" + strings.Repeat("33", 20)
	confirmStranger = "0x" + strings.Repeat("77", 20)
	// tenUSDT is 10 tokens at BSC's 18 decimals.
	tenUSDT = new(big.Int).Mul(big.NewInt(10), new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
)

// fakeNodeOpts fixes a fake node's behaviour before its server starts, so the
// handler goroutines only ever read it.
type fakeNodeOpts struct {
	chainID  uint64
	head     uint64
	delay    time.Duration
	status   int    // forced HTTP status; the body carries a marker secret
	body     string // forced raw 200 body
	rpcError bool   // JSON-RPC error object whose message carries a marker secret
	badChain string // forced eth_chainId result string
}

// fakeNode is an httptest JSON-RPC node. Nothing in the tests touches a real
// RPC provider or a real database.
type fakeNode struct {
	srv        *httptest.Server
	head       atomic.Uint64
	chainCalls atomic.Int64
	blockCalls atomic.Int64
	otherCalls atomic.Int64
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

// fakeTicker hands the monitor channels the test fires by hand.
type fakeTicker struct {
	mu      sync.Mutex
	chans   []chan time.Time
	stopped int
}

// statusHarness is a Service wired to a mocked database and fake RPC nodes.
// Nothing here touches a real database, a real RPC provider or the network.
type statusHarness struct {
	svc   *Service
	mock  sqlmock.Sqlmock
	clock *fakeClock
	tick  *fakeTicker
}

type cursorRow struct {
	chain   string
	block   int64
	updated time.Time
}

type chainReceipt struct {
	status    string
	block     uint64
	blockHash string
	logs      []map[string]any
	absent    bool
}

// fakeChain is an in-process JSON-RPC node. It listens on loopback only and
// never reads configuration or the environment.
type fakeChain struct {
	mu        sync.Mutex
	chainID   string
	receipts  map[string]*chainReceipt
	canonical map[uint64]string
	calls     map[string]int
	httpFail  map[string]int
	server    *httptest.Server
}

// countingProcessor wraps the real ledger so a test can prove how often the
// ledger was reached, and can stand in for outcomes that need PostgreSQL-only
// SQL (the ledger's ignore path uses NOW()).
type countingProcessor struct {
	inner   depositProcessor
	mu      sync.Mutex
	calls   int
	before  func()
	replace func(context.Context, scanner.Transfer, int) (bool, error)
}

type memoryDepositLocker struct {
	mu   sync.Mutex
	held map[int64]bool
	err  error
}

// grantAllLocker proves that exactly-once crediting does not depend on the lock.
type grantAllLocker struct{}

type confirmResolver struct {
	userID  int64
	deposit map[string]bool
	system  map[string]bool
}

type depositSpec struct {
	chain, token, from, to string
	tx, blockHash          string
	index                  int
	amount                 string // NUMERIC text as PostgreSQL renders it
	block                  uint64
	status, reason         string
}

type confirmEnv struct {
	db        *sql.DB
	client    *dbent.Client
	userID    int64
	chain     *fakeChain
	resolver  *confirmResolver
	processor *countingProcessor
	locker    *memoryDepositLocker
	confirmer *depositConfirmer
}

func newFakeNode(t *testing.T, o fakeNodeOpts) *fakeNode {
	t.Helper()
	n := &fakeNode{}
	n.head.Store(o.head)
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "eth_chainId":
			n.chainCalls.Add(1)
		case "eth_blockNumber":
			n.blockCalls.Add(1)
		default:
			n.otherCalls.Add(1)
		}
		if o.delay > 0 {
			select {
			case <-time.After(o.delay):
			case <-r.Context().Done():
				return
			}
		}
		switch {
		case o.status != 0:
			w.WriteHeader(o.status)
			_, _ = w.Write([]byte(`{"error":"SECRETBODY"}`))
		case o.body != "":
			_, _ = w.Write([]byte(o.body))
		case o.rpcError:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32005,"message":"limit reached SECRETRPC"}}`))
		default:
			result := hexutil.EncodeUint64(n.head.Load())
			if req.Method == "eth_chainId" {
				result = hexutil.EncodeUint64(o.chainID)
				if o.badChain != "" {
					result = o.badChain
				}
			}
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":%q}`, result)
		}
	}))
	t.Cleanup(n.srv.Close)
	return n
}

// poolFor maps a configured endpoint to its fake node. The pool talks to the
// httptest server directly; configured endpoints never use this constructor.
func poolFor(nodes map[string]*fakeNode) func(string) *rpc.Pool {
	return func(endpoint string) *rpc.Pool {
		if n := nodes[endpoint]; n != nil {
			return rpc.NewPoolWithHTTPClient(n.srv.URL, n.srv.Client())
		}
		return rpc.NewPool("") // no endpoint: every call fails
	}
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func (f *fakeTicker) New(time.Duration) (<-chan time.Time, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan time.Time, 8)
	f.chans = append(f.chans, ch)
	return ch, func() {
		f.mu.Lock()
		f.stopped++
		f.mu.Unlock()
	}
}

func (f *fakeTicker) Created() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.chans)
}

func (f *fakeTicker) Stopped() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

func (f *fakeTicker) Fire(i int) {
	f.mu.Lock()
	ch := f.chans[i]
	f.mu.Unlock()
	ch <- time.Now()
}

// newTestMonitor builds a standalone monitor whose clock, ticker and pools are fakes.
func newTestMonitor(t *testing.T, nodes map[string]*fakeNode) (*chainMonitor, *fakeClock, *fakeTicker) {
	t.Helper()
	clock, tick := newFakeClock(), &fakeTicker{}
	m := &chainMonitor{now: clock.Now, newPool: poolFor(nodes), newTicker: tick.New}
	t.Cleanup(m.stop)
	return m, clock, tick
}

func poolOf(endpoints ...string) *rpc.Pool { return rpc.NewPool(strings.Join(endpoints, ",")) }

func newStatusHarness(t *testing.T, cfg CryptoConfig, pools map[string]*rpc.Pool, nodes map[string]*fakeNode) *statusHarness {
	t.Helper()
	// Exact SQL matching: a read that turns into a write, or a third query,
	// fails the test instead of slipping through a loose regexp.
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	clock, tick := newFakeClock(), &fakeTicker{}
	svc := &Service{db: db, config: cfg, pools: pools}
	svc.chainMon.now = clock.Now
	svc.chainMon.newPool = poolFor(nodes)
	svc.chainMon.newTicker = tick.New
	t.Cleanup(svc.chainMon.stop)
	return &statusHarness{svc: svc, mock: mock, clock: clock, tick: tick}
}

func (h *statusHarness) expectCursors(rows ...cursorRow) {
	r := sqlmock.NewRows([]string{"chain", "last_block", "updated_at"})
	for _, c := range rows {
		r.AddRow(c.chain, c.block, c.updated)
	}
	h.mock.ExpectQuery(chainCursorQuery).WillReturnRows(r)
}

func (h *statusHarness) expectCursorsError(err error) {
	h.mock.ExpectQuery(chainCursorQuery).WillReturnError(err)
}

func (h *statusHarness) expectPending(counts map[string]int64) {
	r := sqlmock.NewRows([]string{"chain", "count"})
	for chain, n := range counts {
		r.AddRow(chain, n)
	}
	h.mock.ExpectQuery(chainPendingQuery).WillReturnRows(r)
}

func (h *statusHarness) expectPendingError(err error) {
	h.mock.ExpectQuery(chainPendingQuery).WillReturnError(err)
}

// byChain indexes a report by chain name.
func byChain(rep *ChainStatusReport) map[string]ChainStatus {
	out := make(map[string]ChainStatus, len(rep.Chains))
	for _, c := range rep.Chains {
		out[c.Chain] = c
	}
	return out
}

func chainNames(rep *ChainStatusReport) []string {
	names := make([]string, 0, len(rep.Chains))
	for _, c := range rep.Chains {
		names = append(names, c.Chain)
	}
	return names
}

// bscOnly is a config with a single enabled chain and its pool.
func bscOnly(endpoints ...string) (CryptoConfig, map[string]*rpc.Pool) {
	cfg := DefaultCryptoConfig()
	cfg.Chains = []string{ChainBSC}
	cfg.RPC = map[string]string{ChainBSC: strings.Join(endpoints, ",")}
	return cfg, map[string]*rpc.Pool{ChainBSC: poolOf(endpoints...)}
}

func newFakeChain(t *testing.T) *fakeChain {
	t.Helper()
	c := &fakeChain{
		chainID: "0x38", receipts: map[string]*chainReceipt{}, canonical: map[uint64]string{},
		calls: map[string]int{}, httpFail: map[string]int{},
	}
	c.server = httptest.NewServer(c.handler(t))
	t.Cleanup(c.server.Close)
	return c
}

func (c *fakeChain) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("malformed RPC request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		c.calls[req.Method]++
		failure := c.httpFail[req.Method]
		var result any
		switch req.Method {
		case "eth_chainId":
			result = c.chainID
		case "eth_getTransactionReceipt":
			var hash string
			_ = json.Unmarshal(req.Params[0], &hash)
			if rc := c.receipts[strings.ToLower(hash)]; rc != nil && !rc.absent {
				result = map[string]any{
					"status": rc.status, "blockNumber": fmt.Sprintf("0x%x", rc.block),
					"blockHash": rc.blockHash, "transactionHash": strings.ToLower(hash), "logs": rc.logs,
				}
			}
		case "eth_getBlockByNumber":
			var number string
			_ = json.Unmarshal(req.Params[0], &number)
			var n uint64
			_, _ = fmt.Sscanf(number, "0x%x", &n)
			if hash, ok := c.canonical[n]; ok {
				result = map[string]any{"number": number, "hash": hash}
			}
		default:
			t.Errorf("unexpected RPC method %q", req.Method)
		}
		c.mu.Unlock()
		if failure != 0 {
			w.WriteHeader(failure)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	}
}

func (c *fakeChain) pool() *rpc.Pool {
	return rpc.NewPoolWithHTTPClient(c.server.URL, c.server.Client())
}

func (c *fakeChain) callCount(method string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[method]
}

func (c *fakeChain) totalCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, n := range c.calls {
		total += n
	}
	return total
}

func chainTransferLog(token, from, to string, raw *big.Int, block uint64, blockHash, tx string, index int) map[string]any {
	pad := func(address string) string {
		return "0x" + strings.Repeat("0", 24) + strings.TrimPrefix(strings.ToLower(address), "0x")
	}
	return map[string]any{
		"address": token, "topics": []string{confirmTransferTopic, pad(from), pad(to)},
		"data": fmt.Sprintf("0x%064x", raw), "blockNumber": fmt.Sprintf("0x%x", block),
		"transactionHash": tx, "logIndex": fmt.Sprintf("0x%x", index), "blockHash": blockHash,
	}
}

func (p *countingProcessor) Process(ctx context.Context, tr scanner.Transfer, decimals int) (bool, error) {
	p.mu.Lock()
	p.calls++
	before, replace := p.before, p.replace
	p.mu.Unlock()
	if before != nil {
		before()
	}
	if replace != nil {
		return replace(ctx, tr, decimals)
	}
	return p.inner.Process(ctx, tr, decimals)
}

func (p *countingProcessor) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (l *memoryDepositLocker) TryLock(_ context.Context, id int64) (func() error, error) {
	if l.err != nil {
		return nil, l.err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held == nil {
		l.held = map[int64]bool{}
	}
	if l.held[id] {
		return nil, nil
	}
	l.held[id] = true
	return func() error {
		l.mu.Lock()
		defer l.mu.Unlock()
		delete(l.held, id)
		return nil
	}, nil
}

func (grantAllLocker) TryLock(context.Context, int64) (func() error, error) {
	return func() error { return nil }, nil
}

func (r *confirmResolver) IsOwnAddress(_ context.Context, _, address string) (bool, error) {
	a := strings.ToLower(address)
	return r.system[a] || r.deposit[a], nil
}

func (r *confirmResolver) ResolveDeposit(_ context.Context, _, address string) (int64, bool, error) {
	if r.deposit[strings.ToLower(address)] {
		return r.userID, true, nil
	}
	return 0, false, nil
}

func newConfirmEnv(t *testing.T) *confirmEnv {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	// One connection keeps the in-memory database alive and serialises
	// statements, like the existing creditor tests.
	db.SetMaxOpenConns(1)
	_, err = db.Exec("PRAGMA foreign_keys = ON")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	client := enttest.NewClient(t, enttest.WithOptions(dbent.Driver(entsql.OpenDB(dialect.SQLite, db))))
	t.Cleanup(func() { _ = client.Close() })
	_, err = db.Exec(`CREATE TABLE deposit_transactions (
		id INTEGER PRIMARY KEY AUTOINCREMENT, chain TEXT NOT NULL, tx_hash TEXT NOT NULL,
		log_index INTEGER NOT NULL DEFAULT 0, from_address TEXT NOT NULL, to_address TEXT NOT NULL,
		token TEXT NOT NULL, amount TEXT NOT NULL, block_number INTEGER NOT NULL,
		block_hash TEXT NOT NULL DEFAULT '', confirmations INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'pending', reason TEXT NOT NULL DEFAULT '', order_id INTEGER,
		last_attempt_at TEXT, native_price_usd TEXT, native_price_at TIMESTAMP, native_price_source TEXT NOT NULL DEFAULT '',
		provisional INTEGER NOT NULL DEFAULT 0, credited_at TIMESTAMP, revoked_at TIMESTAMP, revoke_reason TEXT,
		UNIQUE(chain, tx_hash, log_index))`)
	require.NoError(t, err)
	user, err := client.User.Create().SetEmail("manual-confirm@example.invalid").SetPasswordHash("test-only").Save(t.Context())
	require.NoError(t, err)

	creditor := &DepositCreditor{entClient: client, db: db}
	creditor.fulfill = func(ctx context.Context, id int64) error {
		return fulfillDepositFixture(ctx, client, id)
	}

	resolver := &confirmResolver{userID: user.ID, deposit: map[string]bool{strings.ToLower(confirmTo): true}, system: map[string]bool{}}
	fc := newFakeChain(t)
	processor := &countingProcessor{inner: ledger.New(db, creditor, resolver, 0.001, 10000, 6.7)}
	locker := &memoryDepositLocker{}
	return &confirmEnv{
		db: db, client: client, userID: user.ID, chain: fc, resolver: resolver,
		processor: processor, locker: locker,
		confirmer: &depositConfirmer{
			db: db, processor: processor, locker: locker, adminID: 9,
			minCredit: 0.001, maxCredit: 10000,
			pool: func(chain string) (*rpc.Pool, error) {
				if chain != "bsc" {
					return nil, errDepositNoRPC
				}
				return fc.pool(), nil
			},
		},
	}
}

func (e *confirmEnv) spec(n int) depositSpec {
	return depositSpec{
		chain: "bsc", token: confirmUSDT, from: confirmFrom, to: confirmTo,
		tx: fmt.Sprintf("0x%064x", 0xabc000+n), blockHash: fmt.Sprintf("0x%064x", 0xdef000+n),
		index: 7, amount: "10.000000000000000000", block: uint64(1000 + n), status: "pending",
	}
}

func (e *confirmEnv) insert(t *testing.T, s depositSpec) int64 {
	t.Helper()
	res, err := e.db.ExecContext(t.Context(), `INSERT INTO deposit_transactions
		(chain, tx_hash, log_index, from_address, to_address, token, amount, block_number, block_hash, status, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		s.chain, s.tx, s.index, s.from, s.to, s.token, s.amount, s.block, s.blockHash, s.status, s.reason)
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)
	return id
}

// publish makes the chain agree with a spec: a successful receipt carrying one
// Transfer log worth ten tokens, in a canonical block.
func (e *confirmEnv) publish(s depositSpec) {
	e.chain.mu.Lock()
	defer e.chain.mu.Unlock()
	e.chain.receipts[s.tx] = &chainReceipt{
		status: "0x1", block: s.block, blockHash: s.blockHash,
		logs: []map[string]any{chainTransferLog(s.token, s.from, s.to, tenUSDT, s.block, s.blockHash, s.tx, s.index)},
	}
	e.chain.canonical[s.block] = s.blockHash
}

func (e *confirmEnv) seed(t *testing.T, n int) (int64, depositSpec) {
	t.Helper()
	s := e.spec(n)
	e.publish(s)
	return e.insert(t, s), s
}

func (e *confirmEnv) confirm(id int64) DepositConfirmResult {
	return e.confirmer.confirmOne(context.Background(), map[string]depositPoolEntry{}, id)
}

func (e *confirmEnv) balance(t *testing.T) float64 {
	t.Helper()
	return e.client.User.GetX(t.Context(), e.userID).Balance
}

func (e *confirmEnv) orders(t *testing.T) int {
	t.Helper()
	return e.client.PaymentOrder.Query().CountX(t.Context())
}

// snapshot captures everything a rejected confirmation must leave untouched.
func (e *confirmEnv) snapshot(t *testing.T) string {
	t.Helper()
	rows, err := e.db.QueryContext(t.Context(), `SELECT id, status, reason, COALESCE(order_id, 0), tx_hash,
		log_index, block_number, block_hash, COALESCE(last_attempt_at, '') FROM deposit_transactions ORDER BY id`)
	require.NoError(t, err)
	var lines []string
	for rows.Next() {
		var id, order, block int64
		var index int
		var status, reason, tx, hash, attempt string
		require.NoError(t, rows.Scan(&id, &status, &reason, &order, &tx, &index, &block, &hash, &attempt))
		lines = append(lines, fmt.Sprintf("%d|%s|%s|%d|%s|%d|%d|%s|%s", id, status, reason, order, tx, index, block, hash, attempt))
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	return strings.Join(lines, "\n") + fmt.Sprintf("\nbalance=%v orders=%d", e.balance(t), e.orders(t))
}

func (e *confirmEnv) status(t *testing.T, id int64) string {
	t.Helper()
	var status string
	require.NoError(t, e.db.QueryRowContext(t.Context(), `SELECT status FROM deposit_transactions WHERE id = $1`, id).Scan(&status))
	return status
}
