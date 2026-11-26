package cryptopay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/TokenFlux/TokenRouter/internal/cryptopay/mode"
	"github.com/TokenFlux/TokenRouter/internal/cryptopay/rpc"
	"github.com/TokenFlux/TokenRouter/internal/cryptopay/scanner"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

type refusalCase struct {
	name            string
	row             func(s *depositSpec)               // what the database stored
	chain           func(e *confirmEnv, s depositSpec) // what the chain says instead
	status, message string
	// untouchedChain: refused from the row alone, before any RPC call.
	// noReceipt: refused before a receipt was requested.
	untouchedChain, noReceipt bool
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf
}

// A ledger outcome is never inferred from Process's return value alone: the
// row decides. Cases that need PostgreSQL-only SQL (the ledger's ignore path
// writes NOW()) are stood in for by a processor that edits the row the way that
// path would.
func TestManualConfirmReportsWhatTheLedgerActuallyDid(t *testing.T) {
	setRow := func(e *confirmEnv, status, reason string) func(context.Context, scanner.Transfer, int) (bool, error) {
		return func(ctx context.Context, tr scanner.Transfer, _ int) (bool, error) {
			_, err := e.db.ExecContext(ctx, `UPDATE deposit_transactions SET status = $1, reason = $2
				WHERE chain = $3 AND tx_hash = $4 AND log_index = $5`, status, reason, tr.Chain, tr.TxHash, tr.LogIndex)
			return false, err
		}
	}
	for _, tc := range []struct {
		name            string
		setup           func(e *confirmEnv)
		status, message string
		rowStatus       string
		forbid          []string
	}{
		{
			name: "sender is a system address", rowStatus: "pending", status: DepositConfirmRejected, message: "账本拒绝入账",
			setup: func(e *confirmEnv) { e.resolver.system[strings.ToLower(confirmFrom)] = true },
		},
		{
			name: "recipient is not a deposit address", rowStatus: "pending", status: DepositConfirmRejected, message: "账本拒绝入账",
			setup: func(e *confirmEnv) { e.resolver.deposit = map[string]bool{} },
		},
		{
			name: "ledger fails with database text", rowStatus: "pending", status: DepositConfirmError, message: "入账处理失败",
			forbid: []string{"secret-db-user", "10.9.8.7", "5432"},
			setup: func(e *confirmEnv) {
				e.processor.replace = func(context.Context, scanner.Transfer, int) (bool, error) {
					return false, errors.New("dial tcp 10.9.8.7:5432: password authentication failed for user secret-db-user")
				}
			},
		},
		{
			name: "ledger times out", rowStatus: "pending", status: DepositConfirmError, message: "超时或被取消",
			setup: func(e *confirmEnv) {
				e.processor.replace = func(context.Context, scanner.Transfer, int) (bool, error) {
					return false, fmt.Errorf("credit deposit: %w", context.DeadlineExceeded)
				}
			},
		},
		{
			name: "instance is read-only", rowStatus: "pending", status: DepositConfirmError, message: "只读测试模式",
			setup: func(e *confirmEnv) {
				e.processor.replace = func(context.Context, scanner.Transfer, int) (bool, error) {
					return false, fmt.Errorf("mark credited: %w", mode.ErrReadOnly)
				}
			},
		},
		{
			name: "ledger ignores the transfer below the minimum", rowStatus: "ignored", status: DepositConfirmRejected,
			message: "低于最小入账金额", setup: func(e *confirmEnv) { e.processor.replace = setRow(e, "ignored", "below_min") },
		},
		{
			name: "ledger flags a reorg duplicate", rowStatus: "reverted", status: DepositConfirmRejected,
			message: "需要人工核对", setup: func(e *confirmEnv) {
				e.processor.replace = setRow(e, "reverted", "reorg_duplicate_credit_manual_review")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			e := newConfirmEnv(t)
			id, _ := e.seed(t, 1)
			tc.setup(e)

			result := e.confirm(id)
			require.Equal(t, tc.status, result.Status, result.Message)
			require.Contains(t, result.Message, tc.message)
			require.Nil(t, result.OrderID)
			for _, secret := range tc.forbid {
				require.NotContains(t, result.Message, secret)
				require.NotContains(t, logs.String(), secret, "logs must carry the sanitised cause only")
			}
			require.Equal(t, tc.rowStatus, e.status(t, id))
			require.Equal(t, 0.0, e.balance(t), "a declined deposit must not move the balance")
			require.Zero(t, e.orders(t))
		})
	}
}

