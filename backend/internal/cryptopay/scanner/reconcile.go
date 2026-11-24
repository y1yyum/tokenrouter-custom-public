package scanner

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/cryptopay/mode"
	"github.com/TokenFlux/TokenRouter/internal/cryptopay/rpc"
	"github.com/lib/pq"
)

const missingReceiptPrefix = "reorg_missing_receipt:"
const missingReceiptThreshold = 3
const reconcileRPCBudget = 2 * time.Second
const reconcilePersistenceReserve = 500 * time.Millisecond

// A pending observation whose chain or token left the configuration can never
// be verified again: no pool would answer for it and no decimals would make its
// amount meaningful. Rotating its attempt clock forever hides a real deposit
// from operators, so it is moved once to the terminal dead_letter status with a
// durable reason instead of being silently re-queued.
const (
	deadLetterChainRemoved = "dead_letter:chain_no_longer_configured"
	deadLetterTokenRemoved = "dead_letter:token_no_longer_watched"
)

// DepositReorg carries the exact observation invalidated by a scanner pass.
type DepositReorg struct {
	Chain       string // original network key
	TxHash      string // original transaction hash
	LogIndex    int    // original event position
	BlockHash   string // invalidated inclusion hash
	BlockNumber uint64 // invalidated inclusion height
	Reason      string // chain verification outcome
}

// DepositReorgOutcome describes the database decision for an invalidated event.
type DepositReorgOutcome struct {
	Handled      bool // terminal state is already stored
	Revoked      bool // this call recorded a reversal
	ManualReview bool // this call queued operator review
}

type depositObservation struct {
	chain, txHash, from, to, token, amount, blockHash string
	logIndex                                          int
	blockNumber                                       uint64
	reason, status                                    string
	// provisional marks a granted row that is still inside the 0-confirmation
	// risk window: it is finalized once finality is reached and clawed back if
	// the chain invalidates it.
	provisional bool
}

