package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

type failureRoundTripper func(*http.Request) (*http.Response, error)

func TestFailureCategoriesDriveRetryDecisions(t *testing.T) {
	cases := []struct {
		name      string
		failure   *Failure
		category  string
		transient bool
	}{
		{"deadline", NewTransportFailure(context.DeadlineExceeded), CategoryTimeout, true},
		{"dial", NewTransportFailure(errors.New("dial tcp: refused")), CategoryTransport, true},
		{"canceled", NewTransportFailure(context.Canceled), CategoryTransport, false},
		{"nonce too low", &Failure{Kind: "remote", RPCCode: -32000, Reason: "nonce too low"}, CategoryRejectedKnown, false},
		{"already known", &Failure{Kind: "remote", Reason: ReasonAlreadyKnown}, CategoryRejectedKnown, false},
		{"insufficient", &Failure{Kind: "remote", Reason: "insufficient funds for gas"}, CategoryRejectedKnown, false},
		{"invalid key", &Failure{Kind: "remote", Message: "invalid api key supplied"}, CategoryAuthConfig, false},
		{"unauthorized http", &Failure{Kind: "http", HTTPStatus: 401}, CategoryAuthConfig, false},
		{"rate limited", &Failure{Kind: "http", HTTPStatus: 429}, CategoryTransport, true},
		{"gateway", &Failure{Kind: "http", HTTPStatus: 502}, CategoryTransport, true},
		{"bad request", &Failure{Kind: "http", HTTPStatus: 400}, CategoryUnknown, false},
		{"unknown node text", &Failure{Kind: "remote", Message: "something new"}, CategoryUnknown, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.failure.Category(); got != tc.category {
				t.Fatalf("category = %q, want %q", got, tc.category)
			}
			if got := IsTransient(tc.failure); got != tc.transient {
				t.Fatalf("IsTransient = %v, want %v", got, tc.transient)
			}
			if got := CategoryOf(errors.Join(errors.New("wrapped"), tc.failure)); got != tc.category {
				t.Fatalf("wrapped category = %q, want %q", got, tc.category)
			}
		})
	}
	if IsTransient(nil) || IsTransient(errors.New("plain")) || IsRejectedKnown(nil) {
		t.Fatal("plain errors must not be classified as RPC failures")
	}
}

func TestSanitizedNodeTextKeepsShapeButNotSecrets(t *testing.T) {
	const secret = "0x" + "AbCdEf0123456789AbCdEf0123456789AbCdEf01"
	message := "execution reverted: https://rpc.example.test/v1/private?api_key=fixture-key " +
		"address " + secret + " password=hunter2"
	got := SanitizeNodeMessage(message)
	for _, leaked := range []string{"https://", "fixture-key", "AbCdEf", "hunter2", "rpc.example.test"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("sanitiser leaked %q in %q", leaked, got)
		}
	}
	if !strings.Contains(got, "execution reverted") {
		t.Fatalf("sanitiser dropped the reason: %q", got)
	}
	long := strings.Repeat("x", 400)
	if out := SanitizeNodeMessage(long); len(out) > 161 || !strings.HasSuffix(out, "…") {
		t.Fatalf("long text was not bounded: %d bytes", len(out))
	}
	if SanitizeNodeMessage("  ") != "" {
		t.Fatal("blank text must stay blank")
	}
}

// Error() is the durable-record boundary and must never grow node text; only
// Detail() carries the sanitised sentence for logs.
func TestNodeTextStaysOutOfErrorAndInDetail(t *testing.T) {
	failure := &Failure{
		Kind: "remote", RPCCode: -32000,
		Message: "invalid api key for https://node.example.test/key?token=secret",
	}
	if strings.Contains(failure.Error(), "node.example.test") || strings.Contains(failure.Error(), "secret") {
		t.Fatalf("Error leaked node text: %q", failure.Error())
	}
	detail := failure.Detail()
	if !strings.Contains(detail, "invalid api key") || strings.Contains(detail, "secret") || strings.Contains(detail, "https://") {
		t.Fatalf("Detail = %q", detail)
	}
	if DetailOf(errors.Join(errors.New("x"), failure)) != detail {
		t.Fatal("DetailOf did not unwrap the failure")
	}
	if DetailOf(errors.New("plain")) != "" {
		t.Fatal("a plain error has no node detail")
	}
}