// Another instance credited the row between our read and our ledger call. The
// ledger credited it once; this request must say so rather than claim the credit.
func TestManualConfirmReportsACreditMadeByAnotherCallerAsAlreadyCredited(t *testing.T) {
	e := newConfirmEnv(t)
	id, _ := e.seed(t, 1)
	e.processor.replace = func(ctx context.Context, tr scanner.Transfer, decimals int) (bool, error) {
		if _, err := e.processor.inner.Process(ctx, tr, decimals); err != nil {
			return false, err
		}
		return false, nil
	}
	result := e.confirm(id)
	require.Equal(t, DepositConfirmAlreadyCredited, result.Status, result.Message)
	require.NotNil(t, result.OrderID)
	require.Equal(t, 67.0, e.balance(t))
	require.Equal(t, 1, e.orders(t))
}

func TestManualConfirmNeverLeaksTheRPCEndpointOrItsCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, message string
		breakIt       func(e *confirmEnv)
	}{
		{
			name: "node answers 503 to the receipt", message: "status 503",
			breakIt: func(e *confirmEnv) { e.chain.httpFail["eth_getTransactionReceipt"] = 503 },
		},
		{
			name: "node is unreachable", message: "没有通过链 ID 校验的 RPC 节点",
			breakIt: func(e *confirmEnv) { e.chain.server.Close() },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			e := newConfirmEnv(t)
			id, _ := e.seed(t, 1)
			u, err := url.Parse(e.chain.server.URL)
			require.NoError(t, err)
			host := u.Host
			u.User = url.UserPassword("rpcuser-SECRET", "rpcpass-SECRET")
			u.Path = "/v1/APIKEY-SECRET-123"
			pool := rpc.NewPoolWithHTTPClient(u.String(), e.chain.server.Client())
			e.confirmer.pool = func(string) (*rpc.Pool, error) { return pool, nil }
			tc.breakIt(e)
			before := e.snapshot(t)

			result := e.confirm(id)
			require.Equal(t, DepositConfirmError, result.Status, result.Message)
			require.Contains(t, result.Message, tc.message)
			require.Equal(t, before, e.snapshot(t))
			for _, secret := range []string{"rpcuser-SECRET", "rpcpass-SECRET", "APIKEY-SECRET-123", host, "127.0.0.1", "http://"} {
				require.NotContains(t, result.Message, secret)
				require.NotContains(t, logs.String(), secret)
			}
		})
	}
}

func TestManualConfirmLogsWhoConfirmedWhichDepositWithoutRPCDetails(t *testing.T) {
	logs := captureLogs(t)
	e := newConfirmEnv(t)
	id, s := e.seed(t, 1)
	require.Equal(t, DepositConfirmCredited, e.confirm(id).Status)

	out := logs.String()
	require.Contains(t, out, "manual deposit confirmation")
	require.Contains(t, out, "admin_id=9")
	require.Contains(t, out, fmt.Sprintf("deposit_id=%d", id))
	require.Contains(t, out, "chain=bsc")
	require.Contains(t, out, "tx_hash="+s.tx)
	require.Contains(t, out, "status=credited")
	require.NotContains(t, out, e.chain.server.URL)
	require.NotContains(t, out, "127.0.0.1")
}

