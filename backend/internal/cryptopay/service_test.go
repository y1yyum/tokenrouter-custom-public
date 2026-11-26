package cryptopay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"

	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/TokenFlux/TokenRouter/internal/cryptopay/addresses"
	"github.com/TokenFlux/TokenRouter/internal/cryptopay/nativeprice"
	"github.com/TokenFlux/TokenRouter/internal/cryptopay/scanner"
	"github.com/TokenFlux/TokenRouter/internal/cryptopay/sweeper"
	"github.com/TokenFlux/TokenRouter/internal/cryptopay/wallet"
)

func depositInfoConfig() CryptoConfig {
	cfg := DefaultCryptoConfig()
	cfg.Enabled = true
	cfg.Chains = []string{ChainEthereum, ChainBSC}
	cfg.CreditMultiplier = 6.7
	cfg.CreditMinAmount = 0.001
	cfg.CreditMaxAmount = 10000
	return cfg
}

func depositInfoAssigned() map[string]string {
	return map[string]string{
		addresses.FamilyEVM: "0x1111111111111111111111111111111111111111",
	}
}

func depositInfoAsset(t *testing.T, info *DepositInfo, chain, symbol string) DepositAsset {
	t.Helper()
	for _, c := range info.Chains {
		if c.Chain != chain {
			continue
		}
		for _, a := range c.Assets {
			if a.Symbol == symbol {
				return a
			}
		}
	}
	t.Fatalf("asset %s on %s not found in %+v", symbol, chain, info.Chains)
	return DepositAsset{}
}

func TestDisabledDepositInfoSerializesEmptyChains(t *testing.T) {
	s := &Service{}
	info, err := s.DepositInfo(t.Context(), 42)
	require.NoError(t, err)
	require.False(t, info.Enabled)
	raw, err := json.Marshal(info)
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &fields))
	require.Equal(t, "[]", string(fields["chains"]))
}

// The deposit page must be able to tell "this asset is settled at the live
// market price" from "this asset has a fixed rate". A native coin carrying the
// stablecoin multiplier is exactly the bug this guards.
func TestDepositInfoMarksNativeCoinAsMarketPricedWithoutRate(t *testing.T) {
	info := buildDepositInfo(depositInfoConfig(), depositInfoAssigned())
	require.True(t, info.Enabled)

	for _, chain := range []string{ChainEthereum, ChainBSC} {
		var native DepositAsset
		found := false
		for _, c := range info.Chains {
			if c.Chain != chain {
				continue
			}
			for _, a := range c.Assets {
				if a.Native {
					native, found = a, true
				}
			}
		}
		require.True(t, found, "%s must expose its native coin", chain)
		require.True(t, native.MarketPriced, "%s native coin must be market priced", chain)
		require.Nil(t, native.Rate, "%s native coin must not expose a fixed rate", chain)
	}

	ether := depositInfoAsset(t, info, ChainEthereum, "ETH")
	require.True(t, ether.MarketPriced)
	require.Nil(t, ether.Rate)
}

// Stablecoins are unchanged: the configured multiplier still applies to a
// token amount that is already a USD amount.
func TestDepositInfoKeepsStablecoinFixedRate(t *testing.T) {
	info := buildDepositInfo(depositInfoConfig(), depositInfoAssigned())
	for _, symbol := range []string{"USDT", "USDC"} {
		asset := depositInfoAsset(t, info, ChainEthereum, symbol)
		require.False(t, asset.MarketPriced, "%s must stay fixed-rate", symbol)
		require.NotNil(t, asset.Rate, "%s must keep its rate", symbol)
		require.Equal(t, 6.7, *asset.Rate)
		require.Nil(t, asset.ReferencePriceUSD)
	}
	// Thresholds keep their existing values for stablecoins (token units).
	require.Equal(t, 0.001, info.MinAmount)
	require.Equal(t, 10000.0, info.MaxAmount)
}