func TestTransactionByHashDistinguishesAbsenceFromFailure(t *testing.T) {
	var mode string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "null":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": nil})
		case "present":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": 1,
				"result": map[string]any{"hash": "0xabc"},
			})
		case "mismatch":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": 1,
				"result": map[string]any{"hash": "0xdifferent"},
			})
		default:
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer s.Close()
	pool := NewPoolWithHTTPClient(s.URL, s.Client())

	mode = "null"
	if present, err := pool.TransactionByHash(t.Context(), "0xabc"); err != nil || present {
		t.Fatalf("null must be an error-free absence: present=%v err=%v", present, err)
	}
	mode = "present"
	if present, err := pool.TransactionByHash(t.Context(), "0xabc"); err != nil || !present {
		t.Fatalf("a returned transaction must be present: present=%v err=%v", present, err)
	}
	mode = "mismatch"
	if _, err := pool.TransactionByHash(t.Context(), "0xabc"); err == nil {
		t.Fatal("a different hash must not count as the reserved transaction")
	}
	mode = "failure"
	_, err := pool.TransactionByHash(t.Context(), "0xabc")
	if err == nil {
		t.Fatal("an HTTP failure must stay an error, never an absence")
	}
	var failure *Failure
	if !errors.As(err, &failure) || !failure.IsTransient() {
		t.Fatalf("a 502 from a node must be a transient failure, got %v", err)
	}
	if failure.Host == "" {
		t.Fatal("a failed call must name the endpoint host for the logs")
	}
}

func nodeAnswering(t *testing.T, code int, message string) *Pool {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"error": map[string]any{"code": code, "message": message},
		})
	}))
	t.Cleanup(s.Close)
	return NewPoolWithHTTPClient(s.URL, s.Client())
}

func TestNodeRejectionIsReducedToOneOfOurOwnPhrases(t *testing.T) {
	const secret = "fixture-private-key-0xAbCdEf0123456789AbCdEf0123456789AbCdEf01"
	cases := []struct{ node, want string }{
		{"nonce too low: address " + secret + ", tx: 7 state: 9", "RPC node rejected request (code -32000): nonce too low"},
		{"replacement transaction underpriced", "RPC node rejected request (code -32000): replacement transaction underpriced"},
		{"transaction underpriced: " + secret, "RPC node rejected request (code -32000): transaction underpriced"},
		{"ALREADY KNOWN", "RPC node rejected request (code -32000): transaction already known"},
		{"insufficient funds for gas * price + value: address " + secret, "RPC node rejected request (code -32000): insufficient funds for gas"},
		{"execution reverted: " + secret, "RPC node rejected request (code -32000): execution reverted"},
		{"query exceeds max block range 5000", "RPC node rejected request (code -32000): query range or result limit exceeded"},
		{"something the table has never seen " + secret, "RPC node rejected request (code -32000)"},
		{"", "RPC node rejected request (code -32000)"},
	}
	for _, tc := range cases {
		t.Run(tc.node, func(t *testing.T) {
			_, err := nodeAnswering(t, -32000, tc.node).Call(t.Context(), "eth_sendRawTransaction", []any{"0x00"})
			if err == nil {
				t.Fatal("a node error must fail the call")
			}
			if got := SafeError(err); got != tc.want {
				t.Fatalf("SafeError = %q, want %q", got, tc.want)
			}
			if strings.Contains(err.Error(), "fixture-") || strings.Contains(SafeError(err), "0xAbCd") {
				t.Fatal("node text reached the error")
			}
			// What a sweep record stores must survive the next round of sanitising.
			if again := SafeRecordedError(SafeError(err)); again != tc.want {
				t.Fatalf("stored text changed on re-sanitising: %q", again)
			}
		})
	}
}