func TestManualConfirmRefusesWhatTheChainDoesNotConfirm(t *testing.T) {
	otherHash := "0x" + strings.Repeat("99", 32)
	cases := []refusalCase{
		{
			name: "reverted transaction", status: DepositConfirmRejected, message: "已回滚",
			chain: func(e *confirmEnv, s depositSpec) { e.chain.receipts[s.tx].status = "0x0" },
		},
		{
			name: "receipt not indexed yet", status: DepositConfirmError, message: "没有返回这笔交易的收据",
			chain: func(e *confirmEnv, s depositSpec) { e.chain.receipts[s.tx].absent = true },
		},
		{
			name: "block hash differs from the canonical block", status: DepositConfirmRejected, message: "不在规范链上",
			chain: func(e *confirmEnv, s depositSpec) { e.chain.canonical[s.block] = otherHash },
		},
		{
			name: "node cannot return the canonical block", status: DepositConfirmError, message: "链上复核失败",
			chain: func(e *confirmEnv, s depositSpec) { delete(e.chain.canonical, s.block) },
		},
		{
			name: "stored amount differs", status: DepositConfirmRejected, message: "唯一 Transfer",
			row: func(s *depositSpec) { s.amount = "2.000000000000000000" },
		},
		{
			name: "stored recipient differs", status: DepositConfirmRejected, message: "唯一 Transfer",
			row: func(s *depositSpec) { s.to = confirmStranger },
		},
		{
			name: "stored sender differs", status: DepositConfirmRejected, message: "唯一 Transfer",
			row: func(s *depositSpec) { s.from = confirmStranger },
		},
		{
			name: "stored token differs", status: DepositConfirmRejected, message: "唯一 Transfer",
			row: func(s *depositSpec) { s.token = confirmUSDC },
		},
		{
			name: "stored block number differs", status: DepositConfirmRejected, message: "换了区块",
			row: func(s *depositSpec) { s.block++ },
		},
		{
			name: "stored block hash differs", status: DepositConfirmRejected, message: "换了区块",
			row: func(s *depositSpec) { s.blockHash = otherHash },
		},
		{
			name: "stored log index differs", status: DepositConfirmRejected, message: "换了区块",
			row: func(s *depositSpec) { s.index = 8 },
		},
		{
			name: "two identical logs make a relocation ambiguous", status: DepositConfirmRejected, message: "唯一 Transfer",
			row: func(s *depositSpec) { s.index = 9 },
			chain: func(e *confirmEnv, s depositSpec) {
				r := e.chain.receipts[s.tx]
				r.logs = append(r.logs, chainTransferLog(s.token, s.from, s.to, tenUSDT, s.block, s.blockHash, s.tx, 8))
			},
		},
		{
			name: "receipt request fails", status: DepositConfirmError, message: "status 503",
			chain: func(e *confirmEnv, s depositSpec) { e.chain.httpFail["eth_getTransactionReceipt"] = 503 },
		},
		{
			name: "node is on another chain", status: DepositConfirmError, message: "没有通过链 ID 校验的 RPC 节点", noReceipt: true,
			chain: func(e *confirmEnv, s depositSpec) { e.chain.chainID = "0x1" },
		},
		{
			name: "amount has more precision than the token", status: DepositConfirmRejected, message: "精度异常", noReceipt: true,
			row: func(s *depositSpec) { s.amount = "10.0000000000000000001" },
		},
		{
			name: "zero amount", status: DepositConfirmRejected, message: "精度异常", noReceipt: true,
			row: func(s *depositSpec) { s.amount = "0" },
		},
		{
			name: "unparsable amount", status: DepositConfirmRejected, message: "精度异常", noReceipt: true,
			row: func(s *depositSpec) { s.amount = "abc" },
		},
		{
			name: "token is not in the trusted list", status: DepositConfirmRejected, message: "不在该链的受信代币清单", untouchedChain: true,
			row: func(s *depositSpec) { s.token = "0x" + strings.Repeat("ab", 20) },
		},
		{
			name: "chain is not EVM", status: DepositConfirmRejected, message: "只支持 EVM", untouchedChain: true,
			row: func(s *depositSpec) { s.chain = "solana" },
		},
		{
			name: "no stored block hash", status: DepositConfirmRejected, message: "缺少区块号或区块哈希", untouchedChain: true,
			row: func(s *depositSpec) { s.blockHash = "" },
		},
		{
			name: "no stored block number", status: DepositConfirmRejected, message: "缺少区块号或区块哈希", untouchedChain: true,
			row: func(s *depositSpec) { s.block = 0 },
		},
		{
			name: "tx hash is not canonical lowercase", status: DepositConfirmRejected, message: "不是规范的小写", untouchedChain: true,
			row: func(s *depositSpec) { s.tx = "0x" + strings.ToUpper(s.tx[2:]) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newConfirmEnv(t)
			s := e.spec(1)
			e.publish(s)
			if tc.chain != nil {
				tc.chain(e, s)
			}
			if tc.row != nil {
				tc.row(&s)
			}
			id := e.insert(t, s)
			before := e.snapshot(t)

			result := e.confirm(id)
			require.Equal(t, tc.status, result.Status, result.Message)
			require.Contains(t, result.Message, tc.message)
			require.Nil(t, result.OrderID)
			require.Equal(t, before, e.snapshot(t), "a refused confirmation must leave the database untouched")
			require.Zero(t, e.processor.count(), "the ledger must not be reached")
			if tc.untouchedChain {
				require.Zero(t, e.chain.totalCalls())
			}
			if tc.noReceipt || tc.untouchedChain {
				require.Zero(t, e.chain.callCount("eth_getTransactionReceipt"))
			}
		})
	}
}

