package cryptopay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/TokenFlux/TokenRouter/internal/cryptopay/rpc"
)

func TestClassifyNodeErrorUsesOnlyTheFixedVocabulary(t *testing.T) {
	secretErr := errors.New(`Post "https://rpc.example.com/v1/SECRETKEY": dial tcp: lookup failed`)
	cases := map[string]struct {
		err  error
		want string
	}{
		"nil":               {nil, ""},
		"deadline":          {context.DeadlineExceeded, NodeErrTimeout},
		"wrapped deadline":  {fmt.Errorf("all rpc endpoints failed: %w", context.DeadlineExceeded), NodeErrTimeout},
		"transport timeout": {fmt.Errorf("wrapped: %w", rpc.NewTransportFailure(context.DeadlineExceeded)), NodeErrTimeout},
		"transport failure": {rpc.NewTransportFailure(errors.New("refused")), NodeErrConnection},
		"429":               {&rpc.Failure{Kind: "http", HTTPStatus: 429}, NodeErrHTTP429},
		"500":               {&rpc.Failure{Kind: "http", HTTPStatus: 500}, NodeErrHTTP5xx},
		"503 wrapped":       {fmt.Errorf("all rpc endpoints failed: %w", &rpc.Failure{Kind: "http", HTTPStatus: 503}), NodeErrHTTP5xx},
		"599":               {&rpc.Failure{Kind: "http", HTTPStatus: 599}, NodeErrHTTP5xx},
		"401":               {&rpc.Failure{Kind: "http", HTTPStatus: 401}, NodeErrHTTP4xx},
		"403":               {&rpc.Failure{Kind: "http", HTTPStatus: 403}, NodeErrHTTP4xx},
		"redirect is not an error class of its own": {&rpc.Failure{Kind: "http", HTTPStatus: 302}, NodeErrBadResp},
		"json-rpc error object":                     {&rpc.Failure{Kind: "remote", RPCCode: -32005}, NodeErrRPC},
		"undecodable body":                          {&rpc.Failure{Kind: "response"}, NodeErrBadResp},
		"request could not be built":                {&rpc.Failure{Kind: "request"}, NodeErrConnection},
		"text mentioning a timeout is not parsed":   {errors.New("context deadline exceeded"), NodeErrConnection},
		"unknown error carrying a secret":           {secretErr, NodeErrConnection},
	}
	allowed := map[string]bool{
		NodeErrTimeout: true, NodeErrConnection: true, NodeErrHTTP429: true, NodeErrHTTP5xx: true,
		NodeErrHTTP4xx: true, NodeErrRPC: true, NodeErrBadResp: true, "": true,
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := classifyNodeError(tc.err)
			require.Equal(t, tc.want, got)
			require.True(t, allowed[got], "%q is outside the fixed vocabulary", got)
		})
	}
}

func TestNodeIdentityKeepsOnlyMainDomainAndPosition(t *testing.T) {
	for _, tc := range []struct {
		endpoint, label, host string
		index                 int
	}{
		{"https://rpc.ankr.com/bsc/SECRETKEY", "ankr#0", "rpc.ankr.com", 0},
		{"https://bsc-mainnet.blockpi.network/v1/rpc/SECRETKEY?token=SECRETKEY", "blockpi#1", "bsc-mainnet.blockpi.network", 1},
		{"https://user:SECRETKEY@bsc-dataseed.binance.org:8545/x#frag", "binance#2", "bsc-dataseed.binance.org", 2},
		{"https://EthereumRPC.PublicNode.com", "publicnode#0", "ethereumrpc.publicnode.com", 0},
		{"https://1rpc.io/bnb", "1rpc#3", "1rpc.io", 3},
		{"https://eth.llamarpc.com", "llamarpc#0", "eth.llamarpc.com", 0},
		{"https://bsc.drpc.org", "drpc#0", "bsc.drpc.org", 0},
		{"https://node.example.co.uk/key", "example#0", "node.example.co.uk", 0},
		{"https://127.0.0.1:8545/SECRETKEY", "node#0", "127.0.0.1", 0},
		{"http://[::1]:8545/SECRETKEY", "node#0", "::1", 0},
		{"https://rpc.ankr.com./bsc", "ankr#0", "rpc.ankr.com.", 0},
		{"localhost:8545/SECRETKEY", "node#4", "", 4},
		{"not a url", "node#0", "", 0},
		{"", "node#0", "", 0},
		{"https://" + strings.Repeat("a", 40) + ".example.com", strings.Repeat("example", 1) + "#0", strings.Repeat("a", 40) + ".example.com", 0},
	} {
		label, host := nodeIdentity(tc.endpoint, tc.index)
		require.Equal(t, tc.label, label, tc.endpoint)
		require.Equal(t, tc.host, host, tc.endpoint)
		require.NotContains(t, label+host, "SECRETKEY")
		require.NotContains(t, label+host, ":8545")
		require.NotContains(t, label+host, "/")
	}
	require.Equal(t, "node", providerLabel(""))
	require.Equal(t, strings.Repeat("a", 24), providerLabel(strings.Repeat("a", 40)+".test"), "labels are capped at 24 characters")
}

