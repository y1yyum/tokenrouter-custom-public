package scanner

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/cryptopay/rpc"
)

// This in-process driver models PostgreSQL SESSION locks, including reentrant
// acquisition and automatic release on physical disconnect. Separate sql.DB
// pools share only the mock server, just as separate instances share PostgreSQL.
// No DSN, environment variable, network database or production config is used.
type scannerMockPG struct {
	mu               sync.Mutex
	nextSession      int
	holder           int
	depth            int
	events           []scannerPGEvent
	closed           []int
	acquire, release *scannerPGReply
	beforeWork       func(context.Context, string) error
	openErr          error
	key              int64 // expected advisory key; zero means the shared deposit-scan key
}

type scannerPGReply struct {
	value driver.Value
	err   error
	empty bool
}

type scannerPGEvent struct {
	step, query   string
	session       int
	ctxErr        error
	cleanupBudget time.Duration
}

type scannerPGDriver struct{ pg *scannerMockPG }

type scannerPGConn struct {
	pg      *scannerMockPG
	session int
}

type scannerPGRows struct {
	columns []string
	values  [][]driver.Value
}

func TestChainPauseDelayGrowsAndIsCapped(t *testing.T) {
	interval := 5 * time.Second
	if got := chainPauseDelay(0, interval); got != 0 {
		t.Fatalf("no failures, no delay: %v", got)
	}
	if got := chainPauseDelay(1, interval); got != 0 {
		t.Fatalf("the first failure is not delayed (scanAll already retries once): %v", got)
	}
	within := func(got, want time.Duration) bool { // +-20% jitter
		return got >= want-want/5 && got <= want+want/5
	}
	for failures, want := range map[int]time.Duration{2: 5 * time.Second, 3: 10 * time.Second, 4: 20 * time.Second, 5: 40 * time.Second} {
		for range 50 {
			if got := chainPauseDelay(failures, interval); !within(got, want) {
				t.Fatalf("failures=%d delay=%v, want about %v", failures, got, want)
			}
		}
	}
	for _, failures := range []int{6, 7, 20, 1000} {
		for range 50 {
			if got := chainPauseDelay(failures, interval); !within(got, maxChainPause) {
				t.Fatalf("failures=%d delay=%v must stay near the %v cap", failures, got, maxChainPause)
			}
		}
	}
	if got := chainPauseDelay(3, 0); got == 0 {
		t.Fatal("a worker without an interval still paces stalled chains")
	}
}

