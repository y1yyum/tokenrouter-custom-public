package cryptopay

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"

	"github.com/TokenFlux/TokenRouter/internal/cryptopay/scanner"
	"github.com/TokenFlux/TokenRouter/internal/cryptopay/wallet"
)

const (
	testSecurityInvalidatedAt = "2026-11-20T19:51:46Z"

	// masterKeyForTest is a 32-byte hex master key, the shape wallet.New requires to
	// initialise the security key. Test material only.
	masterKeyForTest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	// settingVaultForTest mirrors the wallet package's unexported vault key.
	settingVaultForTest = "crypto.signer.vault"
)

// expectSecurityPauseOnce queues the claim + conditional disable + commit that
// a single lock episode applies exactly once.
func expectSecurityPauseOnce(mock sqlmock.Sqlmock, episode string) {
	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO settings").
		WithArgs(SettingKeySecurityPauseMarker, episode).
		WillReturnRows(sqlmock.NewRows([]string{"claimed"}).AddRow(1))
	mock.ExpectExec("UPDATE settings SET value=jsonb_set").
		WithArgs(SettingKeyCryptoConfig).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
}

func TestSecurityPauseDoesNotWaitOnReloadAndPreservesOtherConfig(t *testing.T) {
	t.Setenv("CRYPTO_ADMIN_TEST_MODE", "false")
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	svc := &Service{db: db, wallet: wallet.New(db), config: CryptoConfig{Enabled: true, Chains: []string{"base"}, SweepAuto: true}}
	// The scanner can discover an incident while reload holds this mutex
	// waiting for it. Pausing/notifying must never reacquire that mutex.
	expectSecurityPauseOnce(mock, "unauthorized_token_outflow")
	svc.mu.Lock()
	done := make(chan error, 1)
	go func() {
		done <- svc.handleSecurityIncident(context.Background(), wallet.SecurityStatus{Enabled: true, Initialized: true, Locked: true, DepositsPaused: true, Reason: "unauthorized_token_outflow"})
	}()
	select {
	case err := <-done:
		svc.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		svc.mu.Unlock()
		t.Fatal("security pause deadlocked with configuration reload")
	}
	cfg := svc.Config()
	if cfg.Enabled || !cfg.SweepAuto || len(cfg.Chains) != 1 || cfg.Chains[0] != "base" {
		t.Fatal("pause altered unrelated settings", cfg)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFailedPersistentChannelPauseStillStopsLocalActivity(t *testing.T) {
	t.Setenv("CRYPTO_ADMIN_TEST_MODE", "false")
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	svc := &Service{db: db, wallet: wallet.New(db), config: CryptoConfig{Enabled: true}}
	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO settings").WillReturnError(errors.New("database offline"))
	if err := svc.handleSecurityIncident(t.Context(), wallet.SecurityStatus{Locked: true}); err == nil {
		t.Fatal("persistence failure hidden")
	}
	if !svc.securityPaused.Load() || svc.Config().Enabled {
		t.Fatal("write failure reopened local payment gate")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The security monitor observes a persistent lock every tick. The channel has
// to be paused once per incident, not on every observation: otherwise an
// operator's explicit re-enable is silently reverted seconds later.
func TestSecurityPauseIsAppliedOncePerIncident(t *testing.T) {
	t.Setenv("CRYPTO_ADMIN_TEST_MODE", "false")
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	svc := &Service{db: db, wallet: wallet.New(db), config: CryptoConfig{Enabled: true}}
	status := wallet.SecurityStatus{Enabled: true, Initialized: true, Locked: true, DepositsPaused: true, Reason: "deposit_address_changed", LockedAt: testSecurityInvalidatedAt}

	expectSecurityPauseOnce(mock, testSecurityInvalidatedAt)
	require.NoError(t, svc.handleSecurityIncident(t.Context(), status))

	// An operator re-enables the channel; the same incident must not rewrite it.
	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO settings").
		WithArgs(SettingKeySecurityPauseMarker, testSecurityInvalidatedAt).
		WillReturnRows(sqlmock.NewRows([]string{"claimed"}))
	mock.ExpectCommit()
	require.NoError(t, svc.handleSecurityIncident(t.Context(), status), "a repeated observation must not fail the guard")
	require.NoError(t, mock.ExpectationsWereMet(), "re-enable survived the repeated guard tick without another write")

	// A genuinely new incident pauses the channel again.
	next := wallet.SecurityStatus{Enabled: true, Locked: true, Reason: "unauthorized_token_outflow", LockedAt: "2026-11-21T01:02:03Z"}
	expectSecurityPauseOnce(mock, next.LockedAt)
	require.NoError(t, svc.handleSecurityIncident(t.Context(), next))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestSecurityLockEpisodeFallsBackToReason(t *testing.T) {
	if got := securityLockEpisode(wallet.SecurityStatus{Locked: true, LockedAt: " 2026-11-20T19:51:46Z "}); got != "2026-11-20T19:51:46Z" {
		t.Fatalf("episode = %q, want the trimmed lock time", got)
	}
	if got := securityLockEpisode(wallet.SecurityStatus{Locked: true, Reason: "vault_unavailable"}); got != "vault_unavailable" {
		t.Fatalf("episode = %q, want the reason when LockedAt is empty", got)
	}
	if got := securityLockEpisode(wallet.SecurityStatus{Locked: true}); got != "locked" {
		t.Fatalf("episode = %q, want the stable fallback", got)
	}
}

func within(got, want time.Duration) bool { return got >= want-want/5 && got <= want+want/5 }

func stateOf(p *idleGuardPacer, chain string) idleGuardState {
	if st := p.chains[chain]; st != nil {
		return *st
	}
	return idleGuardState{}
}

func TestIdleGuardPacerBacksOffExponentiallyAndCaps(t *testing.T) {
	var p idleGuardPacer
	now := time.Date(2026, 11, 24, 4, 0, 0, 0, time.UTC)
	if !p.due("plasma", now) {
		t.Fatal("a fresh pacer runs at once")
	}
	failure := errors.New("every node refused us")
	for i, want := range []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		p.note("plasma", failure, now)
		st := stateOf(&p, "plasma")
		if got := st.next.Sub(now); !within(got, want) {
			t.Fatalf("failure %d: next attempt in %v, want about %v", i+1, got, want)
		}
		if p.due("plasma", now) {
			t.Fatalf("failure %d: must not be due immediately", i+1)
		}
		if !p.due("plasma", st.next) {
			t.Fatalf("failure %d: must be due once the delay elapsed", i+1)
		}
	}
}

// The jitter is random, so a cap that is off by a doubling (320s instead of 300s) only
// shows on some draws. Draw many times, including absurd failure counts: the delay
// must stay inside five minutes +-20% and must never overflow into a negative or
// zero wait (which would make the guard hammer a dead provider again).
func TestIdleGuardPacerDelayNeverExceedsTheCap(t *testing.T) {
	now := time.Date(2026, 11, 24, 4, 0, 0, 0, time.UTC)
	for _, failures := range []int{7, 8, 30, 100000} {
		for draw := 0; draw < 300; draw++ {
			var p idleGuardPacer
			for i := 1; i < failures && i < 12; i++ { // walk up to the cap cheaply
				p.note("plasma", errors.New("down"), now)
			}
			if failures > 12 {
				p.chains["plasma"].failures = failures - 1 // as if it had failed that many times
			}
			p.note("plasma", errors.New("down"), now)
			got := p.chains["plasma"].next.Sub(now)
			if got < idleGuardMaxDelay-idleGuardMaxDelay/5 || got > idleGuardMaxDelay+idleGuardMaxDelay/5 {
				t.Fatalf("failures=%d draw=%d: delay %v is outside 5min +-20%%", failures, draw, got)
			}
		}
	}
}

func TestIdleGuardPacerFirstSuccessResumesFullCadence(t *testing.T) {
	var p idleGuardPacer
	now := time.Now()
	for i := 0; i < 8; i++ {
		p.note("plasma", errors.New("down"), now)
	}
	if p.due("plasma", now) {
		t.Fatal("setup: backed off")
	}
	p.note("plasma", nil, now.Add(time.Hour))
	if !p.due("plasma", now) || p.failuresOf("plasma") != 0 {
		t.Fatalf("one success must clear the backoff, got %+v", stateOf(&p, "plasma"))
	}
	p.note("plasma", errors.New("down again"), now)
	if got := stateOf(&p, "plasma").next.Sub(now); !within(got, 5*time.Second) {
		t.Fatalf("restarted at %v, want about 5s", got)
	}
}

// Another instance holding the guard lock says nothing about the chain.
func TestIdleGuardPacerIgnoresAnotherInstanceHoldingTheLock(t *testing.T) {
	var p idleGuardPacer
	now := time.Now()
	p.note("plasma", scanner.ErrScanBusy, now)
	p.note("plasma", fmt.Errorf("wrapped: %w", scanner.ErrScanBusy), now)
	p.note("plasma", errors.Join(scanner.ErrScanBusy, scanner.ErrScanBusy), now)
	if p.failuresOf("plasma") != 0 || !p.due("plasma", now) {
		t.Fatalf("a busy lock must not count as a failure: %+v", stateOf(&p, "plasma"))
	}
	p.note("plasma", errors.New("down"), now)
	before := stateOf(&p, "plasma")
	p.note("plasma", scanner.ErrScanBusy, now.Add(time.Second))
	if after := stateOf(&p, "plasma"); after != before {
		t.Fatalf("a busy lock must leave a running backoff alone: %+v -> %+v", before, after)
	}
}

// The point of the design: a dead chain must not delay the others. A guard exists
// to notice an unauthorised outflow from a hot wallet; one chain's outage cannot
// push another chain's detection out to five minutes.
func TestIdleGuardPacerIsPerChain(t *testing.T) {
	var p idleGuardPacer
	now := time.Date(2026, 11, 24, 4, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		p.note("plasma", errors.New("403 from every node"), now)
	}
	if p.due("plasma", now) {
		t.Fatal("setup: plasma is backed off")
	}
	for _, healthy := range []string{"arbitrum", "base", "xlayer"} {
		if !p.due(healthy, now) {
			t.Fatalf("%s has never failed and must stay due while plasma is backed off", healthy)
		}
		p.note(healthy, nil, now)
	}
	if p.due("plasma", now) || p.failuresOf("plasma") != 10 {
		t.Fatalf("other chains' successes must not touch plasma's backoff: %+v", stateOf(&p, "plasma"))
	}
	// and a failure of one healthy chain starts ITS backoff from the short delay
	p.note("base", errors.New("blip"), now)
	if got := stateOf(&p, "base").next.Sub(now); !within(got, 5*time.Second) {
		t.Fatalf("base failed once: next in %v, want about 5s", got)
	}
	if !p.due("arbitrum", now) || !p.due("xlayer", now) {
		t.Fatal("base's failure must not back off its neighbours")
	}
}

func TestIdleGuardPacerDueChainsSkipsBackedOffAndOrdersHealthiestFirst(t *testing.T) {
	var p idleGuardPacer
	now := time.Date(2026, 11, 24, 4, 0, 0, 0, time.UTC)
	chains := []scanner.ChainConfig{{Chain: "plasma"}, {Chain: "base"}, {Chain: "xlayer"}, {Chain: "arbitrum"}}
	// plasma: one failure, backed off until later; base: failed twice but its delay has passed;
	// xlayer, arbitrum: never failed.
	p.note("plasma", errors.New("down"), now)
	p.note("base", errors.New("down"), now.Add(-time.Hour))
	p.note("base", errors.New("down"), now.Add(-time.Hour))
	got := p.dueChains(chains, now)
	var names []string
	for _, c := range got {
		names = append(names, c.Chain)
	}
	want := []string{"xlayer", "arbitrum", "base"} // plasma skipped; the chain with failures runs last; ties keep input order
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("dueChains = %v, want %v", names, want)
	}
	if len(p.dueChains(nil, now)) != 0 {
		t.Fatal("no chains, nothing due")
	}
}

// The wiring the incident was about: the idle guard's worker takes the guard's own
// lock, not the deposit scan's. Without this the guard (failing on a dead chain
// every tick) held the shared lock 96% of the time and starved every deposit scan.
func TestIdleGuardWorkerTakesItsOwnAdvisoryLock(t *testing.T) {
	worker := newIdleGuardWorker(nil, nil, nil)
	if got := worker.LockKey(); got != scanner.GuardScanLockKey {
		t.Fatalf("the idle guard must use scanner.GuardScanLockKey, got %#x", got)
	}
	deposit := scanner.NewWorker(nil, nil, nil, nil, nil, nil, 5*time.Second, 2000)
	if deposit.LockKey() == worker.LockKey() {
		t.Fatal("the deposit scan and the idle guard must hold different locks")
	}
}

func guardChains(names ...string) []scanner.ChainConfig {
	out := make([]scanner.ChainConfig, 0, len(names))
	for _, name := range names {
		out = append(out, scanner.ChainConfig{Chain: name})
	}
	return out
}

// One tick, several idle chains: a chain that hangs is cut off by ITS OWN deadline
// and backed off, and the chains behind it still run in the same tick with a live
// context. Without the per-chain deadline the hung chain would use the whole tick
// and everything behind it would run on an expired context.
func TestIdleGuardTickGivesEveryChainItsOwnBudget(t *testing.T) {
	old := idleGuardChainBudget
	idleGuardChainBudget = 60 * time.Millisecond
	t.Cleanup(func() { idleGuardChainBudget = old })

	var pacer idleGuardPacer
	ran := map[string]bool{}
	tick, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errs := runIdleGuardChains(tick, &pacer, guardChains("plasma", "base", "xlayer"), func(ctx context.Context, chain scanner.ChainConfig) error {
		ran[chain.Chain] = true
		if chain.Chain == "plasma" {
			<-ctx.Done() // a node that never answers
			return ctx.Err()
		}
		return nil
	})
	if !ran["plasma"] || !ran["base"] || !ran["xlayer"] {
		t.Fatalf("every due chain must run in the tick: %v", ran)
	}
	if len(errs) != 1 {
		t.Fatalf("only the hung chain failed, got %v", errs)
	}
	if pacer.failuresOf("plasma") != 1 || pacer.failuresOf("base") != 0 || pacer.failuresOf("xlayer") != 0 {
		t.Fatalf("the hung chain is backed off by its own deadline, the others are clean: plasma=%d base=%d xlayer=%d",
			pacer.failuresOf("plasma"), pacer.failuresOf("base"), pacer.failuresOf("xlayer"))
	}
}

// When the tick itself has run out, what a chain returned is not its fault: it must
// not be backed off for it, and the chains not reached are left for the next tick.
func TestIdleGuardExpiredTickDoesNotBlameTheChain(t *testing.T) {
	var pacer idleGuardPacer
	tick, cancel := context.WithCancel(context.Background())
	ran := map[string]int{}
	errs := runIdleGuardChains(tick, &pacer, guardChains("base", "xlayer", "arbitrum"), func(ctx context.Context, chain scanner.ChainConfig) error {
		ran[chain.Chain]++
		cancel() // the tick's budget ends while the first chain is working
		return ctx.Err()
	})
	if ran["base"] != 1 || ran["xlayer"] != 0 || ran["arbitrum"] != 0 {
		t.Fatalf("after the tick expired no further chain may start: %v", ran)
	}
	if pacer.failuresOf("base") != 0 {
		t.Fatalf("an expired tick must not count as the chain's failure, got %d", pacer.failuresOf("base"))
	}
	if len(errs) != 1 {
		t.Fatalf("the error is still reported for the log, got %v", errs)
	}
}

// A v3 signer vault sealed behind recovery shards is a supported steady state
// and must not fail the deposit scan.
//
// Production symptom this guards: BSC sat at cursor 135294224 for hours with
// "扫描速度 0 块/分" while the chain grew ~133 blocks/min. Every pass failed in
// its security stage on the first range after the cursor, so the cursor could
// never advance and the chain fell permanently behind.
func TestScanSecurityGuardIgnoresSealedVault(t *testing.T) {
	// Before the fix the guard returned this error verbatim, so the scanner
	// failed the block range and never saved the cursor.
	require.NoError(t, forgiveSealedVault(wallet.ErrSignerShareRequired))
	require.NoError(t, forgiveSealedVault(
		errors.Join(errors.New("security stage"), wallet.ErrSignerShareRequired)))
}

// The guard forgives exactly one condition. A real security incident must keep
// stopping the scan, otherwise this fix would silently disable the outflow
// guard's blocking behaviour.
func TestScanSecurityGuardOnlyForgivesSealedVault(t *testing.T) {
	require.True(t, isSealedVaultSteadyState(wallet.ErrSignerShareRequired))
	require.True(t, isSealedVaultSteadyState(errors.Join(errors.New("wrap"), wallet.ErrSignerShareRequired)))

	for _, err := range []error{
		nil,
		wallet.ErrSecurityLocked,
		wallet.ErrVaultUnavailable,
		wallet.ErrWalletNotConfigured,
		wallet.ErrSigningDisabled,
		errors.New("vault_unavailable"),
	} {
		require.False(t, isSealedVaultSteadyState(err),
			"expected %v to keep blocking the scan", err)
	}
}

// A sealed vault must not abort the scan when an outflow needs the signer audit
// table. AuthorizedTokenTransfer surfaces ErrSignerShareRequired whenever the
// vault is sealed, so one outflow inside the scanned range used to fail the
// whole block range and freeze the cursor behind it.
func TestAuthorizedTokenTransferSealedVaultIsNotAScanFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mgr := wallet.New(db, masterKeyForTest)
	mgr.EnableSecurityMonitoring()

	// currentVaultTx reads the vault row, cannot derive (no data key), and
	// returns ErrSignerShareRequired before any security-state read.
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT value FROM settings").
		WithArgs(settingVaultForTest).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("sub2api-wallet:v3:deadbeef"))
	mock.ExpectRollback()

	_, err = mgr.AuthorizedTokenTransfer(context.Background(), "bsc", 56, "0xdead",
		"0xfrom", "0xto", "0xtoken", "1000")
	require.True(t, isSealedVaultSteadyState(err),
		"an outflow lookup against a sealed vault must be the forgivable steady state, got %v", err)
}