// A client that reads JSON must see no rate at all for a native coin: the
// response-wide rate is gone, and the asset-level one is null.
func TestDepositInfoJSONHasNoRateForNativeCoin(t *testing.T) {
	info := buildDepositInfo(depositInfoConfig(), depositInfoAssigned())
	raw, err := json.Marshal(info)
	require.NoError(t, err)

	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &top))
	_, hasTopLevelRate := top["rate"]
	require.False(t, hasTopLevelRate, "deposit-info must not expose an asset-independent rate: %s", raw)

	var decoded struct {
		Chains []struct {
			Chain  string `json:"chain"`
			Assets []struct {
				Symbol       string   `json:"symbol"`
				Native       bool     `json:"native"`
				MarketPriced bool     `json:"market_priced"`
				Rate         *float64 `json:"rate"`
			} `json:"assets"`
		} `json:"chains"`
	}
	require.NoError(t, json.Unmarshal(raw, &decoded))
	seenNative, seenStable := false, false
	for _, chain := range decoded.Chains {
		for _, asset := range chain.Assets {
			if asset.Native {
				seenNative = true
				require.True(t, asset.MarketPriced, "%s", asset.Symbol)
				require.Nil(t, asset.Rate, "%s must serialize rate as null", asset.Symbol)
				continue
			}
			seenStable = true
			require.False(t, asset.MarketPriced, "%s", asset.Symbol)
			require.NotNil(t, asset.Rate, "%s must serialize its rate", asset.Symbol)
		}
	}
	require.True(t, seenNative && seenStable)
}

// TestNativePriceReferenceUsesOneSnapshotDuringReload changes feeds while a quote
// is in flight. Every native asset in the same response must use the original feed.
func TestNativePriceReferenceUsesOneSnapshotDuringReload(t *testing.T) {
	t.Setenv("CRYPTO_INSTANCE_ENABLED", "false")
	t.Setenv("CRYPTO_ADMIN_TEST_MODE", "")
	t.Setenv("SUB2API_DISABLE_BACKGROUND_JOBS", "true")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	oldFeed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(entered) })
		<-release
		if _, err := fmt.Fprint(w, `{"price":"100"}`); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(oldFeed.Close)
	newFeed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := fmt.Fprint(w, `{"price":"200"}`); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(newFeed.Close)
	repo := &reloadConfigRepository{}
	cfg := depositInfoConfig()
	cfg.NativePriceURL = oldFeed.URL
	repo.set(t, cfg)
	s := &Service{settings: newConfigTestStore(repo), wallet: wallet.New(nil)}
	t.Cleanup(s.Close)
	s.Reload(t.Context())
	info := buildDepositInfo(cfg, depositInfoAssigned())
	done := make(chan struct{})
	go func() {
		s.attachNativeReferencePrices(t.Context(), info)
		close(done)
	}()
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() { unblock(); <-done })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("reference price request never reached local feed")
	}
	cfg.NativePriceURL = newFeed.URL
	repo.set(t, cfg)
	s.Reload(t.Context())
	// The reload must not wait for a network quote holding the read lock.
	unblock()
	<-done
	for _, chain := range []string{ChainEthereum, ChainBSC} {
		symbol := "ETH"
		if chain == ChainBSC {
			symbol = "BNB"
		}
		price := depositInfoAsset(t, info, chain, symbol).ReferencePriceUSD
		require.NotNil(t, price)
		require.Equal(t, "100", *price, "one page response must keep the same provider snapshot")
	}
}

// TestNativePriceReferenceConcurrentReload overlaps repeated quote reads with
// configuration reloads. The concurrent callers exercise the shared provider
// snapshot so race detection can catch unsynchronized access.
func TestNativePriceReferenceConcurrentReload(t *testing.T) {
	t.Setenv("CRYPTO_INSTANCE_ENABLED", "false")
	t.Setenv("CRYPTO_ADMIN_TEST_MODE", "")
	t.Setenv("SUB2API_DISABLE_BACKGROUND_JOBS", "true")
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := fmt.Fprint(w, `{"price":"100"}`); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(feed.Close)
	repo := &reloadConfigRepository{}
	cfg := depositInfoConfig()
	cfg.NativePriceURL = feed.URL
	repo.set(t, cfg)
	s := &Service{settings: newConfigTestStore(repo), wallet: wallet.New(nil)}
	t.Cleanup(s.Close)
	s.Reload(t.Context())
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 4 {
		wg.Go(func() {
			<-start
			for range 100 {
				info := buildDepositInfo(cfg, depositInfoAssigned())
				s.attachNativeReferencePrices(t.Context(), info)
			}
		})
	}
	close(start)
	for range 100 {
		s.Reload(t.Context())
	}
	wg.Wait()
}