// Close the result set before RPC/UPDATE work, including with a one-connection
// pool. Rotation uses the persisted check clock and survives restarts.
func readObservations(rows *sql.Rows, status string) ([]depositObservation, error) {
	var out []depositObservation
	for rows.Next() {
		p := depositObservation{status: status}
		if err := rows.Scan(&p.chain, &p.txHash, &p.logIndex, &p.from, &p.to, &p.token,
			&p.amount, &p.blockNumber, &p.blockHash, &p.reason, &p.provisional); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

// recheckCredited rotates through the risk window instead of selecting the
// latest 200 forever. Suspected missing receipts stay eligible past the window
// until resolved. The window starts at order completion, since pending can
// last longer than an hour. Legacy rows fall back to observation creation;
// last_attempt_at only rotates the queue and never renews the window.
// These updates never change balances or fulfillment orders.
func (w *Worker) recheckCredited(ctx context.Context, scope ...string) error {
	if err := mode.CheckWritable(); err != nil {
		return err
	}
	filter := ""
	var args []any
	if len(scope) > 0 {
		filter = " AND d.chain = ANY($1)"
		args = append(args, pq.Array(scope))
	}
	rows, err := w.db.QueryContext(ctx, `
		SELECT d.chain, d.tx_hash, d.log_index, d.from_address, d.to_address, d.token,
		       d.amount::text, d.block_number, d.block_hash, d.reason, d.provisional
		  FROM deposit_transactions d
		  LEFT JOIN payment_orders o ON o.id = d.order_id
		 WHERE d.status = 'credited'
		   AND (COALESCE(o.completed_at, d.created_at) > NOW() - INTERVAL '1 hour'
		        OR d.reason LIKE 'reorg_missing_receipt:%'
		        OR d.provisional)`+filter+`
		 ORDER BY COALESCE(d.last_attempt_at, d.created_at), d.id
		 LIMIT 200`, args...)
	if err != nil {
		return err
	}
	observations, err := readObservations(rows, "credited")
	if err != nil {
		return err
	}
	return w.reconcile(ctx, observations)
}

// retryPending uses the same receipt, canonical-log and finality checks as
// live scanning. Each attempted row rotates even if its RPC remains broken.
func (w *Worker) retryPending(ctx context.Context, scope ...string) error {
	if err := mode.CheckWritable(); err != nil {
		return err
	}
	filter := ""
	var args []any
	if len(scope) > 0 {
		filter = " AND chain = ANY($1)"
		args = append(args, pq.Array(scope))
	}
	rows, err := w.db.QueryContext(ctx, `
		SELECT chain, tx_hash, log_index, from_address, to_address, token,
		       amount::text, block_number, block_hash, reason, provisional
		  FROM deposit_transactions
		 WHERE status = 'pending'
		   AND (last_attempt_at IS NULL OR last_attempt_at < NOW() -
		       CASE WHEN chain = 'bsc' THEN INTERVAL '5 seconds' ELSE INTERVAL '1 minute' END)`+filter+`
		 ORDER BY COALESCE(last_attempt_at, created_at), id
		 LIMIT 50`, args...)
	if err != nil {
		return err
	}
	observations, err := readObservations(rows, "pending")
	if err != nil {
		return err
	}
	return w.reconcile(ctx, observations)
}

func (w *Worker) chainConfig(chain string) (ChainConfig, bool) {
	for _, c := range w.chains {
		if c.Chain == chain {
			return c, true
		}
	}
	return ChainConfig{}, false
}

func findReincludedTransfer(receipt *ReceiptCheck, observation depositObservation, decimals int) (Transfer, error) {
	if receipt == nil || receipt.Status != 1 || len(receipt.Logs) == 0 {
		return Transfer{}, ErrTransferMismatch
	}
	want, err := observation.transfer(decimals)
	if err != nil {
		return Transfer{}, err
	}
	var match *Transfer
	for _, log := range receipt.Logs {
		if log.Removed || len(log.Topics) != 3 || !strings.EqualFold(log.Address, want.Token) ||
			!strings.EqualFold(log.Topics[0], transferTopic) || !validHex(log.Topics[1], 32) || !validHex(log.Topics[2], 32) ||
			!strings.EqualFold(log.Topics[1], padTopic(want.From)) || !strings.EqualFold(log.Topics[2], padTopic(want.To)) || !validHex(log.Data, 32) {
			continue
		}
		amount, ok := new(big.Int).SetString(log.Data[2:], 16)
		if !ok || amount.Cmp(want.Amount) != 0 {
			continue
		}
		index, indexErr := hexQuantity(log.LogIndex)
		block, blockErr := hexQuantity(log.BlockNumber)
		if indexErr != nil || blockErr != nil || block != receipt.BlockNumber ||
			!strings.EqualFold(log.BlockHash, receipt.BlockHash) || !strings.EqualFold(log.TxHash, receipt.TxHash) {
			continue
		}
		candidate := Transfer{Chain: observation.chain, TxHash: strings.ToLower(receipt.TxHash), LogIndex: int(index),
			From: want.From, To: want.To, Token: want.Token, Amount: new(big.Int).Set(want.Amount),
			BlockNumber: receipt.BlockNumber, BlockHash: receipt.BlockHash}
		if match != nil {
			return Transfer{}, ErrTransferMismatch
		}
		match = &candidate
	}
	if match == nil {
		return Transfer{}, ErrTransferMismatch
	}
	return *match, nil
}

// updateReincludedObservation moves a pending event only while it still has
// neither a grant nor an order. The compare-and-set also protects the equal-log
// case, where only the block changes but the merchant number stays the same.
func (w *Worker) updateReincludedObservation(ctx context.Context, old depositObservation, transfer Transfer) (bool, error) {
	result, err := w.db.ExecContext(ctx, `UPDATE deposit_transactions
		SET log_index = $1, block_number = $2, block_hash = $3, last_attempt_at = NOW()
		WHERE chain = $4 AND tx_hash = $5 AND log_index = $6 AND status = 'pending' AND reason = $7
		  AND order_id IS NULL AND credited_at IS NULL AND revoked_at IS NULL AND NOT provisional
		  AND NOT EXISTS (SELECT 1 FROM payment_orders WHERE out_trade_no = $8)`,
		transfer.LogIndex, transfer.BlockNumber, transfer.BlockHash, old.chain, old.txHash, old.logIndex,
		old.reason, depositOrderNo(old.chain, old.txHash, old.logIndex))
	if err != nil {
		return false, fmt.Errorf("move pending observation to reincluded log: %w", err)
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

// hasPendingOrder includes a lost link so receipt absence or relocation never
// discards a partially settled deposit as an ordinary unpaid observation.
func (w *Worker) hasPendingOrder(ctx context.Context, p depositObservation) (bool, error) {
	var exists bool
	err := w.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM deposit_transactions
		WHERE chain = $1 AND tx_hash = $2 AND log_index = $3
		  AND (order_id IS NOT NULL OR credited_at IS NOT NULL OR provisional))
		OR EXISTS (SELECT 1 FROM payment_orders WHERE out_trade_no = $4)`,
		p.chain, p.txHash, p.logIndex, depositOrderNo(p.chain, p.txHash, p.logIndex)).Scan(&exists)
	return exists, err
}

func depositOrderNo(chain, txHash string, logIndex int) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d", chain, strings.ToLower(txHash), logIndex)))
	return "crypto-" + hex.EncodeToString(digest[:20])
}

func watchedDecimals(chain ChainConfig, token string) (int, bool) {
	watched := false
	for _, contract := range chain.Tokens {
		if strings.EqualFold(contract, token) {
			watched = true
			break
		}
	}
	if !watched {
		return 0, false
	}
	for contract, decimals := range chain.Decimals {
		if strings.EqualFold(contract, token) && decimals >= 0 && decimals <= 255 {
			return decimals, true
		}
	}
	return 0, false
}

// transferDecimals resolves the exponent of any observed transfer. The native
// coin is not in the token contract list (it has no contract), so it is
// answered from the chain's own native settings; a chain that does not scan
// native coin reports it unwatched instead of guessing 18.
func transferDecimals(chain ChainConfig, token string) (int, bool) {
	if strings.EqualFold(token, NativeToken) {
		if !chain.Native || chain.NativeDecimals <= 0 || chain.NativeDecimals > 255 {
			return 0, false
		}
		return chain.NativeDecimals, true
	}
	return watchedDecimals(chain, token)
}

func (p depositObservation) transfer(decimals int) (Transfer, error) {
	raw, err := decimalToRawUnits(p.amount, decimals)
	if err != nil {
		return Transfer{}, err
	}
	return Transfer{Chain: p.chain, TxHash: p.txHash, LogIndex: p.logIndex,
		From: p.from, To: p.to, Token: p.token, Amount: raw,
		Native:      strings.EqualFold(p.token, NativeToken),
		BlockNumber: p.blockNumber, BlockHash: p.blockHash}, nil
}

func (w *Worker) reconcile(ctx context.Context, observations []depositObservation) error {
	if err := mode.CheckWritable(); err != nil {
		return err
	}
	// A backlog of slow receipt/reorg checks must not monopolize the shared
	// scanner lock and keep newly mined deposits waiting. Completed attempts
	// rotate durably; the remaining rows get the next bounded pass.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var errs []error
	type poolCheck struct {
		pool *rpc.Pool
		err  error
	}
	verified := make(map[string]poolCheck)
	for _, p := range observations {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		pool := w.pools[p.chain]
		chain, configured := w.chainConfig(p.chain)
		decimals, watched := transferDecimals(chain, p.token)
		if !configured || !watched {
			// Disabled chains/tokens can otherwise occupy the first page
			// forever and starve every configured chain behind them.
			if p.status == "pending" {
				reason := deadLetterTokenRemoved
				if !configured {
					reason = deadLetterChainRemoved
				}
				changed, err := w.deadLetterObservation(ctx, p, reason)
				if err != nil {
					errs = append(errs, err)
				} else if changed {
					// One error per row, at the transition: the durable reason is
					// what an operator acts on, the log is what makes it visible.
					slog.Error("[cryptopay] pending deposit dead-lettered: chain or token no longer configured",
						"chain", p.chain, "tx", p.txHash, "log_index", p.logIndex, "reason", reason)
				}
				continue
			}
			if _, err := w.updateObservation(ctx, p, p.status, p.reason); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if pool == nil {
			// The chain is still configured; a missing RPC pool (no endpoint, a
			// failed reload) is an operator problem that can be fixed, not the
			// permanent loss of verification that dead_letter records.
			slog.Warn("[cryptopay] deposit recheck skipped: no RPC pool", "chain", p.chain, "tx", p.txHash)
			if _, err := w.updateObservation(ctx, p, p.status, p.reason); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		// A slow receipt must leave time to persist this row's check clock.
		// Using the same expiring context for RPC and UPDATE otherwise leaves
		// the oldest row first forever, starving every pending row behind it.
		deadline, _ := ctx.Deadline() // The pass always has a ten-second cap.
		if time.Until(deadline) <= reconcilePersistenceReserve {
			return errors.Join(append(errs, context.DeadlineExceeded)...)
		}
		reason, status := p.reason, p.status
		reincluded := false
		// An RPC failure breaks a sequence of confirmed absences; it is never
		// evidence of a reorg. Successful observations clear the suspicion too.
		if strings.HasPrefix(reason, missingReceiptPrefix) {
			reason = ""
		}
		check, checked := verified[p.chain]
		if !checked {
			// Chain probing isolates stalled endpoints for up to three seconds.
			// Keep that once-per-chain budget separate from receipt work, or a
			// two-second parent would discard healthy results with a slow peer.
			poolCtx, cancelPool := context.WithDeadline(ctx, deadline.Add(-reconcilePersistenceReserve))
			check.pool, check.err = verifiedChainPool(poolCtx, pool, p.chain)
			cancelPool()
			// A per-row timeout is inconclusive. The next row gets a fresh
			// attempt rather than inheriting an exhausted verification budget.
			if !errors.Is(check.err, context.DeadlineExceeded) && !errors.Is(check.err, context.Canceled) {
				verified[p.chain] = check
			}
		}
		budget := min(reconcileRPCBudget, time.Until(deadline)-reconcilePersistenceReserve)
		rpcCtx, cancelObservation := context.WithTimeout(ctx, max(0, budget))
		pool, verifyErr := check.pool, check.err
		if verifyErr == nil {
			receipt, receiptErr := VerifyTransaction(rpcCtx, pool, p.txHash)
			switch {
			case errors.Is(receiptErr, ErrReceiptNotFound):
				if p.status == "pending" {
					// Keep ordinary unseen deposits pending. An order or grant
					// already exists for interrupted settlement, so require the
					// same repeated absence evidence as for a credited deposit.
					hasOrder, err := w.hasPendingOrder(ctx, p)
					if err != nil {
						verifyErr = err
						break
					}
					if !hasOrder {
						status, reason = "pending", ""
						break
					}
				}
				count := 0
				if strings.HasPrefix(p.reason, missingReceiptPrefix) {
					count, _ = strconv.Atoi(strings.TrimPrefix(p.reason, missingReceiptPrefix))
				}
				count++
				if count >= missingReceiptThreshold {
					status, reason = "reverted", "reorg_receipt_missing"
				} else {
					reason = missingReceiptPrefix + strconv.Itoa(count)
				}
			case receiptErr != nil && !errors.Is(receiptErr, ErrTransactionReverted):
				verifyErr = receiptErr
			default:
				verifyErr = verifyCanonicalBlock(rpcCtx, pool, receipt)
				if errors.Is(verifyErr, ErrNonCanonicalBlock) {
					status, reason, verifyErr = "reverted", "reorg_noncanonical_block", nil
				} else if verifyErr == nil && errors.Is(receiptErr, ErrTransactionReverted) {
					status, reason = "reverted", "reorg_receipt_reverted"
				} else if verifyErr == nil {
					transfer, err := p.transfer(decimals)
					verifyErr = err
					if verifyErr == nil {
						if transfer.Native {
							// The transaction itself is the evidence; a native
							// transfer has no log to relocate, so a mismatch
							// means the row no longer describes this chain.
							verifyErr = verifyNativeTransfer(rpcCtx, pool, transfer, receipt)
						} else {
							verifyErr = verifyReceiptTransfer(transfer, receipt)
						}
					}
					if errors.Is(verifyErr, ErrTransferMismatch) {
						status, reason, verifyErr = "reverted", "reorg_transfer_changed", nil
						if p.status == "pending" && !transfer.Native {
							hasOrder, err := w.hasPendingOrder(ctx, p)
							if err != nil {
								verifyErr = err
							} else if !hasOrder {
								transfer, err = findReincludedTransfer(receipt, p, decimals)
								switch {
								case err == nil:
									status, reason, reincluded = "pending", "", true
								case !errors.Is(err, ErrTransferMismatch):
									verifyErr = err
								}
							}
						}
					}
					if verifyErr == nil && status == "credited" && p.provisional {
						// A provisional credit becomes final once the chain
						// reaches final depth. No balance moves here: the amount
						// already landed at detection time and finalization only
						// ends the risk-window recheck.
						if final, finalErr := transactionIsFinalForChain(rpcCtx, pool, chain.Chain, receipt.BlockNumber, chain.Confirmations); finalErr == nil && final {
							w.finalizeProvisional(ctx, chain, p)
						}
					}
					if verifyErr == nil && status == "pending" && p.status == "pending" {
						final, err := transactionIsFinalForChain(rpcCtx, pool, chain.Chain, receipt.BlockNumber, chain.Confirmations)
						verifyErr = err
						if err == nil && final && !w.securityCovered(chain.Chain, transfer.BlockNumber) {
							// Head observation records pending rows before any security
							// range reached their block. Final alone is not enough: wait
							// until the cursor or a successful security range covers it.
							slog.Debug("[cryptopay] pending credit waits for security scan", "chain", chain.Chain, "tx", p.txHash, "block", transfer.BlockNumber)
							final = false
						}
						if err == nil && final {
							cancelObservation()
							var changed bool
							if reincluded {
								changed, err = w.updateReincludedObservation(ctx, p, transfer)
							} else {
								// Persist the attempt before fulfillment. Failure keeps
								// the deposit retryable and must never be hidden.
								changed, err = w.updateObservation(ctx, p, status, reason)
							}
							if err != nil {
								errs = append(errs, err)
								continue
							}
							if !changed {
								continue
							}
							credited, err := w.handler.Process(ctx, transfer, decimals)
							if err != nil {
								errs = append(errs, err)
							} else if credited {
								w.markCreditedConfirmations(ctx, chain, transfer)
							}
							continue
						}
						if err == nil && !final && !reincluded && w.securityCovered(chain.Chain, transfer.BlockNumber) {
							// Instant credit: verified on the canonical chain but
							// not final yet. The ledger credits now (provisional)
							// or records pending when the switch/floor says to
							// wait; either way this row stays durable.
							cancelObservation()
							credited, handled, creditErr := w.creditProvisional(ctx, chain, transfer, decimals)
							if creditErr != nil {
								errs = append(errs, fmt.Errorf("provisional credit %s/%s/%d: %w", p.chain, p.txHash, p.logIndex, creditErr))
								continue
							}
							if credited {
								w.markCreditedConfirmations(ctx, chain, transfer)
								continue
							}
							if handled {
								// Not eligible or already recorded as pending by
								// the ledger; the normal path below rotates the
								// retry clock.
								_ = handled
							}
						}
					}
				}
			}
		}
		// A pending row can already have a wallet receipt. Resolve the chain
		// loss under the same row lock as wallet delivery before publishing
		// any terminal status or notifying the operator.
		if verifyErr == nil && status == "reverted" && w.resolveDepositReorg != nil {
			cancelObservation()
			outcome, err := w.resolveDepositReorg(ctx, DepositReorg{
				Chain: p.chain, TxHash: p.txHash, LogIndex: p.logIndex,
				BlockHash: p.blockHash, BlockNumber: p.blockNumber, Reason: reason,
			})
			if err != nil {
				errs = append(errs, fmt.Errorf("resolve reorg %s/%s/%d: %w", p.chain, p.txHash, p.logIndex, err))
				continue
			}
			if outcome.Handled {
				if outcome.ManualReview && w.onReorg != nil {
					w.onReorg(p.chain, p.txHash)
				}
				continue
			}
		}
		cancelObservation()
		if verifyErr != nil {
			status, reason = p.status, p.reason
			if strings.HasPrefix(p.reason, missingReceiptPrefix) {
				reason = missingReceiptPrefix + "0"
			}
			errs = append(errs, fmt.Errorf("recheck %s/%s/%d: %w", p.chain, p.txHash, p.logIndex, verifyErr))
		}
		changed, err := w.updateObservation(ctx, p, status, reason)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if changed && status == "pending" && w.liveProgress {
			if head, ok := w.passHead(p.chain); ok {
				if err := w.updatePendingConfirmations(ctx, p.chain, p.txHash, p.logIndex,
					confirmationsAt(head, p.blockNumber, chain.Confirmations)); err != nil {
					errs = append(errs, err)
				}
			}
		}
		// Persist first, notify once on the guarded credited -> reverted
		// transition. No refund or debit is attempted here.
		if changed && p.status == "credited" && status == "reverted" && w.onReorg != nil {
			w.onReorg(p.chain, p.txHash)
		}
	}
	return errors.Join(errs...)
}

func (w *Worker) updateObservation(ctx context.Context, p depositObservation, status, reason string) (bool, error) {
	result, err := w.db.ExecContext(ctx, `UPDATE deposit_transactions
		SET status = $1, reason = $2, last_attempt_at = NOW()
		WHERE chain = $3 AND tx_hash = $4 AND log_index = $5
		  AND status = $6 AND reason = $7`,
		status, reason, p.chain, p.txHash, p.logIndex, p.status, p.reason)
	if err != nil {
		return false, fmt.Errorf("persist deposit recheck: %w", err)
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}

// deadLetterObservation concludes a pending observation that can no longer be
// verified. The compare-and-set matches updateObservation, so a concurrent
// credit or a newly re-added chain keeps its row and exactly one pass records
// the terminal transition.
func (w *Worker) deadLetterObservation(ctx context.Context, p depositObservation, reason string) (bool, error) {
	result, err := w.db.ExecContext(ctx, `UPDATE deposit_transactions
		SET status = 'dead_letter', reason = $1, last_attempt_at = NOW()
		WHERE chain = $2 AND tx_hash = $3 AND log_index = $4
		  AND status = 'pending' AND reason = $5`,
		reason, p.chain, p.txHash, p.logIndex, p.reason)
	if err != nil {
		return false, fmt.Errorf("dead-letter deposit observation: %w", err)
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}