func TestHeadStageFailedOnlyMatchesTheHeadLookup(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"head", stageErr("polygon", "head", 0, 0, boom), true},
		{"head joined with others", errors.Join(stageErr("polygon", "security", 1, 2, boom), stageErr("polygon", "head", 0, 0, boom)), true},
		{"deposit", stageErr("polygon", "deposit", 1, 2, boom), false},
		{"security", stageErr("polygon", "security", 1, 2, boom), false},
	} {
		if got := headStageFailed(tc.err); got != tc.want {
			t.Errorf("%s: headStageFailed = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// chainAt builds the per-pass coverage a scan of `polygon` leaves behind: the
// cursor it started from and the head it saw.
func chainAt(w *Worker, head, startCursor uint64) ChainConfig {
	w.noteHead("polygon", head)
	w.noteCursor("polygon", startCursor)
	return ChainConfig{Chain: "polygon", Confirmations: 5}
}

func TestForwardStalledNeedsPendingWorkAndNoProgress(t *testing.T) {
	w := &Worker{}
	chain := chainAt(w, 1000, 100) // confirmed head 995, cursor 100: 895 blocks waiting
	if !w.forwardStalled(chain) {
		t.Fatal("blocks were waiting and the cursor did not move")
	}
	w.noteCursor("polygon", 150) // the pass moved it, then failed later
	if w.forwardStalled(chain) {
		t.Fatal("a pass that advanced the cursor is progress, not a stall")
	}

	caughtUp := &Worker{}
	chain = chainAt(caughtUp, 1000, 995) // nothing beyond the cursor
	if caughtUp.forwardStalled(chain) {
		t.Fatal("a caught-up chain has nothing to stall on")
	}

	blind := &Worker{} // never learned its head or cursor this pass
	if blind.forwardStalled(ChainConfig{Chain: "polygon", Confirmations: 5}) {
		t.Fatal("without evidence of waiting work there is no stall")
	}
}

func TestNoteChainPassPausesAfterRepeatedStallsOnly(t *testing.T) {
	w := &Worker{interval: 5 * time.Second}
	chain := chainAt(w, 1000, 100)
	now := time.Date(2026, 11, 24, 2, 0, 0, 0, time.UTC)
	stalled := stageErr("polygon", "deposit", 101, 200, errors.New("rpc down"))

	w.noteChainPass(chain, stalled, now)
	if _, paused := w.forwardPause("polygon", now); paused {
		t.Fatal("one stall must not pause the forward scan")
	}
	w.noteChainPass(chain, stalled, now)
	if _, paused := w.forwardPause("polygon", now); !paused {
		t.Fatal("the second consecutive stall must pause it")
	}
	if _, paused := w.forwardPause("polygon", now.Add(2*time.Minute)); paused {
		t.Fatal("a pause is bounded by the cap and must lapse")
	}
	if _, paused := w.forwardPause("bsc", now); paused {
		t.Fatal("pausing one chain must not touch another")
	}
	// A batch-level security failure that holds the cursor is a stall too, whatever its stage label says.
	other := &Worker{interval: 5 * time.Second}
	chain = chainAt(other, 1000, 100)
	for range 2 {
		other.noteChainPass(chain, stageErr("polygon", "security", 101, 200, errors.New("guard unavailable")), now)
	}
	if _, paused := other.forwardPause("polygon", now); !paused {
		t.Fatal("a security-stage failure that froze the cursor must pause the chain")
	}
	// recovery clears the streak completely, so the next stall starts from zero again
	w.noteChainPass(chain, nil, now)
	w.noteChainPass(chain, stalled, now)
	if _, paused := w.forwardPause("polygon", now); paused {
		t.Fatal("a successful pass must reset the stall streak")
	}
}

func TestNoteChainPassIgnoresFailuresThatLeftTheCursorFree(t *testing.T) {
	now := time.Now()
	// caught up: head-window and replay failures are not a stall
	w := &Worker{interval: 5 * time.Second}
	chain := chainAt(w, 1000, 995)
	for range 6 {
		w.noteChainPass(chain, stageErr("polygon", "replay", 1, 2, errors.New("late replay failed")), now)
		w.noteChainPass(chain, stageErr("polygon", "security", 1, 2, errors.New("head window failed")), now)
	}
	if _, paused := w.forwardPause("polygon", now); paused {
		t.Fatal("failures on a chain with nothing to move must never pause it")
	}
	// work was waiting but the pass moved the cursor before failing: progress
	progressed := &Worker{interval: 5 * time.Second}
	chain = chainAt(progressed, 1000, 100)
	progressed.noteCursor("polygon", 300)
	for range 6 {
		progressed.noteChainPass(chain, stageErr("polygon", "deposit", 301, 400, errors.New("timed out after progress")), now)
	}
	if _, paused := progressed.forwardPause("polygon", now); paused {
		t.Fatal("a pass that advanced the cursor must not be treated as a stall")
	}
	// a failed head lookup says nothing about the forward scan: the pass never reached it
	blind := &Worker{interval: 5 * time.Second}
	for range 6 {
		blind.noteChainPass(ChainConfig{Chain: "polygon", Confirmations: 5}, stageErr("polygon", "head", 0, 0, errors.New("no head")), now)
	}
	if _, paused := blind.forwardPause("polygon", now); paused {
		t.Fatal("head lookup failures alone must never pause the forward scan")
	}
}

// The Polygon incident: a rate limited head call about once a minute renewed the
// pause on every tick, so the pause never lapsed and the forward scan - which the
// head stage does not feed - starved for hours. A failed head lookup is neutral: it
// must leave an active pause exactly as it is and let it lapse on schedule.
func TestHeadLookupFailuresDoNotRenewAnActivePause(t *testing.T) {
	w := &Worker{interval: 5 * time.Second}
	chain := chainAt(w, 1000, 100)
	now := time.Date(2026, 11, 24, 2, 0, 0, 0, time.UTC)
	stalled := stageErr("polygon", "deposit", 101, 200, errors.New("rpc down"))
	w.noteChainPass(chain, stalled, now)
	w.noteChainPass(chain, stalled, now)
	w.replayMu.Lock()
	before := *w.chainPause["polygon"]
	w.replayMu.Unlock()

	for i := range 30 {
		headFailure := stageErr("polygon", "head", 0, 0, errors.New("HTTP 429"))
		w.noteChainPass(chain, headFailure, now.Add(time.Duration(i)*time.Second))
		w.noteChainPass(chain, errors.Join(headFailure, stageErr("polygon", "observe", 0, 0, errors.New("429"))), now.Add(time.Duration(i)*time.Second))
	}
	w.replayMu.Lock()
	after := *w.chainPause["polygon"]
	w.replayMu.Unlock()
	if after != before {
		t.Fatalf("head lookup failures changed the pause: %+v -> %+v", before, after)
	}
	if _, paused := w.forwardPause("polygon", before.next.Add(time.Millisecond)); paused {
		t.Fatal("the pause must lapse on schedule however often the head lookup fails meanwhile")
	}
	// a real forward stall after the pause lapsed still counts, as before
	w.noteChainPass(chain, stalled, before.next.Add(time.Second))
	w.replayMu.Lock()
	renewed := *w.chainPause["polygon"]
	w.replayMu.Unlock()
	if renewed.failures != before.failures+1 {
		t.Fatalf("a genuine forward stall must still extend the streak: %+v -> %+v", before, renewed)
	}
}

// A pass whose forward scan was skipped on purpose made no attempt: it must not
// count as one more failure (that would run the delay up to the cap within five
// ticks) and must not clear the pause either.
func TestSkippedForwardAttemptsDoNotChangeThePause(t *testing.T) {
	w := &Worker{interval: 5 * time.Second}
	chain := chainAt(w, 1000, 100)
	now := time.Date(2026, 11, 24, 2, 0, 0, 0, time.UTC)
	stalled := stageErr("polygon", "deposit", 101, 200, errors.New("rpc down"))
	w.noteChainPass(chain, stalled, now)
	w.noteChainPass(chain, stalled, now)
	w.replayMu.Lock()
	before := *w.chainPause["polygon"]
	w.replayMu.Unlock()

	skipped := stageErr("polygon", "deposit", 101, 995, &chainPausedError{failures: 2, wait: 3 * time.Second})
	for i := range 10 {
		w.noteChainPass(chain, skipped, now.Add(time.Duration(i)*time.Second))
	}
	w.replayMu.Lock()
	after := *w.chainPause["polygon"]
	w.replayMu.Unlock()
	if after != before {
		t.Fatalf("skipped attempts changed the pause: %+v -> %+v", before, after)
	}
}

// End to end through the real pass loop. A chain whose forward scan keeps failing
// without moving the cursor stops spending requests on the forward batches, but
// its head stage still runs every pass and says so, manual scans always run, and
// one healthy pass resets everything.
func TestStalledForwardScanBacksOffWhileHeadStageAndManualScansKeepRunning(t *testing.T) {
	pg := &scannerMockPG{}
	w, fixture := scannerLockFixture(t, pg)
	setFailure := func(method string) {
		fixture.mu.Lock()
		fixture.httpFail = method
		fixture.mu.Unlock()
	}
	setFailure("eth_getLogs") // every forward batch is answered with HTTP 503
	timer := context.WithValue(t.Context(), periodicPassKey{}, true)

	for range 2 {
		if err := w.RunOnce(timer); err == nil {
			t.Fatal("the failing forward scan must surface")
		}
	}
	logsWhileStalling := fixture.callCount("eth_getLogs")
	if logsWhileStalling == 0 {
		t.Fatal("the first attempts must really try the forward batches")
	}
	if _, paused := w.forwardPause("bsc", time.Now()); !paused {
		t.Fatal("two stalled passes must pause the forward scan")
	}

	// A timer pass now skips the forward batches - no more getLogs - but still
	// reads the head and reports the skipped attempt, so the chain stays visible
	// as failing instead of going quiet.
	headsBefore := fixture.callCount("eth_blockNumber")
	err := w.RunOnce(timer)
	if _, ok := errors.AsType[*chainPausedError](err); !ok {
		t.Fatalf("a skipped forward attempt must be reported, got %v", err)
	}
	if got := fixture.callCount("eth_getLogs"); got != logsWhileStalling {
		t.Fatalf("a paused forward scan must not spend getLogs requests: %d -> %d", logsWhileStalling, got)
	}
	if fixture.callCount("eth_blockNumber") <= headsBefore {
		t.Fatal("the head stage must keep running while the forward scan waits")
	}

	// A manual scan is an operator asking to try now.
	if err := w.RunOnce(t.Context()); err == nil {
		t.Fatal("the manual scan runs and hits the failure")
	}
	if got := fixture.callCount("eth_getLogs"); got <= logsWhileStalling {
		t.Fatalf("manual scans must bypass the pause: getLogs %d -> %d", logsWhileStalling, got)
	}

	// Once the pause lapses and the node answers again, one pass clears everything.
	setFailure("")
	w.replayMu.Lock()
	w.chainPause["bsc"].next = time.Now().Add(-time.Second)
	w.replayMu.Unlock()
	if err := w.RunOnce(timer); err != nil {
		t.Fatalf("healthy pass: %v", err)
	}
	if _, paused := w.forwardPause("bsc", time.Now()); paused {
		t.Fatal("a successful pass must clear the pause")
	}
	w.replayMu.Lock()
	_, tracked := w.chainPause["bsc"]
	w.replayMu.Unlock()
	if tracked {
		t.Fatal("a successful pass must drop the chain's stall record")
	}
}

// The idle-chain guard must not share the deposit scan's lock: a guard that keeps
// failing on a dead chain held the shared lock back to back (96% of the time in
// production) and starved the deposit scan of every chain.
func TestGuardScanUsesItsOwnAdvisoryLock(t *testing.T) {
	if GuardScanLockKey == scanLockKey {
		t.Fatal("the guard scan and the deposit scan must not contend for one lock")
	}
	if GuardScanLockKey != 0x43525950544f5347 { // "CRYPTOSG"; the sweeper's is "CRYPTOSW"
		t.Fatalf("unexpected guard lock key %#x", GuardScanLockKey)
	}
	w := &Worker{}
	if got := w.advisoryLockKey(); got != scanLockKey {
		t.Fatalf("a worker with no explicit key takes the shared deposit-scan lock, got %#x", got)
	}
	w.SetScanLockKey(GuardScanLockKey)
	if got := w.advisoryLockKey(); got != GuardScanLockKey {
		t.Fatalf("SetScanLockKey was ignored: %#x", got)
	}
}

// The configured key is the one that reaches PostgreSQL, for both the acquire and
// the release.
func TestWorkerAcquiresAndReleasesTheConfiguredAdvisoryLock(t *testing.T) {
	pg := &scannerMockPG{key: GuardScanLockKey}
	w, _ := scannerLockFixture(t, pg)
	w.SetScanLockKey(GuardScanLockKey)
	if err := w.RunOnce(t.Context()); err != nil {
		t.Fatalf("the mock rejects any key but the guard's: %v", err)
	}
	scannerAssertSteps(t, pg, "acquire", "load cursor", "save cursor", "pending", "recheck", "release")
	scannerAssertIdle(t, w, pg, false)

	// and a default worker against a mock that wants the guard key is rejected,
	// which is what proves the mock really checks the key
	other := &scannerMockPG{key: GuardScanLockKey}
	w2, _ := scannerLockFixture(t, other)
	if err := w2.RunOnce(t.Context()); err == nil {
		t.Fatal("the shared key must not be accepted where the guard key is expected")
	}
}

// A rate limit is the one scan failure worth repeating inside the same tick:
// the block range is fine, only the request rate was too high. Retrying it keeps
// the cursor moving instead of losing the whole interval.
func TestIsRateLimitedScan(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"http 429", &rpc.Failure{Kind: "remote", HTTPStatus: http.StatusTooManyRequests}, true},
		{"rpc 429", &rpc.Failure{Kind: "remote", RPCCode: 429}, true},
		{"wrapped http 429", fmt.Errorf("stage deposit: %w", &rpc.Failure{Kind: "remote", HTTPStatus: http.StatusTooManyRequests}), true},
		{"joined with 429 second", errors.Join(errors.New("other"), &rpc.Failure{Kind: "remote", RPCCode: 429}), true},
		{"rpc 500", &rpc.Failure{Kind: "remote", HTTPStatus: http.StatusInternalServerError}, false},
		{"rejected params", &rpc.Failure{Kind: "remote", RPCCode: -32602}, false},
		{"plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRateLimitedScan(tc.err); got != tc.want {
				t.Fatalf("isRateLimitedScan(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func (p *scannerMockPG) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.openErr != nil {
		return nil, p.openErr
	}
	p.nextSession++
	return &scannerPGConn{pg: p, session: p.nextSession}, nil
}

func (p *scannerMockPG) Driver() driver.Driver { return scannerPGDriver{pg: p} }

func (d scannerPGDriver) Open(string) (driver.Conn, error) {
	return d.pg.Connect(context.Background())
}

func (c *scannerPGConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected Prepare")
}

func (c *scannerPGConn) Begin() (driver.Tx, error) {
	return nil, errors.New("session lock must not use a transaction")
}

func (c *scannerPGConn) Close() error {
	c.pg.mu.Lock()
	defer c.pg.mu.Unlock()
	c.pg.closed = append(c.pg.closed, c.session)
	if c.pg.holder == c.session {
		c.pg.holder, c.pg.depth = 0, 0
	}
	return nil
}

func (c *scannerPGConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.Contains(query, "pg_try_advisory_lock") || strings.Contains(query, "pg_advisory_unlock") {
		// Keep these literals independent of scanLockKey to catch a wrong key.
		wantKey := int64(0x43525950544f5343)
		if c.pg.key != 0 {
			wantKey = c.pg.key
		}
		if len(args) != 1 || args[0].Value != wantKey {
			return nil, fmt.Errorf("unexpected scanner advisory key: %v", args)
		}
		p := c.pg
		p.mu.Lock()
		defer p.mu.Unlock()
		event := scannerPGEvent{query: query, session: c.session, ctxErr: ctx.Err()}
		var reply *scannerPGReply
		var value bool
		if strings.Contains(query, "pg_try_advisory_lock") {
			event.step = "acquire"
			value = p.holder == 0 || p.holder == c.session
			if value {
				p.holder, p.depth = c.session, p.depth+1
			}
			reply = p.acquire
		} else {
			event.step = "release"
			if deadline, ok := ctx.Deadline(); ok {
				event.cleanupBudget = time.Until(deadline)
			}
			value = p.holder == c.session
			reply = p.release
			if value && reply == nil {
				p.depth--
				if p.depth == 0 {
					p.holder = 0
				}
			}
		}
		p.events = append(p.events, event)
		if reply != nil {
			// An acquire failure is injected AFTER the server takes the lock:
			// pooling this session would leave a real, reentrant orphan lock.
			if reply.err != nil {
				return nil, reply.err
			}
			if reply.empty {
				return &scannerPGRows{columns: []string{"locked"}}, nil
			}
			return scannerOneRow("locked", reply.value), nil
		}
		return scannerOneRow("locked", value), nil
	}
	var step string
	switch {
	case strings.Contains(query, "status = 'pending'"):
		step = "pending"
	case strings.Contains(query, "d.status = 'credited'"):
		step = "recheck"
	case strings.Contains(query, "SELECT last_block"):
		step = "load cursor"
	default:
		return nil, fmt.Errorf("unexpected query: %s", query)
	}
	if err := c.work(ctx, step, query); err != nil {
		return nil, err
	}
	if step == "load cursor" {
		return scannerOneRow("last_block", int64(100)), nil
	}
	return &scannerPGRows{columns: []string{
		"chain", "tx_hash", "log_index", "from_address", "to_address",
		"token", "amount", "block_number", "block_hash", "reason",
	}}, nil
}

func (c *scannerPGConn) ExecContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if !strings.Contains(query, "INSERT INTO crypto_scan_cursors") {
		return nil, fmt.Errorf("unexpected exec: %s", query)
	}
	if err := c.work(ctx, "save cursor", query); err != nil {
		return nil, err
	}
	return driver.RowsAffected(1), nil
}

func (c *scannerPGConn) work(ctx context.Context, step, query string) error {
	p := c.pg
	p.mu.Lock()
	p.events = append(p.events, scannerPGEvent{step: step, query: query, session: c.session, ctxErr: ctx.Err()})
	protected := p.holder != 0 && p.holder != c.session && p.depth == 1
	p.mu.Unlock()
	if !protected {
		return fmt.Errorf("%s ran without a lock on a separate session", step)
	}
	if p.beforeWork != nil {
		if err := p.beforeWork(ctx, step); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func scannerOneRow(column string, value driver.Value) *scannerPGRows {
	return &scannerPGRows{columns: []string{column}, values: [][]driver.Value{{value}}}
}

func (r *scannerPGRows) Columns() []string { return r.columns }
func (r *scannerPGRows) Close() error      { return nil }
func (r *scannerPGRows) Next(dest []driver.Value) error {
	if len(r.values) == 0 {
		return io.EOF
	}
	copy(dest, r.values[0])
	r.values = r.values[1:]
	return nil
}

func (p *scannerMockPG) snapshot() (int, int, []scannerPGEvent, []int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.holder, p.depth, append([]scannerPGEvent(nil), p.events...), append([]int(nil), p.closed...)
}

func scannerLockFixture(t *testing.T, pg *scannerMockPG) (*Worker, *rpcFixture) {
	t.Helper()
	db := sql.OpenDB(pg)
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	tr := testTransfer()
	f := newRPCFixture(tr)
	f.logs = nil
	w := NewWorker(db, map[string]*rpc.Pool{"bsc": f.pool(t)},
		[]ChainConfig{{Chain: "bsc", Tokens: []string{tr.Token}, Confirmations: 5}},
		NewCursorStore(db), &recordingHandler{},
		func(context.Context, string) ([]string, error) { return []string{tr.To}, nil }, 0, 0)
	return w, f
}

func scannerAssertSteps(t *testing.T, pg *scannerMockPG, want ...string) {
	t.Helper()
	_, _, events, _ := pg.snapshot()
	var got []string
	for _, e := range events {
		got = append(got, e.step)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("steps=%v want=%v", got, want)
	}
}

func scannerAssertIdle(t *testing.T, w *Worker, pg *scannerMockPG, discarded bool) {
	t.Helper()
	holder, depth, _, closed := pg.snapshot()
	stats := w.db.Stats()
	if holder != 0 || depth != 0 || stats.InUse != 0 {
		t.Fatalf("leaked session lock: holder=%d depth=%d stats=%+v", holder, depth, stats)
	}
	if discarded && (len(closed) != 1 || closed[0] != 1 || stats.OpenConnections != stats.Idle) {
		t.Fatalf("uncertain lock session was not discarded: closed=%v stats=%+v", closed, stats)
	}
	if !discarded && len(closed) != 0 {
		t.Fatalf("healthy session was discarded: %v", closed)
	}
}

func TestScannerScanAllHoldsDedicatedSessionThroughCursor(t *testing.T) {
	pg := &scannerMockPG{}
	w, f := scannerLockFixture(t, pg)
	w.scanAll(t.Context())
	scannerAssertSteps(t, pg, "acquire", "load cursor", "save cursor", "pending", "recheck", "release")
	scannerAssertIdle(t, w, pg, false)
	_, _, events, _ := pg.snapshot()
	if events[0].session != events[len(events)-1].session || w.db.Stats().OpenConnections != 2 {
		t.Fatalf("lock was not pinned to its own session: events=%+v stats=%+v", events, w.db.Stats())
	}
	if f.callCount("eth_getLogs") != 1 {
		t.Fatal("the protected pass did not scan the chain")
	}
}

func TestDepositNetworkStatusScannerUsesOnlyLockedPeriodicScan(t *testing.T) {
	t.Run("manual and periodic", func(t *testing.T) {
		pg := &scannerMockPG{}
		w, f := scannerLockFixture(t, pg)
		var snapshots []DepositScanObservation
		w.SetDepositScanObserver(func(o DepositScanObservation) {
			holder, depth, events, _ := pg.snapshot()
			if holder <= 0 || depth != 1 || events[len(events)-1].step != "save cursor" {
				t.Fatalf("observation was outside the lock or preceded the commit: %+v", events)
			}
			snapshots = append(snapshots, o)
		})
		if err := w.RunOnce(t.Context()); err != nil || len(snapshots) != 0 {
			t.Fatalf("manual scan published a user status: snapshots=%+v err=%v", snapshots, err)
		}
		w.scanAll(t.Context())
		if len(snapshots) != 1 || !snapshots[0].CanMeasure || snapshots[0].Failed ||
			snapshots[0].CursorBefore != 100 || snapshots[0].Cursor <= 100 || snapshots[0].Cursor != snapshots[0].ConfirmedHead {
			t.Fatalf("periodic observation lacked committed per-chain evidence: %+v", snapshots)
		}
		if f.callCount("eth_getLogs") != 2 {
			t.Fatal("status observation introduced another log scan")
		}
	})
	t.Run("contender", func(t *testing.T) {
		pg := &scannerMockPG{holder: -1, depth: 1}
		w, _ := scannerLockFixture(t, pg)
		w.SetDepositScanObserver(func(DepositScanObservation) { t.Fatal("lock contender published a status") })
		w.scanAll(t.Context())
		scannerAssertSteps(t, pg, "acquire")
	})
}

func TestScannerScanAllSkipsWhenAcquireFalse(t *testing.T) {
	// A different PostgreSQL session already owns the global lock.
	pg := &scannerMockPG{holder: -1, depth: 1}
	w, f := scannerLockFixture(t, pg)
	w.scanAll(t.Context())
	scannerAssertSteps(t, pg, "acquire")
	holder, depth, _, closed := pg.snapshot()
	if holder != -1 || depth != 1 || len(closed) != 0 || w.db.Stats().InUse != 0 || f.callCount("eth_chainId") != 0 {
		t.Fatalf("contender performed work or changed the owner's lock: holder=%d depth=%d closed=%v", holder, depth, closed)
	}
}

func TestScannerScanAllDiscardsUnknownAcquireSession(t *testing.T) {
	for _, tt := range []struct {
		name  string
		reply scannerPGReply
	}{
		{"lost response", scannerPGReply{err: errors.New("lost acquire response")}},
		{"cancelled query", scannerPGReply{err: context.Canceled}},
		{"NULL response", scannerPGReply{value: nil}},
		{"invalid response", scannerPGReply{value: "unknown"}},
		{"missing response", scannerPGReply{empty: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pg := &scannerMockPG{acquire: &tt.reply}
			w, f := scannerLockFixture(t, pg)
			w.scanAll(t.Context())
			scannerAssertSteps(t, pg, "acquire")
			scannerAssertIdle(t, w, pg, true)
			if w.db.Stats().OpenConnections != 0 || f.callCount("eth_chainId") != 0 {
				t.Fatal("failed acquire pooled the session or started scanning")
			}
		})
	}
}

func TestScannerScanAllDiscardsFailedReleaseSession(t *testing.T) {
	for _, tt := range []struct {
		name  string
		reply scannerPGReply
	}{
		{"lost response", scannerPGReply{err: errors.New("lost unlock response")}},
		{"cleanup timeout", scannerPGReply{err: context.DeadlineExceeded}},
		{"not held", scannerPGReply{value: false}},
		{"NULL response", scannerPGReply{value: nil}},
		{"invalid response", scannerPGReply{value: "unknown"}},
		{"missing response", scannerPGReply{empty: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pg := &scannerMockPG{release: &tt.reply}
			w, _ := scannerLockFixture(t, pg)
			w.scanAll(t.Context())
			scannerAssertSteps(t, pg, "acquire", "load cursor", "save cursor", "pending", "recheck", "release")
			scannerAssertIdle(t, w, pg, true)
			if w.db.Stats().OpenConnections != 1 {
				t.Fatal("lock session survived or business session was discarded")
			}
		})
	}
}

func TestScannerScanAllCancellationStillUnlocksWithFiveSecondContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pg := &scannerMockPG{beforeWork: func(_ context.Context, step string) error {
		if step == "pending" {
			cancel()
		}
		return ctx.Err()
	}}
	w, _ := scannerLockFixture(t, pg)
	w.scanAll(ctx)
	scannerAssertSteps(t, pg, "acquire", "load cursor", "save cursor", "pending", "release")
	scannerAssertIdle(t, w, pg, false)
	_, _, events, _ := pg.snapshot()
	release := events[len(events)-1]
	if release.ctxErr != nil || release.cleanupBudget <= 4*time.Second || release.cleanupBudget > 5*time.Second {
		t.Fatalf("unlock must use an uncancelled independent 5s deadline: %+v", release)
	}
}

func TestScannerScanAllRejectsMissingDatabaseAndSingleConnectionPool(t *testing.T) {
	var nilWorker *Worker
	for _, w := range []*Worker{nilWorker, {}} {
		if release, err := w.tryScanLock(t.Context()); err == nil || release != nil {
			t.Fatal("missing database must fail closed")
		}
		w.scanAll(t.Context()) // must neither panic nor run unprotected work
	}
	pg := &scannerMockPG{}
	w, f := scannerLockFixture(t, pg)
	w.db.SetMaxOpenConns(1)
	w.scanAll(t.Context())
	scannerAssertSteps(t, pg)
	if w.db.Stats().OpenConnections != 0 || f.callCount("eth_chainId") != 0 {
		t.Fatal("one-connection pool was used despite deadlock risk")
	}
}

func TestScannerScanAllSkipsConnectionErrorsAndAlreadyCancelledContext(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelled=%v", cancelled), func(t *testing.T) {
			pg := &scannerMockPG{openErr: errors.New("database unavailable")}
			w, f := scannerLockFixture(t, pg)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if cancelled {
				cancel()
			}
			w.scanAll(ctx)
			scannerAssertSteps(t, pg)
			if w.db.Stats().InUse != 0 || f.callCount("eth_chainId") != 0 {
				t.Fatal("connection failure started work or leaked a connection")
			}
		})
	}
}