func TestAttachNativeReferencePricesAddsDisplayQuote(t *testing.T) {
	prices := map[string]string{"ETHUSDT": "3210.12000000", "BNBUSDT": "640.50000000"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		symbol := r.URL.Query().Get("symbol")
		price, ok := prices[symbol]
		if !ok {
			t.Errorf("unexpected ticker symbol %q", symbol)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"symbol":%q,"price":%q}`, symbol, price)
	}))
	defer server.Close()

	s := &Service{nativePrice: nativeprice.New(server.URL, nativeprice.WithBackoff(time.Millisecond))}
	info := buildDepositInfo(depositInfoConfig(), depositInfoAssigned())
	s.attachNativeReferencePrices(t.Context(), info)

	ether := depositInfoAsset(t, info, ChainEthereum, "ETH")
	require.NotNil(t, ether.ReferencePriceUSD)
	require.Equal(t, "3210.12", *ether.ReferencePriceUSD)
	// Every market-priced asset in the response gets its own chain's quote.
	require.Equal(t, "640.5", *depositInfoAsset(t, info, ChainBSC, "BNB").ReferencePriceUSD)
	// The display quote never becomes a credit rate: ETH stays market priced.
	require.True(t, ether.MarketPriced)
	require.Nil(t, ether.Rate)
	// Stablecoins are not quoted: they do not need a market reference.
	require.Nil(t, depositInfoAsset(t, info, ChainEthereum, "USDT").ReferencePriceUSD)
}

// A failing feed must not fail the page, must not leak a bogus price and must
// not turn the deposit page into a retry storm.
func TestAttachNativeReferencePricesIsSilentWhenFeedFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	s := &Service{nativePrice: nativeprice.New(server.URL, nativeprice.WithBackoff(time.Millisecond))}
	info := buildDepositInfo(depositInfoConfig(), depositInfoAssigned())
	started := time.Now()
	s.attachNativeReferencePrices(t.Context(), info)
	elapsed := time.Since(started)

	require.Less(t, elapsed, 3*time.Second)
	for _, chain := range info.Chains {
		for _, asset := range chain.Assets {
			require.Nil(t, asset.ReferencePriceUSD, "%s must have no quote after a feed outage", asset.Symbol)
			if asset.Native {
				require.True(t, asset.MarketPriced)
				require.Nil(t, asset.Rate)
			}
		}
	}
}

// The time budget is a hard ceiling: a hung ticker delays the page by at most
// nativeReferencePriceTimeout and then the page renders without a quote.
func TestAttachNativeReferencePricesRespectsTheTimeBudget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	s := &Service{nativePrice: nativeprice.New(server.URL, nativeprice.WithBackoff(time.Millisecond))}
	info := buildDepositInfo(depositInfoConfig(), depositInfoAssigned())
	started := time.Now()
	s.attachNativeReferencePrices(t.Context(), info)
	elapsed := time.Since(started)

	require.GreaterOrEqual(t, elapsed, nativeReferencePriceTimeout)
	require.Less(t, elapsed, nativeReferencePriceTimeout+2*time.Second)
	require.Nil(t, depositInfoAsset(t, info, ChainEthereum, "ETH").ReferencePriceUSD)
}

// No configured feed at all is the default for a deployment that never set
// one: the page still renders, assets still declare their pricing mode.
func TestAttachNativeReferencePricesWithoutFeed(t *testing.T) {
	s := &Service{}
	info := buildDepositInfo(depositInfoConfig(), depositInfoAssigned())
	s.attachNativeReferencePrices(t.Context(), info)
	require.Nil(t, depositInfoAsset(t, info, ChainEthereum, "ETH").ReferencePriceUSD)
	require.True(t, depositInfoAsset(t, info, ChainEthereum, "ETH").MarketPriced)
}

func TestUserDepositHistoryReturnsFullTransactionHashForCaller(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	hash := "0x" + strings.Repeat("ab", 32)
	mock.ExpectQuery(`(?s)SELECT d.chain, d.tx_hash,.*WHERE a.user_id = \$1.*ORDER BY d.id DESC LIMIT \$2`).
		WithArgs(int64(42), 50).
		WillReturnRows(sqlmock.NewRows([]string{"chain", "tx_hash", "token", "amount", "status", "reason", "created_at", "credited", "log_index", "order_id", "block_number", "confirmations", "provisional", "credited_at", "revoked_at", "revoke_reason"}).
			AddRow("bsc", hash, "0xtoken", "10", "credited", "", time.Now(), 67, 2, 10, 101, 0, false, nil, nil, ""))
	s := &Service{db: db}
	rows, err := s.UserDepositHistory(t.Context(), 42, 50)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, hash, rows[0].TxHash)
	require.NotNil(t, rows[0].CreditedKnown)
	require.True(t, *rows[0].CreditedKnown)
	encoded, err := json.Marshal(rows)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"tx_hash":"`+hash+`"`)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDepositHistoryKeepsActualOrderAmountAfterRateChange(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mock.ExpectQuery(`(?s)COALESCE\(o.amount, 0\).*LEFT JOIN payment_orders`).
		WithArgs(int64(1), 50).
		WillReturnRows(sqlmock.NewRows([]string{"chain", "tx_hash", "token", "amount", "status", "reason", "created_at", "credited", "log_index", "order_id", "block_number", "confirmations", "provisional", "credited_at", "revoked_at", "revoke_reason"}).
			AddRow("bsc", "0xfixture", "0xtoken", "10.000000000000000000", "credited", "", time.Now(), 67, 2, 10, 101, 0, false, nil, nil, ""))
	s := &Service{db: db, config: CryptoConfig{CreditMultiplier: 6}}
	rows, err := s.UserDepositHistory(t.Context(), 1, 50)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, 67.0, rows[0].Credited)
	require.NotNil(t, rows[0].CreditedKnown)
	require.True(t, *rows[0].CreditedKnown)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestLegacyHistoryResolvesOriginalOrderInOneBatch(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mock.ExpectQuery(`(?s)COALESCE\(o.amount, 0\).*LEFT JOIN payment_orders`).WithArgs(int64(1), 50).
		WillReturnRows(sqlmock.NewRows([]string{"chain", "tx_hash", "token", "amount", "status", "reason", "created_at", "credited", "log_index", "order_id", "block_number", "confirmations", "provisional", "credited_at", "revoked_at", "revoke_reason"}).
			AddRow("bsc", "0xlegacy", "0xtoken", "10", "credited", "", time.Now(), 0, 0, nil, 101, 0, false, nil, nil, ""))
	_, identity := depositOrderIdentity("bsc", "0xlegacy", 0)
	mock.ExpectQuery(`SELECT out_trade_no, amount FROM payment_orders`).WithArgs(int64(1), identity).
		WillReturnRows(sqlmock.NewRows([]string{"out_trade_no", "amount"}).AddRow(identity, 67))
	s := &Service{db: db, config: CryptoConfig{CreditMultiplier: 99}}
	rows, err := s.UserDepositHistory(t.Context(), 1, 50)
	require.NoError(t, err)
	require.Equal(t, 67.0, rows[0].Credited)
	require.NotNil(t, rows[0].CreditedKnown)
	require.True(t, *rows[0].CreditedKnown)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestListDepositsExportPageUsesPrimaryKeyCursorWithoutSummaryScan(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	created := time.Date(2026, 10, 21, 8, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	mock.ExpectQuery(`(?s)SELECT id, chain, tx_hash, log_index.*WHERE id < \$1 AND chain = \$2 AND status = \$3 ORDER BY id DESC LIMIT \$4`).
		WithArgs(int64(150), "bsc", "credited", 100).
		WillReturnRows(sqlmock.NewRows([]string{"id", "chain", "tx_hash", "log_index", "from_address", "to_address", "token", "amount", "status", "reason", "created_at", "native_price_usd", "native_price_at", "native_price_source", "provisional", "provisional_amount", "credited_at", "revoked_at", "revoke_reason"}).
			AddRow(149, "bsc", "0xhash", 3, "from", "to", "token", "2.500000000000000000", "credited", "", created, "", nil, "", false, 0, nil, nil, ""))

	s := &Service{db: db}
	rows, err := s.ListDepositsExportPage(t.Context(), "bsc", "credited", 150, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, int64(149), rows[0].ID)
	require.Equal(t, "2.5", rows[0].Amount)
	require.Equal(t, "2026-10-21T00:00:00Z", rows[0].CreatedAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestListDepositsExportPageStartsWithNewestRecord(t *testing.T) {
	for _, filtered := range []bool{false, true} {
		t.Run(map[bool]string{false: "all", true: "filtered"}[filtered], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			defer func() { _ = db.Close() }()
			query := `(?s)SELECT id, chain, tx_hash, log_index.*WHERE TRUE ORDER BY id DESC LIMIT \$1`
			if filtered {
				query = `(?s)SELECT id, chain, tx_hash, log_index.*WHERE TRUE AND chain = \$1 AND status = \$2 ORDER BY id DESC LIMIT \$3`
			}
			expected := mock.ExpectQuery(query)
			chain, status := "", ""
			if filtered {
				chain, status = "bsc", "credited"
				expected.WithArgs(chain, status, 200)
			} else {
				expected.WithArgs(200)
			}
			expected.WillReturnRows(sqlmock.NewRows([]string{"id", "chain", "tx_hash", "log_index", "from_address", "to_address", "token", "amount", "status", "reason", "created_at", "native_price_usd", "native_price_at", "native_price_source", "provisional", "provisional_amount", "credited_at", "revoked_at", "revoke_reason"}).
				AddRow(250, "bsc", "0xhash", 0, "from", "to", "native", "1.000", "credited", "", time.Now(), "2000.500000000000000000", time.Now(), "stub", false, 0, nil, nil, ""))
			s := &Service{db: db}
			rows, err := s.ListDepositsExportPage(t.Context(), chain, status, 0, 200)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			require.Equal(t, int64(250), rows[0].ID)
			require.Equal(t, "2000.5", rows[0].NativePriceUSD)
			require.NotEmpty(t, rows[0].NativePriceAt)
			require.Equal(t, "stub", rows[0].NativePriceSource)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

// Instant credits reach the user's own history as "provisional" until final
// confirmation, and as "revoked" once the chain invalidated them. The UI keys
// its badges off this status instead of re-deriving it from ledger columns.
func TestUserDepositHistoryDerivesProvisionalAndRevokedStatus(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	created := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	creditedAt := created.Add(2 * time.Second)
	revokedAt := created.Add(30 * time.Second)
	mock.ExpectQuery(`(?s)SELECT d.chain, d.tx_hash,.*provisional.*WHERE a.user_id = \$1.*ORDER BY d.id DESC LIMIT \$2`).
		WithArgs(int64(7), 50).
		WillReturnRows(sqlmock.NewRows([]string{"chain", "tx_hash", "token", "amount", "status", "reason", "created_at", "credited", "log_index", "order_id", "block_number", "confirmations", "provisional", "credited_at", "revoked_at", "revoke_reason"}).
			AddRow("bsc", "0xprov", "0xtoken", "10", "credited", "", created, 67, 2, 10, 101, 0, true, creditedAt, nil, "").
			AddRow("bsc", "0xrev", "0xtoken", "10", "reverted", "", created, 0, 3, nil, 101, 0, true, creditedAt, revokedAt, "reorg_noncanonical_block"))

	s := &Service{db: db}
	rows, err := s.UserDepositHistory(t.Context(), 7, 50)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	require.Equal(t, userDepositStatusProvisional, rows[0].Status)
	require.True(t, rows[0].Provisional)
	require.Equal(t, creditedAt.Format(time.RFC3339), rows[0].CreditedAt)
	require.Empty(t, rows[0].RevokedAt)
	require.Equal(t, 67.0, rows[0].Credited)

	require.Equal(t, userDepositStatusRevoked, rows[1].Status)
	require.True(t, rows[1].Provisional)
	require.Equal(t, "reorg_noncanonical_block", rows[1].RevokeReason)
	require.Equal(t, revokedAt.Format(time.RFC3339), rows[1].RevokedAt)
	require.Zero(t, rows[1].Credited, "a revoked credit must not show as credited")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestDepositNetworksHaveVerifiedTokenAndAddressDefinitions(t *testing.T) {
	for _, chain := range []string{ChainEthereum, ChainBSC, ChainPolygon, ChainPlasma, ChainBase, ChainXLayer} {
		family, err := addresses.FamilyForChain(chain)
		if err != nil {
			t.Fatal(err)
		}
		if len(watchedTokens[chain]) == 0 {
			t.Fatalf("%s has no allowed assets", chain)
		}
		for _, token := range watchedTokens[chain] {
			if token.Symbol == "" {
				t.Fatalf("asset without a symbol: %+v", token)
			}
			if family != addresses.FamilyEVM || sweeper.ChainIDs[chain] <= 0 || chainlistIDs[chain] <= 0 {
				t.Fatalf("%s has inconsistent scanner/probe/signing settings", chain)
			}
			if token.Native {
				// A mainnet coin has no contract, so it must never be mistaken
				// for an ERC-20: the sentinel, not a hex address, identifies it.
				if token.Contract != scanner.NativeToken {
					t.Fatalf("%s native asset must use the native sentinel: %+v", chain, token)
				}
				if token.Decimals != 18 {
					t.Fatalf("%s native asset must have 18 decimals: %+v", chain, token)
				}
				if common.IsHexAddress(token.Contract) {
					t.Fatalf("%s native asset must not carry a contract address", chain)
				}
				continue
			}
			// BSC's BEP-20 stablecoins are 18-decimal; every other watched
			// token uses 6. Both are asserted explicitly because a wrong
			// exponent moves a deposit by twelve orders of magnitude.
			plausibleDecimals := token.Decimals == 6 || (chain == ChainBSC && token.Decimals == 18)
			if !plausibleDecimals {
				t.Fatalf("incorrect stablecoin precision: %+v", token)
			}
			if !common.IsHexAddress(token.Contract) || common.HexToAddress(token.Contract) == (common.Address{}) {
				t.Fatalf("%s token has an invalid contract: %+v", chain, token)
			}
		}
	}
	if tokenSymbol(ChainXLayer, "0xb6ceceab302e2e4948951ee7843fc24e92933061") != "USDC" || tokenSymbol(ChainXLayer, "0x74b7f16337b8972027f6196a17a631ac6de26d22") != "USDC.e" {
		t.Fatal("native and bridged USDC were confused")
	}
}

func TestNativeAssetsAreOnlyTheMainnetCoins(t *testing.T) {
	cases := map[string]string{
		ChainEthereum: "ETH",
		ChainBSC:      "BNB",
	}
	for chain, symbol := range cases {
		token, ok := nativeToken(chain)
		if !ok {
			t.Fatalf("%s must support its mainnet coin", chain)
		}
		if token.Symbol != symbol {
			t.Fatalf("%s native symbol = %q, want %q", chain, token.Symbol, symbol)
		}
		if tokenSymbol(chain, scanner.NativeToken) != symbol {
			t.Fatalf("%s tokenSymbol(native) = %q, want %q", chain, tokenSymbol(chain, scanner.NativeToken), symbol)
		}
		if decimals := tokenDecimals(chain, scanner.NativeToken); decimals != 18 {
			t.Fatalf("%s native decimals = %d, want 18", chain, decimals)
		}
	}
	// MATIC/POL is deliberately out of scope until the operator asks for it,
	// and every other chain must keep its previous token-only asset list.
	for _, chain := range []string{ChainPolygon, ChainPlasma, ChainArbitrum, ChainBase, ChainXLayer} {
		if _, ok := nativeToken(chain); ok {
			t.Fatalf("%s must not expose a native coin yet", chain)
		}
	}
}

func TestNativeCoinNeverReachesEthGetLogsFilters(t *testing.T) {
	for _, chain := range evmChains {
		contracts := evmContracts(chain)
		if len(contracts) == 0 {
			t.Fatalf("%s has no ERC-20 contracts", chain)
		}
		// The scanner probes and getLogs filters index this list directly; a
		// non-hex sentinel there would make every query invalid.
		for _, contract := range contracts {
			if contract == scanner.NativeToken || !common.IsHexAddress(contract) {
				t.Fatalf("%s leaked a non-ERC-20 entry into the contract list: %q", chain, contract)
			}
		}
		if len(contracts) != len(watchedTokens[chain])-nativeCount(chain) {
			t.Fatalf("%s contract list and asset table disagree", chain)
		}
	}
}

func nativeCount(chain string) int {
	count := 0
	for _, token := range watchedTokens[chain] {
		if token.Native {
			count++
		}
	}
	return count
}

// The executor decides "value transfer or ERC-20 transfer" from this map, so a
// missing Native flag would silently disable native sweeping.
func TestSweepSpecsCarryTheNativeFlag(t *testing.T) {
	specs := sweepTokenSpecs()
	for chain, symbol := range map[string]string{ChainEthereum: "ETH", ChainBSC: "BNB"} {
		spec, ok := specs[tokenDecimalsKey(chain, scanner.NativeToken)]
		if !ok {
			t.Fatalf("%s native asset is missing from the sweep specs", chain)
		}
		if !spec.Native || spec.Decimals != 18 || spec.Contract != scanner.NativeToken {
			t.Fatalf("%s native spec = %+v (want Native/18/%s)", chain, spec, symbol)
		}
	}
	// Stablecoins must stay non-native, and must keep their own decimals.
	for _, chain := range []string{ChainEthereum, ChainBSC, ChainPolygon} {
		for _, token := range watchedTokens[chain] {
			if token.Native {
				continue
			}
			spec, ok := specs[tokenDecimalsKey(chain, token.Contract)]
			if !ok || spec.Native || spec.Decimals != token.Decimals || spec.Contract != token.Contract {
				t.Fatalf("%s stablecoin spec regressed: %+v", chain, spec)
			}
		}
	}
	if len(specs) != lenAllWatchedAssets() {
		t.Fatalf("sweep specs = %d entries, want %d", len(specs), lenAllWatchedAssets())
	}
}

// The balance reader rebuilds raw amounts with this map: a native row resolved
// through the stablecoin fallback would be 1e12 too small.
func TestSweepDecimalsIncludeNativeAt18(t *testing.T) {
	decimals := sweepDecimalsByAsset()
	for _, chain := range []string{ChainEthereum, ChainBSC} {
		if got := decimals[tokenDecimalsKey(chain, scanner.NativeToken)]; got != 18 {
			t.Fatalf("%s native decimals = %d, want 18", chain, got)
		}
	}
	if got := decimals[tokenDecimalsKey(ChainEthereum, "0xdac17f958d2ee523a2206206994597c13d831ec7")]; got != 6 {
		t.Fatalf("ethereum USDT decimals = %d, want 6", got)
	}
	if got := decimals[tokenDecimalsKey(ChainBSC, "0x55d398326f99059ff775485246999027b3197955")]; got != 18 {
		t.Fatalf("bsc USDT decimals = %d, want 18", got)
	}
}

func lenAllWatchedAssets() int {
	total := 0
	for _, tokens := range watchedTokens {
		total += len(tokens)
	}
	return total
}

// The scanner only sees a native deposit when Native/NativeDecimals are set,
// and an eth_getLogs filter breaks if the sentinel leaks into Tokens.
func TestChainScanConfigWiresNativeScanning(t *testing.T) {
	for chain, want := range map[string]struct {
		native   bool
		symbol   string
		contract int
	}{
		ChainEthereum: {native: true, symbol: "ETH", contract: 2},
		ChainBSC:      {native: true, symbol: "BNB", contract: 2},
		ChainPolygon:  {native: false, contract: 3},
	} {
		config, ok := chainScanConfig(chain, 7, 1234, 32)
		if !ok {
			t.Fatalf("%s produced no scan config", chain)
		}
		if config.Native != want.native {
			t.Fatalf("%s Native = %v, want %v", chain, config.Native, want.native)
		}
		if config.Confirmations != 7 || config.MaxRange != 1234 || config.ScanConcurrency != 32 {
			t.Fatalf("%s scan config dropped chain settings: %+v", chain, config)
		}
		if len(config.Tokens) != want.contract {
			t.Fatalf("%s watched %d contracts, want %d", chain, len(config.Tokens), want.contract)
		}
		for _, contract := range config.Tokens {
			if contract == scanner.NativeToken {
				t.Fatalf("%s put the native sentinel into the eth_getLogs filter", chain)
			}
		}
		if !want.native {
			if config.NativeDecimals != 0 {
				t.Fatalf("%s set native decimals without a native asset", chain)
			}
			continue
		}
		if config.NativeDecimals != 18 {
			t.Fatalf("%s NativeDecimals = %d, want 18", chain, config.NativeDecimals)
		}
		if config.Decimals[scanner.NativeToken] != 18 {
			t.Fatalf("%s scanner cannot resolve the native exponent", chain)
		}
		if token, ok := nativeToken(chain); !ok || token.Symbol != want.symbol {
			t.Fatalf("%s native symbol = %q, want %q", chain, token.Symbol, want.symbol)
		}
	}
}

func TestSweepHistoryRedactsStoredRPCSecretsWithoutRewritingTransactions(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	mock.ExpectQuery("SELECT id, chain, from_address").WillReturnRows(
		sqlmock.NewRows([]string{"id", "chain", "from_address", "to_address", "token", "amount", "gas_tx_hash", "sweep_tx_hash", "status", "error", "created_at", "job_version"}).
			AddRow(1, "bsc", "from", "to", "token", "1", "gas-hash", "sweep-hash", "sweep_pending", "rpc https://example.test/fixture-secret-path?key=fixture-secret-query: fixture-node-message", time.Now(), 1))
	rows, err := (&Service{db: db}).ListSweepRecords(t.Context(), "", 100)
	if err != nil || len(rows) != 1 || rows[0].Status != "sweep_pending" || rows[0].SweepTxHash != "sweep-hash" {
		t.Fatal("history failed or transaction state changed")
	}
	encoded, err := json.Marshal(rows)
	if err != nil || strings.Contains(string(encoded), "fixture-") || strings.Contains(string(encoded), "https://") {
		t.Fatal("history response exposed old RPC credentials")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestWalletUsesPersistentInstallationKeyWithoutDedicatedPaymentKey gives the
// service identical key material under both host configuration states. Wallet
// encryption must be available only when the host marks the key as persistent.
func TestWalletUsesPersistentInstallationKeyWithoutDedicatedPaymentKey(t *testing.T) {
	t.Setenv("SUB2API_DISABLE_BACKGROUND_JOBS", "true")
	for _, persistent := range []bool{false, true} {
		t.Run(map[bool]string{false: "process-local", true: "persisted"}[persistent], func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			settings := newConfigTestStore(readOnlyConfigRepository{raw: "{}"})
			appConfig := &config.Config{Security: config.SecurityConfig{
				SecretEncryptionKey: strings.Repeat("ab", 32), SecretEncryptionKeyConfigured: persistent,
			}}
			s := NewService(db, settings, nil, nil, appConfig)
			mock.ExpectQuery("SELECT value FROM settings").WithArgs("crypto.signer.vault").
				WillReturnRows(sqlmock.NewRows([]string{"value"}))
			status, err := s.WalletStatus(t.Context())
			if err != nil || status.OK != persistent {
				t.Fatalf("wallet encryption ready = %v, want %v, err=%v", status, persistent, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
