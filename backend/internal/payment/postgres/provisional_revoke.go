package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	dbent "github.com/TokenFlux/TokenRouter/ent"
	"github.com/TokenFlux/TokenRouter/ent/paymentauditlog"
	"github.com/TokenFlux/TokenRouter/ent/paymentorder"
	"github.com/TokenFlux/TokenRouter/ent/redeemcode"
	"github.com/TokenFlux/TokenRouter/internal/billing"
	"github.com/TokenFlux/TokenRouter/internal/payment"
)

// provisional_revoke.go resolves invalidated chain events by inspecting the
// original grant under the same lock that billing holds while moving funds.
// A pending order can exist before or after wallet delivery. The credential
// and its usage receipt distinguish those cases; the recorded credit time and
// provisional flag determine whether an evidenced grant may be reversed.
//
// For a proven provisional credit, the conditional event claim, deduction,
// negative history receipt and payment audit commit together. Missing or
// conflicting evidence goes to the existing manual queue. A terminal status
// prevents a later balance grant from the same event.

const (
	// These labels identify the reversal in audits, wallet history and the
	// existing dead-letter queue used for deposits that need operator review.
	auditDepositProvisionalRevoked = "DEPOSIT_PROVISIONAL_REVOKED"
	redeemTypeCryptoRevoke         = "crypto_revoke"
	provisionalRevokeLedgerNotes   = "链上充值未最终确认已回滚"
	depositCreditEvidenceReason    = "dead_letter:credit_evidence"
)

// ProvisionalDepositRevocation identifies an event the scanner has invalidated.
type ProvisionalDepositRevocation struct {
	Chain        string // deposit network key
	TxHash       string // original transaction hash
	LogIndex     int    // event position in block
	BlockHash    string // observed block hash
	BlockNumber  uint64 // observed block height
	RevokeReason string // scanner determination code
	OutTradeNo   string // deterministic recovery identity
	LegacyCode   string // historical order credential
}

// ProvisionalRevocationResult tells the caller whether database resolution
// finished and whether it owes a user or operator notification.
type ProvisionalRevocationResult struct {
	Handled           bool    // terminal result already saved
	Revoked           bool    // this call deducted the original amount
	ManualReview      bool    // this call queued operator review
	UserID            int64   // original deposit recipient
	Amount            float64 // recovered wallet amount
	BalanceAfter      float64 // resulting wallet balance
	BalanceAfterKnown bool    // this attempt deducted funds
	Negative          bool    // resulting wallet deficit
	LedgerID          int64   // negative history row identity
}

// cryptoReorgDeposit retains the observed event and its settlement markers.
type cryptoReorgDeposit struct {
	id          int64         // persisted deposit identity
	orderID     sql.NullInt64 // original order link
	status      string        // current deposit status
	provisional bool          // grant remains exposed to reorg
	creditedAt  sql.NullTime  // wallet grant was recorded
	revokedAt   sql.NullTime  // reversal already committed
	from        string        // recorded transfer sender
	to          string        // recorded transfer recipient
	token       string        // recorded chain asset
	amount      string        // recorded amount in token units
	blockNumber int64         // recorded block height
	blockHash   string        // recorded block identity
}