func TestScannerScanAllMultipleWorkersOnlyOwnerPerformsWork(t *testing.T) {
	for _, blockedStep := range []string{"pending", "recheck", "load cursor", "chain scan", "save cursor"} {
		t.Run(blockedStep, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			entered, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			block := func(ctx context.Context, step string) error {
				if step == blockedStep {
					once.Do(func() {
						close(entered)
						select {
						case <-resume:
						case <-ctx.Done():
						}
					})
				}
				return ctx.Err()
			}
			pg := &scannerMockPG{beforeWork: block}
			first, firstRPC := scannerLockFixture(t, pg)
			second, secondRPC := scannerLockFixture(t, pg) // independent connection pool
			addresses := first.addresses
			first.addresses = func(ctx context.Context, chain string) ([]string, error) {
				if err := block(ctx, "chain scan"); err != nil {
					return nil, err
				}
				return addresses(ctx, chain)
			}
			go func() {
				defer close(done)
				first.scanAll(ctx)
			}()
			t.Cleanup(func() {
				cancel()
				<-done
			})
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("owner did not reach the protected step")
			}
			second.scanAll(ctx)
			holder, depth, events, _ := pg.snapshot()
			if holder == 0 || depth != 1 || events[len(events)-1].step != "acquire" || secondRPC.callCount("eth_chainId") != 0 {
				t.Fatalf("contender performed work: holder=%d depth=%d events=%+v", holder, depth, events)
			}
			close(resume)
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("owner did not release its lock")
			}
			if firstRPC.callCount("eth_getLogs") != 1 || secondRPC.callCount("eth_getLogs") != 0 {
				t.Fatal("multiple workers scanned during one ownership period")
			}
			scannerAssertIdle(t, first, pg, false)
			// After release, another instance must make progress on a new pass.
			second.scanAll(ctx)
			if secondRPC.callCount("eth_getLogs") != 1 {
				t.Fatal("released lock prevented the next instance from scanning")
			}
			scannerAssertIdle(t, second, pg, false)
		})
	}
}