func TestManualConfirmNeverReachesChainOrLedgerForRowsThatAreNotPending(t *testing.T) {
	for _, tc := range []struct{ status, reason, message string }{
		{"ignored", "below_min", "低于最小入账金额"},
		{"ignored", "address_unassigned", "没有分配给用户"},
		{"reverted", "reorg_noncanonical_block", "链重组使记录失效"},
		{"reverted", "reorg_duplicate_credit_manual_review", "需要人工核对"},
	} {
		t.Run(tc.status+"/"+tc.reason, func(t *testing.T) {
			e := newConfirmEnv(t)
			s := e.spec(1)
			s.status, s.reason = tc.status, tc.reason
			e.publish(s)
			id := e.insert(t, s)
			before := e.snapshot(t)

			result := e.confirm(id)
			require.Equal(t, DepositConfirmRejected, result.Status, result.Message)
			require.Contains(t, result.Message, tc.status)
			require.Contains(t, result.Message, tc.message)
			require.Equal(t, before, e.snapshot(t))
			require.Zero(t, e.chain.totalCalls())
			require.Zero(t, e.processor.count())
		})
	}
}

func TestManualConfirmReportsAlreadyCreditedRowsWithTheirOrderAndMissingRowsAsRejected(t *testing.T) {
	e := newConfirmEnv(t)
	s := e.spec(1)
	s.status = "credited"
	id := e.insert(t, s)
	_, err := e.db.ExecContext(t.Context(), `UPDATE deposit_transactions SET order_id = 42 WHERE id = $1`, id)
	require.NoError(t, err)

	done := e.confirm(id)
	require.Equal(t, DepositConfirmAlreadyCredited, done.Status, done.Message)
	require.NotNil(t, done.OrderID)
	require.EqualValues(t, 42, *done.OrderID)

	missing := e.confirm(id + 1000)
	require.Equal(t, DepositConfirmRejected, missing.Status)
	require.Contains(t, missing.Message, "记录不存在")
	require.Zero(t, e.chain.totalCalls())
	require.Zero(t, e.processor.count())
}
