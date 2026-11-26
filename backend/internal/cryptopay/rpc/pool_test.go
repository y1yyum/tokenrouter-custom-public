package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// laggingNode answers getLogs like a node that has only the blocks it knows:
// the observed head is the only clue that its (possibly empty) answer is short.
type laggingNode struct {
	server    *httptest.Server
	head      uint64
	headCalls atomic.Int64
	logCalls  atomic.Int64
}

func TestGetLogsRejectsInvalidResultsAndRotatesEndpoints(t *testing.T) {
	for _, fixture := range []struct {
		name, response string
	}{
		{"null", `{"jsonrpc":"2.0","id":1,"result":null}`},
		{"missing", `{"jsonrpc":"2.0","id":1}`},
		{"object", `{"jsonrpc":"2.0","id":1,"result":{}}`},
		{"string", `{"jsonrpc":"2.0","id":1,"result":"[]"}`},
		{"number", `{"jsonrpc":"2.0","id":1,"result":0}`},
		{"boolean", `{"jsonrpc":"2.0","id":1,"result":false}`},
		{"null_member", `{"jsonrpc":"2.0","id":1,"result":[null]}`},
		{"empty_object_member", `{"jsonrpc":"2.0","id":1,"result":[{}]}`},
		{"non_object_member", `{"jsonrpc":"2.0","id":1,"result":["fixture-invalid-log"]}`},
		{"malformed_json", `{"jsonrpc":"2.0","id":1,"result":[`},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			var invalidCalls, healthyCalls atomic.Int64
			invalid := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				invalidCalls.Add(1)
				_, _ = w.Write([]byte(fixture.response))
			}))
			defer invalid.Close()
			healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				healthyCalls.Add(1)
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":[]}`))
			}))
			defer healthy.Close()

			_, err := NewPoolWithHTTPClient(invalid.URL, invalid.Client()).Call(t.Context(), "eth_getLogs", []any{})
			var failure *Failure
			if err == nil || !errors.As(err, &failure) || failure.Kind != "response" {
				t.Fatal("invalid logs response was accepted or lost its safe classification", err)
			}
			if strings.Contains(err.Error(), invalid.URL) || strings.Contains(err.Error(), "fixture-invalid-log") {
				t.Fatal("invalid logs response leaked endpoint or upstream-controlled data")
			}

			result, err := NewPoolWithHTTPClient(invalid.URL+","+healthy.URL, healthy.Client()).Call(t.Context(), "eth_getLogs", []any{})
			if err != nil || !bytes.Equal(result, []byte("[]")) {
				t.Fatal("bad logs result prevented failover to a valid empty result", err)
			}
			if invalidCalls.Load() != 2 || healthyCalls.Load() != 1 {
				t.Fatal("unexpected endpoint selection", invalidCalls.Load(), healthyCalls.Load())
			}
		})
	}
}

func TestGetLogsEmptyArrayAndPendingReceiptNullKeepDifferentContracts(t *testing.T) {
	for _, fixture := range []struct {
		name, method, result string
	}{
		{"empty_logs", "eth_getLogs", `[]`},
		{"pending_receipt", "eth_getTransactionReceipt", `null`},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":` + fixture.result + `}`))
			}))
			defer server.Close()
			result, err := NewPoolWithHTTPClient(server.URL, server.Client()).Call(t.Context(), fixture.method, []any{})
			if err != nil || string(result) != fixture.result {
				t.Fatal("legitimate method result was rejected", err)
			}
		})
	}
}

func blockNumberServer(t *testing.T, head string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":%q}`, head)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMaxBlockNumberPicksHighestHealthyEndpoint(t *testing.T) {
	low := blockNumberServer(t, "0x10")
	high := blockNumberServer(t, "0x2a")
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(broken.Close)

	pool := NewPoolWithHTTPClient(low.URL+","+high.URL+","+broken.URL, low.Client())
	head, err := pool.MaxBlockNumber(context.Background())
	if err != nil {
		t.Fatalf("MaxBlockNumber failed: %v", err)
	}
	if head != 42 {
		t.Fatalf("head = %d, want 42 (the highest healthy endpoint)", head)
	}
}

func TestMaxBlockNumberReportsFailureWhenNoEndpointAnswers(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(broken.Close)

	pool := NewPoolWithHTTPClient(broken.URL, broken.Client())
	if _, err := pool.MaxBlockNumber(context.Background()); err == nil {
		t.Fatal("expected an error when every endpoint fails")
	}
}

