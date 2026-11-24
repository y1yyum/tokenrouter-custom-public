package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	dbent "github.com/TokenFlux/TokenRouter/ent"
	"github.com/TokenFlux/TokenRouter/internal/billing"
	"github.com/TokenFlux/TokenRouter/internal/payment"
)

// crypto_deposit_grant.go joins the deposit record to the wallet transaction.
// A recovery attempt verifies the single-use credential and receipt before
// treating a paid order as delivered. The callback then commits the deposit
// timestamp and risk state alongside the wallet change and usage receipt.
// Reorg handling takes the same row lock, so it either closes an unpaid
// deposit first or sees the complete evidence for an already committed grant.

var (
	// ErrCryptoDepositEvidence indicates that the persisted order, redeem
	// credential or receipt cannot prove the original balance operation.
	ErrCryptoDepositEvidence = errors.New("crypto deposit grant evidence is inconsistent")
	// ErrCryptoDepositClosed indicates that the chain event has been resolved
	// or its saved identity no longer matches the verified observation.
	ErrCryptoDepositClosed = errors.New("crypto deposit is closed or has changed")
)

// CryptoDepositGrant binds a chain observation to the exact original order.
type CryptoDepositGrant struct {
	Chain       string // canonical network name
	TxHash      string // canonical transaction hash
	LogIndex    int    // event position in block
	BlockHash   string // last verified inclusion block
	Provisional bool   // inclusion has not passed finality
}

// cryptoDepositGrantKey carries the deposit identity into the rebate transaction.
type cryptoDepositGrantKey struct{}

// readCryptoDepositGrant checks whether a reserved credential has one
// matching receipt. The value comes from the same record whose locked value
// was applied by the wallet transaction. Unexpected or conflicting records
// require review instead of interpreting the order's PAID status as a grant.
func readCryptoDepositGrant(ctx context.Context, client *dbent.Client, order *dbent.PaymentOrder) (sql.NullTime, error) {
	rows, err := client.QueryContext(ctx, `
		SELECT c.type, c.value, c.status, c.max_uses, c.used_count, c.used_by, c.used_at,
		       (SELECT COUNT(*) FROM redeem_code_usages all_uses WHERE all_uses.redeem_code_id = c.id),
		       u.used_at
		FROM redeem_codes c LEFT JOIN redeem_code_usages u ON u.redeem_code_id = c.id AND u.user_id = $2
		WHERE c.code = $1`, order.RechargeCode, order.UserID)
	if err != nil {
		return sql.NullTime{}, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return sql.NullTime{}, err
		}
		// An interrupted legacy fulfillment could have committed the
		// grant before its credential and receipt were lost. Only a bare
		// PAID order or the atomic protocol establishes an unpaid state.
		if order.Status != payment.OrderStatusPaid && order.ProviderSnapshot["wallet_grant_atomic"] != true {
			return sql.NullTime{}, ErrCryptoDepositEvidence
		}
		return sql.NullTime{}, nil
	}
	var kind, status string
	var amount float64
	var maxUses, usedCount, receiptCount int
	var usedBy sql.NullInt64
	var usedAt, receipt sql.NullTime
	if err := rows.Scan(&kind, &amount, &status, &maxUses, &usedCount, &usedBy, &usedAt, &receiptCount, &receipt); err != nil {
		return sql.NullTime{}, err
	}
	if kind != billing.RedeemTypeBalance || amount != order.Amount || maxUses != 1 {
		return sql.NullTime{}, ErrCryptoDepositEvidence
	}
	if receiptCount == 0 && usedCount == 0 && !usedBy.Valid && !usedAt.Valid && status == billing.StatusUnused {
		return sql.NullTime{}, nil
	}
	if receiptCount != 1 || usedCount != 1 || !usedBy.Valid || usedBy.Int64 != order.UserID ||
		!usedAt.Valid || !receipt.Valid || !receipt.Time.Equal(usedAt.Time) || status != billing.StatusUsed {
		return sql.NullTime{}, ErrCryptoDepositEvidence
	}
	return receipt, nil
}