func TestOnlyAnAlreadyKnownRejectionCountsAsAlreadyKnown(t *testing.T) {
	_, err := nodeAnswering(t, -32000, "already known").Call(t.Context(), "eth_sendRawTransaction", []any{"0x00"})
	if !IsAlreadyKnown(err) || !IsAlreadyKnown(fmt.Errorf("wrapped: %w", err)) {
		t.Fatal("an already-known answer was not recognised")
	}
	for _, other := range []string{"nonce too low", "transaction underpriced", "execution reverted", ""} {
		_, err := nodeAnswering(t, -32000, other).Call(t.Context(), "eth_sendRawTransaction", []any{"0x00"})
		if IsAlreadyKnown(err) {
			t.Fatalf("%q treated as already known", other)
		}
	}
	if IsAlreadyKnown(nil) || IsAlreadyKnown(errors.New("already known")) || IsAlreadyKnown(&Failure{Kind: "http", HTTPStatus: 500}) {
		t.Fatal("only a remote failure carrying the reason qualifies")
	}
}

func TestRecordedErrorsKeepTheirReasonAndNothingElse(t *testing.T) {
	kept := []string{
		"current credited balance is below the per-address sweep minimum",
		"current credited sweep balance or address assignment is invalid",
		"live balance 9000000 is below planned amount 10000000",
		// Gas funding waits written by the sweeper. They name an operator
		// action, so the sweep record must keep the sentence instead of
		// replacing it with the generic payment failure.
		"insufficient native gas budget to fund this sweep; it will be retried when the gas budget allows",
		"gas wallet native balance cannot cover another top-up; fund the gas wallet and the sweep will resume",
		"sweep gas budget is not configured for bsc",
		`chain "bsc" is outside the requested sweep scope`,
		"RPC node rejected request (code -32000): nonce too low",
		"signed transaction fee does not match request",
		"current credited balance is below the per-address sweep minimum (gave up after 25 attempts)",
		"RPC node rejected request (code -32000): replacement transaction underpriced (gave up after 3 attempts)",
		"Payment operation failed; reconcile transaction status before retrying (gave up after 25 attempts)",
	}
	for _, text := range kept {
		if got := SafeRecordedError(text); got != text {
			t.Errorf("lost a fixed message: %q -> %q", text, got)
		}
		if got := SafeRecordedError(SafeRecordedError(text)); got != text {
			t.Errorf("not stable when applied twice: %q -> %q", text, got)
		}
	}
	const generic = "Payment operation failed; reconcile transaction status before retrying"
	hidden := map[string]string{
		"dial tcp 10.0.0.5:443: refused fixture-secret (gave up after 25 attempts)": generic + " (gave up after 25 attempts)",
		"https://rpc.example/fixture-key-path (gave up after 7 attempts)":           generic + " (gave up after 7 attempts)",
		"live balance 9000000 is below planned amount 10000000 fixture-secret":      generic,
		"RPC node rejected request (code -32000): fixture-secret":                   generic,
		"nonce too low (gave up after 25 attempts) fixture-secret":                  generic,
		" (gave up after 25 attempts)":                                              generic,
	}
	for text, want := range hidden {
		got := SafeRecordedError(text)
		if strings.Contains(got, "fixture-") || strings.Contains(got, "10.0.0.5") {
			t.Errorf("leaked: %q -> %q", text, got)
		}
		if got != want {
			t.Errorf("%q -> %q, want %q", text, got, want)
		}
	}
}

func TestConfirmedNonceAsksForMinedTransactionsOnly(t *testing.T) {
	var mu sync.Mutex
	var tags []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var tag string
		if len(req.Params) == 2 {
			_ = json.Unmarshal(req.Params[1], &tag)
		}
		mu.Lock()
		tags = append(tags, req.Method+":"+tag)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": "0x30"})
	}))
	defer s.Close()
	pool := NewPoolWithHTTPClient(s.URL, s.Client())
	if n, err := pool.ConfirmedNonce(t.Context(), "fixture"); err != nil || n != 48 {
		t.Fatalf("confirmed nonce = %d, %v", n, err)
	}
	if n, err := pool.Nonce(t.Context(), "fixture"); err != nil || n != 48 {
		t.Fatalf("pending nonce = %d, %v", n, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tags) != 2 || tags[0] != "eth_getTransactionCount:latest" || tags[1] != "eth_getTransactionCount:pending" {
		t.Fatalf("block tags asked for: %v", tags)
	}
}