func newLaggingNode(t *testing.T, head uint64) *laggingNode {
	t.Helper()
	node := &laggingNode{head: head}
	node.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		w.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case "eth_blockNumber":
			node.headCalls.Add(1)
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":"0x%x"}`, node.head)
		case "eth_getLogs":
			node.logCalls.Add(1)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":[]}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(node.server.Close)
	return node
}

func TestCallEndpointNamesTheNodeThatAnswered(t *testing.T) {
	first := newLaggingNode(t, 0x10)
	second := newLaggingNode(t, 0x2a)
	pool := NewPoolWithHTTPClient(first.server.URL+","+second.server.URL, first.server.Client())

	result, endpoint, err := pool.CallEndpoint(context.Background(), "eth_blockNumber", nil)
	if err != nil {
		t.Fatalf("CallEndpoint failed: %v", err)
	}
	if string(result) != `"0x10"` || endpoint != first.server.URL {
		t.Fatalf("result=%s endpoint=%q, want the first node and its head", result, endpoint)
	}
}

func TestRequireSyncedToRejectsAndCoolsDownALaggingNode(t *testing.T) {
	lagging := newLaggingNode(t, 100)
	synced := newLaggingNode(t, 0x2a)
	pool := NewPoolWithHTTPClient(lagging.server.URL+","+synced.server.URL, lagging.server.Client())

	head, err := pool.RequireSyncedTo(context.Background(), lagging.server.URL, 200)
	var failure *Failure
	if !errors.As(err, &failure) || failure.Kind != "stale" || head != 100 {
		t.Fatalf("behind node was not rejected as stale: head=%d err=%v", head, err)
	}
	if failure.Category() != CategoryStale {
		t.Fatalf("stale failure category = %q", failure.Category())
	}
	if endpointCooldown(failure) <= 0 {
		t.Fatal("a stale node must cool down so the rotation stops using it")
	}
	// The next call must skip the node that just proved it is behind.
	result, endpoint, err := pool.CallEndpoint(context.Background(), "eth_blockNumber", nil)
	if err != nil || endpoint != synced.server.URL || string(result) != `"0x2a"` {
		t.Fatalf("rotation did not move to the caught-up node: endpoint=%q result=%s err=%v", endpoint, result, err)
	}
	if calls := lagging.headCalls.Load(); calls != 1 {
		t.Fatalf("lagging node was queried %d times, want once while cooling down", calls)
	}
}

func TestRequireSyncedToAcceptsANodeAtTheRequestedHeight(t *testing.T) {
	node := newLaggingNode(t, 200)
	pool := NewPoolWithHTTPClient(node.server.URL, node.server.Client())
	head, err := pool.RequireSyncedTo(context.Background(), node.server.URL, 200)
	if err != nil || head != 200 {
		t.Fatalf("node at the requested height was rejected: head=%d err=%v", head, err)
	}
}

func TestEndpointHeadRejectsAnUnparsableAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"not-a-height"}`))
	}))
	t.Cleanup(server.Close)
	pool := NewPoolWithHTTPClient(server.URL, server.Client())
	if _, err := pool.EndpointHead(context.Background(), server.URL); err == nil {
		t.Fatal("an invalid head must not be accepted")
	}
}

func TestRPCRejectsMalformedQuantitiesBeforeUse(t *testing.T) {
	for _, input := range []string{"0x1junk", "0x-1", "0x+1", "1", "0x01", "0x", "0x10000000000000000000000000000000000000000000000000000000000000000"} {
		t.Run(input, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": input})
			}))
			defer s.Close()
			pool := NewPoolWithHTTPClient(s.URL, s.Client())
			for name, call := range map[string]func() error{
				"head":           func() error { _, err := pool.BlockNumber(t.Context()); return err },
				"nonce":          func() error { _, err := pool.Nonce(t.Context(), "fixture"); return err },
				"native balance": func() error { _, err := pool.Balance(t.Context(), "fixture"); return err },
				"gas price":      func() error { _, err := pool.GasPrice(t.Context()); return err },
			} {
				t.Run(name, func(t *testing.T) {
					if err := call(); err == nil {
						t.Fatalf("malformed RPC quantity %q accepted for %s", input, name)
					}
				})
			}
		})
	}
}