func TestNodeHealthBoundariesAreStrict(t *testing.T) {
	slow := 2000 * time.Millisecond
	for name, tc := range map[string]struct {
		behind  uint64
		latency time.Duration
		want    string
	}{
		"fresh and fast":             {0, 10 * time.Millisecond, NodeStatusOK},
		"exactly 20 blocks behind":   {20, 0, NodeStatusOK},
		"21 blocks behind":           {21, 0, NodeStatusBehind},
		"exactly 2000ms":             {0, 2000 * time.Millisecond, NodeStatusOK},
		"2001ms":                     {0, 2001 * time.Millisecond, NodeStatusSlow},
		"behind outranks slow":       {21, 5 * time.Second, NodeStatusBehind},
		"20 behind but slow is slow": {20, 2001 * time.Millisecond, NodeStatusSlow},
	} {
		t.Run(name, func(t *testing.T) { require.Equal(t, tc.want, nodeHealth(tc.behind, tc.latency, slow)) })
	}
}

func okProbe(head uint64, latency time.Duration) nodeProbe {
	return nodeProbe{Answered: true, HasHead: true, Head: head, HasLatency: true, Latency: latency}
}

func TestBuildNodeStatusesGradesEachNodeAgainstTheBestHead(t *testing.T) {
	endpoints := []string{
		"https://rpc.ankr.com/bsc/SECRETKEY",
		"https://bsc.drpc.org",
		"https://bsc.blockpi.network/v1/rpc/SECRETKEY",
		"https://eth.llamarpc.com",
		"https://1rpc.io/bnb",
		"https://bsc-dataseed.binance.org",
	}
	probes := []nodeProbe{
		okProbe(1000, 100*time.Millisecond),
		okProbe(985, 50*time.Millisecond),
		okProbe(970, 60*time.Millisecond),
		// A wrong-chain node reporting a huge head must not become the yardstick.
		{Answered: true, WrongChain: true, HasHead: true, Head: 5000, HasLatency: true, Latency: 40 * time.Millisecond, ErrorKind: NodeErrWrongChain},
		{ErrorKind: NodeErrTimeout},
		okProbe(1000, 2500*time.Millisecond),
	}
	nodes, usable := buildNodeStatuses(endpoints, probes, 2000*time.Millisecond)
	require.Equal(t, 4, usable, "ok, ok, behind and slow nodes can still scan")
	require.Len(t, nodes, len(endpoints))

	type want struct {
		label, status string
		behind        *uint64
		kind          string
	}
	u := func(v uint64) *uint64 { return &v }
	for i, w := range []want{
		{"ankr#0", NodeStatusOK, u(0), ""},
		{"drpc#1", NodeStatusOK, u(15), ""},
		{"blockpi#2", NodeStatusBehind, u(30), ""},
		{"llamarpc#3", NodeStatusWrongChain, nil, NodeErrWrongChain},
		{"1rpc#4", NodeStatusError, nil, NodeErrTimeout},
		{"binance#5", NodeStatusSlow, u(0), ""},
	} {
		n := nodes[i]
		require.Equal(t, w.label, n.Label)
		require.Equal(t, w.status, n.Status, n.Label)
		require.Equal(t, w.behind, n.Behind, n.Label)
		if w.kind == "" {
			require.Nil(t, n.ErrorKind, n.Label)
		} else {
			require.Equal(t, w.kind, *n.ErrorKind, n.Label)
			require.Nil(t, n.Head, "a node without a trustworthy head reports none")
		}
		require.NotContains(t, n.Label+n.Host, "SECRETKEY")
	}
	require.Equal(t, int64(100), *nodes[0].LatencyMS)
	require.Equal(t, int64(40), *nodes[3].LatencyMS, "a wrong-chain node still answered")
	require.Nil(t, nodes[4].LatencyMS, "a timeout has no round trip to report")
}