func TestRPCFailuresNeverExposeEndpointOrNodeControlledText(t *testing.T) {
	const pathSecret = "fixture-rpc-path-secret"
	const querySecret = "fixture-rpc-query-secret"
	const nodeSecret = "fixture-node-error-secret"
	for _, scenario := range []string{"http", "node error", "invalid JSON", "invalid quantity"} {
		t.Run(scenario, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				switch scenario {
				case "http":
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(nodeSecret))
				case "node error":
					_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": -32005, "message": nodeSecret}})
				case "invalid JSON":
					_, _ = w.Write([]byte(nodeSecret))
				case "invalid quantity":
					_ = json.NewEncoder(w).Encode(map[string]any{"result": nodeSecret})
				}
			}))
			defer server.Close()
			pool := NewPoolWithHTTPClient(server.URL+"/"+pathSecret+"?api_key="+querySecret, server.Client())
			_, err := pool.BlockNumber(t.Context())
			if err == nil {
				t.Fatal("expected RPC failure")
			}
			var log bytes.Buffer
			slog.New(slog.NewJSONHandler(&log, nil)).Warn("payment scan failed", "error", err)
			for _, text := range []string{err.Error(), SafeError(err), log.String()} {
				for _, secret := range []string{pathSecret, querySecret, nodeSecret, server.URL} {
					if strings.Contains(text, secret) {
						t.Fatal("RPC failure leaked fixture credential or upstream message")
					}
				}
			}
			var failure *Failure
			if !errors.As(err, &failure) {
				t.Fatal("missing stable failure classification")
			}
			if scenario == "http" && failure.HTTPStatus != http.StatusForbidden {
				t.Fatal("upstream HTTP status was not preserved")
			}
			if scenario == "node error" && failure.RPCCode != -32005 {
				t.Fatal("upstream numeric RPC code was not preserved")
			}
		})
	}
}

func (f failureRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRPCTransportFailuresKeepOnlySafeContextSentinels(t *testing.T) {
	for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			client := &http.Client{Transport: failureRoundTripper(func(*http.Request) (*http.Response, error) {
				return nil, &url.Error{Op: "POST", URL: "https://example.test/fixture-private-path?api_key=fixture-private-key", Err: fmt.Errorf("fixture-upstream-secret: %w", sentinel)}
			})}
			_, err := NewPoolWithHTTPClient("https://example.test/fixture-private-path?api_key=fixture-private-key", client).BlockNumber(t.Context())
			if !errors.Is(err, sentinel) {
				t.Fatal("context cancellation/timeout semantics changed")
			}
			var unsafe *url.Error
			if errors.As(err, &unsafe) || strings.Contains(err.Error(), "fixture-") {
				t.Fatal("unsafe transport details remain in returned error chain")
			}
		})
	}
}

func TestHistoricalErrorTextOnlyRetainsTrustedMessages(t *testing.T) {
	for _, text := range []string{
		"rpc https://example.test/fixture-private-path?key=fixture-private-key: http 403",
		"RPC response is invalid fixture-upstream-secret",
		"RPC node rejected request (code 403) fixture-upstream-secret",
	} {
		got := SafeRecordedError(text)
		if strings.Contains(got, "fixture-") || strings.Contains(got, "https://") {
			t.Fatal("legacy record leaked endpoint credentials")
		}
	}
	for _, text := range []string{"", "RPC HTTP request failed (status 403)", "RPC node rejected request (code -32005)", "awaiting canonical receipt and required confirmations"} {
		if SafeRecordedError(text) != text {
			t.Fatal("stable status/category was lost")
		}
	}
}