// PrepareCryptoDepositGrant checks persisted evidence under the deposit lock.
// For an existing grant it can repair its timestamp and lower its risk after
// a fresh finality check. A historical grant with no risk marker can resume
// automatically only after finality has been established for the same event.
//
// The returned context binds the same row to later redemption and rebate
// transactions. They recheck eligibility because each runs after this
// transaction releases the lock.
func PrepareCryptoDepositGrant(ctx context.Context, client *dbent.Client, order *dbent.PaymentOrder, grant CryptoDepositGrant) (context.Context, error) {
	tx, err := client.Tx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	txctx := dbent.NewTxContext(ctx, tx)
	db := tx.Client()
	rows, err := db.QueryContext(txctx, `
		UPDATE deposit_transactions SET order_id = $4
		WHERE chain = $1 AND tx_hash = $2 AND log_index = $3
		  AND (order_id IS NULL OR order_id = $4) AND status IN ('pending', 'credited')
		  AND LOWER(block_hash) = LOWER($5) AND revoked_at IS NULL
		RETURNING status, provisional, credited_at`, grant.Chain, grant.TxHash, grant.LogIndex, order.ID, grant.BlockHash)
	if err != nil {
		return nil, fmt.Errorf("link order: %w", err)
	}
	var status string
	var provisional bool
	var creditedAt sql.NullTime
	if !rows.Next() {
		err := rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, fmt.Errorf("link order: %w", err)
		}
		return nil, ErrCryptoDepositClosed
	}
	err = rows.Scan(&status, &provisional, &creditedAt)
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	receipt, err := readCryptoDepositGrant(txctx, db, order)
	if err != nil {
		return nil, err
	}
	if !receipt.Valid && (creditedAt.Valid || provisional || status == "credited" || order.Status == payment.OrderStatusCompleted) {
		return nil, ErrCryptoDepositEvidence
	}
	if receipt.Valid && !creditedAt.Valid && !provisional && grant.Provisional {
		return nil, ErrCryptoDepositEvidence
	}
	if receipt.Valid {
		if _, err := db.ExecContext(txctx, `
			UPDATE deposit_transactions
			SET credited_at = COALESCE(credited_at, $1), provisional = provisional AND $2
			WHERE chain = $3 AND tx_hash = $4 AND log_index = $5`,
			receipt.Time, grant.Provisional, grant.Chain, grant.TxHash, grant.LogIndex); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	ctx = context.WithValue(ctx, cryptoDepositGrantKey{}, grant)
	return billing.ContextAuthorizeOrderBalanceRedeem(ctx, order.RechargeCode, func(txctx context.Context, userID int64, amount float64) error {
		if userID != order.UserID || amount != order.Amount {
			return ErrCryptoDepositEvidence
		}
		return recordCryptoDepositGrant(txctx, order.ID, grant)
	}), nil
}

// recordCryptoDepositGrant takes the chain row lock before the wallet write.
// The timestamp and provisional marker commit only if the grant and the
// usage receipt both commit in billing's existing Ent transaction.
func recordCryptoDepositGrant(ctx context.Context, orderID int64, grant CryptoDepositGrant) error {
	tx := dbent.TxFromContext(ctx)
	if tx == nil {
		return errors.New("crypto deposit redemption transaction unavailable")
	}
	client := tx.Client()
	// Serialize competing inclusions even when their log indexes changed.
	// The event row lock alone cannot coordinate distinct ledger rows for
	// different versions of the same mined transaction.
	if _, err := client.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"cryptopay grant|"+grant.Chain+"|"+grant.TxHash); err != nil {
		return fmt.Errorf("lock deposit transaction identity: %w", err)
	}
	rows, err := client.QueryContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM deposit_transactions
		WHERE chain = $1 AND tx_hash IN ($2, UPPER($2)) AND log_index <> $3
		  AND LOWER(COALESCE(block_hash, '')) <> LOWER($4)
		  AND (credited_at IS NOT NULL OR order_id IS NOT NULL OR provisional
		       OR status = 'credited' OR reason LIKE 'dead_letter:credit_evidence%'))`,
		grant.Chain, grant.TxHash, grant.LogIndex, grant.BlockHash)
	if err != nil {
		return fmt.Errorf("check competing deposit inclusion: %w", err)
	}
	var competing bool
	if rows.Next() {
		err = rows.Scan(&competing)
	}
	if rowsErr := rows.Err(); err == nil {
		err = rowsErr
	}
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if competing {
		return ErrCryptoDepositEvidence
	}
	result, err := client.ExecContext(ctx, `
		UPDATE deposit_transactions
		   SET credited_at = CURRENT_TIMESTAMP, provisional = $1
		 WHERE chain = $2 AND tx_hash = $3 AND log_index = $4 AND order_id = $5
		   AND LOWER(block_hash) = LOWER($6)
		   AND status = 'pending' AND revoked_at IS NULL AND credited_at IS NULL`,
		grant.Provisional, grant.Chain, grant.TxHash, grant.LogIndex, orderID, grant.BlockHash)
	if err != nil {
		return fmt.Errorf("record deposit grant in wallet transaction: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return ErrCryptoDepositClosed
	}
	return nil
}

// lockCryptoDepositRebate prevents a delayed rebate from following a reversal.
// The row lock is retained by the order rebate transaction through its
// persistent deduplication claim and promotion quota writes.
func lockCryptoDepositRebate(ctx context.Context, client *dbent.Client, orderID int64) error {
	grant, ok := ctx.Value(cryptoDepositGrantKey{}).(CryptoDepositGrant)
	if !ok {
		return payment.ErrCryptoRetryRequiresLedger
	}
	rows, err := client.QueryContext(ctx, `
		SELECT credited_at FROM deposit_transactions
		WHERE chain = $1 AND tx_hash = $2 AND log_index = $3 AND order_id = $4
		  AND LOWER(block_hash) = LOWER($5) AND status IN ('pending', 'credited')
		  AND revoked_at IS NULL AND credited_at IS NOT NULL
		FOR UPDATE`, grant.Chain, grant.TxHash, grant.LogIndex, orderID, grant.BlockHash)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return ErrCryptoDepositClosed
	}
	var creditedAt time.Time
	return rows.Scan(&creditedAt)
}