// A hung pass used to hold the scan session lock forever: every later tick got
// ErrScanBusy and no chain advanced until the process was restarted. The pass
// deadline is what guarantees the lock is released.
func TestScanPassDeadlineIsLongerThanOneChainBudget(t *testing.T) {
	if scanPassDeadline <= maxScanTimePerChain {
		t.Fatalf("pass deadline %s must exceed the per-chain budget %s", scanPassDeadline, maxScanTimePerChain)
	}
	// Sanity bound: a pass should not be allowed to run for hours either.
	if scanPassDeadline > time.Hour {
		t.Fatalf("pass deadline %s is unreasonably long", scanPassDeadline)
	}
}

// scanAll must pass down a deadline but keep reacting to the worker's own stop
// signal: a stop has to abort the pass immediately, not wait 15 minutes.
func TestScanAllHonoursParentCancellation(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	ctx, cancel := context.WithTimeout(parent, scanPassDeadline)
	defer cancel()

	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("pass context must carry a deadline")
	}

	cancelParent()
	<-ctx.Done()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("parent cancel should win over the deadline, got %v", ctx.Err())
	}
}

// The raw cause is what finally makes a stuck chain diagnosable, but an RPC key
// sits inside the endpoint URL, so it must never reach the log verbatim.
func TestRawScanCauseRedactsEndpointKeys(t *testing.T) {
	err := errors.New(`Post "https://bsc.blockpi.network/v1/rpc/9d5a92d447e4a11a67b6ffe43e17f7fb4626b4ef": dial tcp: i/o timeout`)

	got := rawScanCause(err)

	if strings.Contains(got, "9d5a92d447e4a11a67b6ffe43e17f7fb4626b4ef") {
		t.Fatalf("RPC key leaked into the log: %s", got)
	}
	if !strings.Contains(got, "/rpc/<redacted>") {
		t.Fatalf("key was not replaced, got %s", got)
	}
	// The part that actually explains the failure must survive.
	if !strings.Contains(got, "i/o timeout") {
		t.Fatalf("cause was stripped, got %s", got)
	}
}

func TestRawScanCauseRedactsQueryKeysAndKeepsMessages(t *testing.T) {
	err := errors.New("explorer call failed: https://api.bscscan.com/v2/api?module=account&apikey=ABCDEF123456&action=txlist: status 403")

	got := rawScanCause(err)

	if strings.Contains(got, "ABCDEF123456") {
		t.Fatalf("query api key leaked: %s", got)
	}
	if !strings.Contains(got, "apikey=<redacted>") || !strings.Contains(got, "status 403") {
		t.Fatalf("unexpected raw cause: %s", got)
	}
}

func TestRawScanCauseCapsLength(t *testing.T) {
	got := rawScanCause(errors.New(strings.Repeat("x", 4000)))
	if len(got) > 700 {
		t.Fatalf("raw cause should be bounded, got %d bytes", len(got))
	}
}

func TestRawScanCauseNilIsEmpty(t *testing.T) {
	if got := rawScanCause(nil); got != "" {
		t.Fatalf("nil error should produce empty cause, got %q", got)
	}
}