func TestBuildNodeStatusesWithoutAnyUsableNode(t *testing.T) {
	endpoints := []string{"https://bsc.drpc.org", "https://1rpc.io/bnb", "https://eth.llamarpc.com"}
	// The third probe is missing entirely and the first has no recorded kind.
	nodes, usable := buildNodeStatuses(endpoints, []nodeProbe{{}, {ErrorKind: NodeErrHTTP429}}, nodeSlowAfter)
	require.Zero(t, usable)
	require.Len(t, nodes, 3)
	for _, n := range nodes {
		require.Equal(t, NodeStatusError, n.Status)
		require.NotNil(t, n.ErrorKind)
	}
	require.Equal(t, NodeErrConnection, *nodes[0].ErrorKind, "never an empty error kind")
	require.Equal(t, NodeErrHTTP429, *nodes[1].ErrorKind)
	require.Equal(t, NodeErrConnection, *nodes[2].ErrorKind)

	empty, none := buildNodeStatuses(nil, nil, nodeSlowAfter)
	require.NotNil(t, empty)
	require.Empty(t, empty)
	require.Zero(t, none)
}

func TestBestProbeHead(t *testing.T) {
	_, ok := bestProbeHead(nil)
	require.False(t, ok)
	_, ok = bestProbeHead([]nodeProbe{{ErrorKind: NodeErrTimeout}, {WrongChain: true, HasHead: true, Head: 9}})
	require.False(t, ok, "failed and wrong-chain nodes give no head")
	best, ok := bestProbeHead([]nodeProbe{okProbe(7, 0), {WrongChain: true, HasHead: true, Head: 99}, okProbe(12, 0), okProbe(10, 0)})
	require.True(t, ok)
	require.Equal(t, uint64(12), best)
}

func TestDominantNodeError(t *testing.T) {
	bad := func(kind string) nodeProbe { return nodeProbe{ErrorKind: kind} }
	require.Equal(t, NodeErrHTTP5xx, dominantNodeError([]nodeProbe{bad(NodeErrTimeout), bad(NodeErrHTTP5xx), bad(NodeErrHTTP5xx)}))
	require.Equal(t, NodeErrTimeout, dominantNodeError([]nodeProbe{bad(NodeErrTimeout), bad(NodeErrHTTP5xx)}), "a tie goes to the first seen")
	require.Equal(t, NodeErrWrongChain, dominantNodeError([]nodeProbe{{WrongChain: true, HasHead: true, Head: 1}, bad(NodeErrTimeout), {WrongChain: true}}))
	require.Equal(t, NodeErrTimeout, dominantNodeError([]nodeProbe{okProbe(1, 0), bad(NodeErrTimeout)}), "healthy nodes do not count")
	require.Equal(t, NodeErrConnection, dominantNodeError([]nodeProbe{{}}), "an unrecorded kind reads as connection")
	require.Empty(t, dominantNodeError([]nodeProbe{okProbe(1, 0)}))
	require.Empty(t, dominantNodeError(nil))
}
