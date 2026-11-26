package sweeper

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/TokenFlux/TokenRouter/internal/cryptopay/rpc"
)

// A receipt that satisfies the configured depth is not final: the head it was
// measured from can itself be reorged. When the chain exposes a finality tag
// the mined block must also be behind it.
func TestSweptReceiptNeedsDepthAndFinality(t *testing.T) {
	e, _, chain, _ := setupExecutor(t)
	chain.mined = true
	chain.txs, chain.blocks = map[string]bool{"0xhash": true}, map[string]uint64{"0xhash": 100}
	chain.head = 110 // depth = 10, well above the required 2

	chain.finalHeight = 99 // chain has not finalized the block yet
	state, err := checkReceipt(t.Context(), e.pools["bsc"], "bsc", "0xhash", 2)
	if err != nil || state != receiptWaiting {
		t.Fatalf("depth alone finalized the sweep: state=%v err=%v", state, err)
	}

	chain.finalHeight = 100 // the block itself is now final
	state, err = checkReceipt(t.Context(), e.pools["bsc"], "bsc", "0xhash", 2)
	if err != nil || state != receiptConfirmed {
		t.Fatalf("final block was not confirmed: state=%v err=%v", state, err)
	}
}

// Chains whose nodes do not serve a finality tag keep the configured depth
// rule that governed confirmation before the tag check existed.
func TestSweepFinalityFallsBackToDepthWhenNoTagIsServed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*localChain)
	}{
		{"tag rejected", func(c *localChain) { c.finalityErr = true }},
		{"tag null", func(c *localChain) { c.finalityNull = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, chain, _ := setupExecutor(t)
			chain.mined = true
			chain.txs, chain.blocks = map[string]bool{"0xhash": true}, map[string]uint64{"0xhash": 100}
			chain.head = 110
			tc.setup(chain)
			state, err := checkReceipt(t.Context(), e.pools["bsc"], "bsc", "0xhash", 2)
			if err != nil || state != receiptConfirmed {
				t.Fatalf("unavailable finality tag must keep depth policy: state=%v err=%v", state, err)
			}
		})
	}
}

// BSC's safe tag is only its justified head; using it would weaken the
// economic finality the chain exposes through finalized.
func TestFinalityTagsPreferFinalizedAndNeverUseBSCSafe(t *testing.T) {
	if tags := finalityTags("bsc"); len(tags) != 1 || tags[0] != "finalized" {
		t.Fatalf("bsc finality tags = %v", tags)
	}
	tags := finalityTags("ethereum")
	if len(tags) != 2 || tags[0] != "finalized" || tags[1] != "safe" {
		t.Fatalf("ethereum finality tags = %v", tags)
	}
}

func TestTagHeightReportsNullAsUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":null}`))
	}))
	t.Cleanup(server.Close)
	pool := rpc.NewPoolWithHTTPClient(server.URL, server.Client())
	height, ok, err := pool.MaxBlockTagHeight(t.Context(), "finalized")
	if err != nil || ok || height != 0 {
		t.Fatalf("null tag must be reported as unavailable: height=%d ok=%t err=%v", height, ok, err)
	}
}

// A provider answering a finality tag with something that is not a block header
// must yield a classified failure, so the caller cannot mistake it for a final
// block and a durable record cannot receive an unclassifiable decode error.
func TestTagHeightRejectsMalformedBlock(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"number":"finalized"}}`))
	}))
	t.Cleanup(server.Close)
	pool := rpc.NewPoolWithHTTPClient(server.URL, server.Client())
	_, _, err := pool.MaxBlockTagHeight(t.Context(), "finalized")
	if err == nil {
		t.Fatal("a malformed tag height must not silently pass as final")
	}
	if _, ok := err.(*rpc.Failure); !ok {
		t.Fatalf("a malformed tag answer must be a classified rpc.Failure, got %T: %v", err, err)
	}
}

func signSweep(t *testing.T, gasLimit uint64) string {
	t.Helper()
	_, _, chain, entry := setupExecutor(t)
	return chain.signed(EVMSignRequest{
		Index: 42, Kind: "sweep", ChainID: 56, Token: entry.Token,
		To: testDestination, Amount: "10000000", Nonce: 3, GasPrice: "1", GasLimit: gasLimit,
	})
}

func TestShouldRetrySweepRevertOnlyForLegacyLimit(t *testing.T) {
	legacy := signSweep(t, legacyTransferGasLimit)
	if !shouldRetrySweepRevert(false, legacy) {
		t.Fatal("a transfer signed with the legacy limit must be retried once")
	}
	current := signSweep(t, transferGasLimit)
	if shouldRetrySweepRevert(false, current) {
		t.Fatal("a retry that already used the larger limit must go to manual review")
	}
	if shouldRetrySweepRevert(true, legacy) {
		t.Fatal("gas top-ups must never be auto-retried")
	}
	if shouldRetrySweepRevert(false, "") {
		t.Fatal("undecodable bytes must not be retried automatically")
	}
}

func TestGasLimitMatchesLegacyAndCurrent(t *testing.T) {
	if !gasLimitMatches(legacyTransferGasLimit, 0) || !gasLimitMatches(transferGasLimit, 0) {
		t.Fatal("both legacy and current sweep limits must validate")
	}
	if gasLimitMatches(12345, 0) {
		t.Fatal("an arbitrary gas limit must not validate")
	}
	if !gasLimitMatches(21000, 21000) || gasLimitMatches(21001, 21000) {
		t.Fatal("exact matching must stay strict for gas top-ups")
	}
}