// RevokeProvisionalDepositCredit resolves an invalidated deposit under the
// same row lock acquired by the wallet grant. If reversal obtains the lock
// first, its terminal state prevents a later grant. If billing obtains it
// first, the usage receipt and risk marker are visible before classification.
//
// Pending and credited observations both use the original order. An unpaid
// pending order is closed without a wallet change. Uncertain or final grants
// are left for review and only an evidenced provisional grant is deducted.
func RevokeProvisionalDepositCredit(ctx context.Context, entClient *dbent.Client, in ProvisionalDepositRevocation) (*ProvisionalRevocationResult, error) {
	if entClient == nil {
		return nil, errors.New("provisional revocation: payment storage unavailable")
	}
	txHash := strings.ToLower(strings.TrimSpace(in.TxHash))
	chain := strings.TrimSpace(in.Chain)
	if chain == "" || txHash == "" || in.LogIndex < 0 || in.OutTradeNo == "" {
		return nil, errors.New("provisional revocation: invalid deposit identity")
	}
	reason := strings.TrimSpace(in.RevokeReason)
	if reason == "" {
		reason = "reorg"
	}

	tx, err := entClient.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("provisional revocation: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	txCtx := dbent.NewTxContext(ctx, tx)
	client := tx.Client()
	rows, err := client.QueryContext(txCtx, `SELECT id, order_id, status, provisional, credited_at, revoked_at,
		from_address, to_address, token, CAST(amount AS TEXT), block_number, block_hash
		FROM deposit_transactions WHERE chain = $1 AND tx_hash = $2 AND log_index = $3
		FOR UPDATE`, chain, txHash, in.LogIndex)
	if err != nil {
		return nil, fmt.Errorf("provisional revocation: lock deposit: %w", err)
	}
	if !rows.Next() {
		err := rows.Err()
		_ = rows.Close()
		return &ProvisionalRevocationResult{}, err
	}
	var deposit cryptoReorgDeposit
	err = rows.Scan(&deposit.id, &deposit.orderID, &deposit.status, &deposit.provisional, &deposit.creditedAt, &deposit.revokedAt,
		&deposit.from, &deposit.to, &deposit.token, &deposit.amount, &deposit.blockNumber, &deposit.blockHash)
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	// Receipt RPCs run before this transaction. An older reorg report
	// cannot resolve a deposit that has since moved to another block.
	if deposit.blockNumber < 0 || uint64(deposit.blockNumber) != in.BlockNumber || !strings.EqualFold(deposit.blockHash, in.BlockHash) {
		return &ProvisionalRevocationResult{Handled: true}, nil
	}
	if deposit.revokedAt.Valid || deposit.status == "dead_letter" ||
		(deposit.status == "reverted" && !deposit.provisional) {
		return &ProvisionalRevocationResult{Handled: true}, nil
	}
	if deposit.status != "pending" && deposit.status != "credited" && deposit.status != "reverted" {
		return &ProvisionalRevocationResult{}, nil
	}

	// Publish the terminal state under the held lock. A receipt found through
	// deterministic recovery can also restore the missing order link and time.
	resolve := func(status, why string, orderID int64, receiptTime any, revoked bool) error {
		_, err := client.ExecContext(txCtx, `UPDATE deposit_transactions
			SET status = $1, reason = $2, last_attempt_at = CURRENT_TIMESTAMP,
			    order_id = COALESCE(order_id, NULLIF(CAST($3 AS BIGINT), 0)),
			    credited_at = COALESCE(credited_at, $4),
			    revoked_at = CASE WHEN $5 THEN CURRENT_TIMESTAMP ELSE revoked_at END,
			    revoke_reason = CASE WHEN $5 THEN $2 ELSE revoke_reason END
			WHERE id = $6`, status, why, orderID, receiptTime, revoked, deposit.id)
		if err != nil {
			return fmt.Errorf("provisional revocation: resolve deposit: %w", err)
		}
		return tx.Commit()
	}
	review := func() (*ProvisionalRevocationResult, error) {
		err := resolve("dead_letter", depositCreditEvidenceReason+":"+reason, 0, nil, false)
		return &ProvisionalRevocationResult{Handled: true, ManualReview: true}, err
	}

	// A persisted link wins. Only historical rows missing that link may fall
	// back to the exact merchant number for the original event.
	var order *dbent.PaymentOrder
	if deposit.orderID.Valid {
		order, err = tx.PaymentOrder.Get(txCtx, deposit.orderID.Int64)
	} else {
		order, err = tx.PaymentOrder.Query().Where(paymentorder.OutTradeNoEQ(in.OutTradeNo)).Only(txCtx)
	}
	if dbent.IsNotFound(err) {
		if deposit.orderID.Valid || deposit.creditedAt.Valid || deposit.provisional || deposit.status != "pending" {
			return review()
		}
		err := resolve("reverted", reason, 0, nil, false)
		return &ProvisionalRevocationResult{Handled: true}, err
	}
	if err != nil {
		return nil, fmt.Errorf("provisional revocation: load original order: %w", err)
	}
	if !deposit.matchesOrder(order, in, chain, txHash) {
		return review()
	}
	receipt, err := readCryptoDepositGrant(txCtx, client, order)
	if errors.Is(err, ErrCryptoDepositEvidence) {
		return review()
	}
	if err != nil {
		return nil, fmt.Errorf("provisional revocation: read original grant: %w", err)
	}
	if !receipt.Valid {
		if deposit.creditedAt.Valid || deposit.provisional || deposit.status != "pending" || order.Status == payment.OrderStatusCompleted {
			return review()
		}
		err := resolve("reverted", reason, order.ID, nil, false)
		return &ProvisionalRevocationResult{Handled: true}, err
	}
	if !deposit.provisional {
		if !deposit.creditedAt.Valid && deposit.status == "pending" {
			return review()
		}
		err := resolve("reverted", reason, order.ID, receipt.Time, false)
		return &ProvisionalRevocationResult{Handled: true, ManualReview: true}, err
	}

	// The deposit marker normally commits with both reversal records. If
	// imported data contains either record without the marker, its effect
	// is uncertain and the row needs review before another deduction.
	sourceRef := provisionalRevokeSourceRef(deposit.id, chain, txHash, in.LogIndex)
	ledgerExists, err := tx.RedeemCode.Query().Where(redeemcode.TypeEQ(redeemTypeCryptoRevoke), redeemcode.SourceRefEQ(sourceRef)).Exist(txCtx)
	if err != nil {
		return nil, err
	}
	orderRef := strconv.FormatInt(order.ID, 10)
	auditExists, err := tx.PaymentAuditLog.Query().Where(
		paymentauditlog.OrderIDEQ(orderRef), paymentauditlog.ActionEQ(auditDepositProvisionalRevoked),
	).Exist(txCtx)
	if err != nil {
		return nil, err
	}
	if ledgerExists || auditExists {
		return review()
	}

	// Subtract the full credited amount so that spent funds leave a wallet
	// deficit, preserving the debt when the chain transfer disappears.
	deduct, err := client.ExecContext(txCtx, `UPDATE users
		    SET balance = balance - $1, updated_at = CURRENT_TIMESTAMP
		  WHERE id = $2 AND deleted_at IS NULL`, order.Amount, order.UserID)
	if err != nil {
		return nil, fmt.Errorf("provisional revocation: deduct balance: %w", err)
	}
	deducted, err := deduct.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("provisional revocation: deduct result: %w", err)
	}
	result := &ProvisionalRevocationResult{
		Handled: true, Revoked: deducted > 0, ManualReview: deducted == 0,
		UserID: order.UserID, Amount: order.Amount,
	}
	if deducted > 0 {
		var balance float64
		balanceRows, err := client.QueryContext(txCtx, `SELECT balance FROM users WHERE id = $1`, order.UserID)
		if err != nil {
			return nil, fmt.Errorf("provisional revocation: read balance: %w", err)
		}
		if balanceRows.Next() {
			err = balanceRows.Scan(&balance)
		}
		if scanErr := balanceRows.Err(); err == nil {
			err = scanErr
		}
		if closeErr := balanceRows.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return nil, fmt.Errorf("provisional revocation: read balance: %w", err)
		}
		result.BalanceAfter, result.BalanceAfterKnown, result.Negative = balance, true, balance < 0
	} else {
		// Deleted user: there is no balance left to claw back, but the
		// revocation stays durable for the audit trail.
		slog.Warn("[cryptopay] provisional credit revoked for a missing user; nothing to deduct",
			"chain", chain, "tx", txHash, "log_index", in.LogIndex, "user_id", order.UserID)
	}

	// The host history reads usage receipts, and its code schema requires a
	// unique nonempty code. Give the fully consumed reversal record an identity
	// derived from the event and persist its history receipt in this transaction.
	if deducted > 0 {
		now := time.Now()
		digest := sha256.Sum256([]byte(sourceRef))
		entry, err := tx.RedeemCode.Create().
			SetCode(fmt.Sprintf("revoke-%x", digest[:12])).
			SetType(redeemTypeCryptoRevoke).
			SetValue(-order.Amount).
			SetStatus(billing.StatusUsed).
			SetMaxUses(1).
			SetUsedCount(1).
			SetUsedBy(order.UserID).
			SetUsedAt(now).
			SetNotes(provisionalRevokeLedgerNotes).
			SetSourceRef(sourceRef).
			Save(txCtx)
		if err != nil {
			return nil, fmt.Errorf("provisional revocation: write balance ledger: %w", err)
		}
		if _, err := tx.RedeemCodeUsage.Create().
			SetRedeemCodeID(entry.ID).
			SetUserID(order.UserID).
			SetUsedAt(now).
			Save(txCtx); err != nil {
			return nil, fmt.Errorf("provisional revocation: write balance history receipt: %w", err)
		}
		result.LedgerID = entry.ID
	}

	// Attach the reason, buyer and resulting wallet balance to the order audit so
	// a reversal is distinguishable from an administrative payment refund.
	detail, err := json.Marshal(map[string]any{
		"chain":            chain,
		"tx_hash":          txHash,
		"log_index":        in.LogIndex,
		"user_id":          order.UserID,
		"amount":           order.Amount,
		"balance_after":    result.BalanceAfter,
		"balance_known":    result.BalanceAfterKnown,
		"negative_balance": result.Negative,
		"reason":           reason,
	})
	if err != nil {
		return nil, fmt.Errorf("provisional revocation: encode audit detail: %w", err)
	}
	if _, err := tx.PaymentAuditLog.Create().
		SetOrderID(orderRef).
		SetAction(auditDepositProvisionalRevoked).
		SetOperator("system").
		SetDetail(string(detail)).
		Save(txCtx); err != nil {
		return nil, fmt.Errorf("provisional revocation: write audit log: %w", err)
	}
	if err := resolve("reverted", reason, order.ID, receipt.Time, true); err != nil {
		return nil, err
	}
	return result, nil
}

// matchesOrder verifies the original event, sole-use credential and amount
// representation before an order can support a wallet deduction. The
// persisted deposit contains token units while the snapshot carries the
// integer raw amount and its decimal exponent.
func (d cryptoReorgDeposit) matchesOrder(order *dbent.PaymentOrder, in ProvisionalDepositRevocation, chain, txHash string) bool {
	if !payment.IsCryptoDepositOrder(OrderFromEntity(order)) || order.OrderType != payment.OrderTypeBalance ||
		order.OutTradeNo != in.OutTradeNo || order.UserID <= 0 || order.PaidAt == nil ||
		!payment.IsValidCreditedBalance(order.Amount) || !strings.EqualFold(order.PaymentTradeNo, txHash) ||
		!billing.IsOrderBalanceCredential(order.RechargeCode) ||
		(order.RechargeCode != in.LegacyCode && !strings.HasPrefix(order.RechargeCode, "cd-")) {
		return false
	}
	snapshot := order.ProviderSnapshot
	if !payment.CryptoDepositLogIndexMatches(snapshot["log_index"], in.LogIndex) ||
		d.blockNumber < 0 || !payment.CryptoDepositLogIndexMatches(snapshot["block"], int(d.blockNumber)) {
		return false
	}
	if blockHash := payment.PsSnapshotStringValue(snapshot["block_hash"]); blockHash != "" && !strings.EqualFold(blockHash, d.blockHash) {
		return false
	}
	for key, expected := range map[string]string{"chain": chain, "tx": txHash, "from": d.from, "to": d.to, "token": d.token} {
		if !strings.EqualFold(payment.PsSnapshotStringValue(snapshot[key]), expected) {
			return false
		}
	}
	scale, err := strconv.Atoi(fmt.Sprint(snapshot["decimals"]))
	if err != nil || scale < 0 || scale > 18 {
		return false
	}
	raw, err := decimal.NewFromString(payment.PsSnapshotStringValue(snapshot["raw_amount"]))
	if err != nil || raw.Sign() <= 0 || !raw.IsInteger() {
		return false
	}
	amount, err := decimal.NewFromString(d.amount)
	return err == nil && raw.Shift(-int32(scale)).Equal(amount)
}

// provisionalRevokeSourceRef gives the unique negative entry its original
// event identity, allowing audit lookup and protecting against duplicate rows.
func provisionalRevokeSourceRef(depositID int64, chain, txHash string, logIndex int) string {
	return fmt.Sprintf("crypto_deposit:%d:%s:%s:%d", depositID, chain, txHash, logIndex)
}
