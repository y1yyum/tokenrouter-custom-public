package cryptopay

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	dbent "github.com/TokenFlux/TokenRouter/ent"
	"github.com/TokenFlux/TokenRouter/ent/paymentorder"
	"github.com/TokenFlux/TokenRouter/internal/billing"
	"github.com/TokenFlux/TokenRouter/internal/cryptopay/ledger"
	"github.com/TokenFlux/TokenRouter/internal/cryptopay/mode"
	"github.com/TokenFlux/TokenRouter/internal/cryptopay/scanner"
	"github.com/TokenFlux/TokenRouter/internal/notification"
	"github.com/TokenFlux/TokenRouter/internal/notification/bark"
	"github.com/TokenFlux/TokenRouter/internal/payment"
	paymentpostgres "github.com/TokenFlux/TokenRouter/internal/payment/postgres"
)

// creditor.go binds observed transfers to TokenRouter balance orders and
// forwards chain reorganizations to the transactional revocation store.
// The order number keeps the source event identity so a scanner that stopped
// after creating the order can find it again. The redeem credential for a new
// order is independent of the public transfer identity because the same
// billing service accepts credentials from users.
//
// Only the chain ledger calls this creditor after checking the transfer.
// The order must match the deposit and be linked before shared payment
// fulfillment may issue its balance and order rebate. Revocation publishes
// balance cache and Bark effects after its database transaction commits.

// DepositCreditor turns an observed deposit into a balance top-up by
// creating a PAID order and reusing the existing fulfillment pipeline.
// The deterministic order identity and existing fulfillment lease make
// interrupted and concurrent attempts safe to resume.
type DepositCreditor struct {
	entClient *dbent.Client                      // payment and user storage
	db        *sql.DB                            // chain ledger connection
	fulfill   func(context.Context, int64) error // authorized balance executor
	// invalidateBalance removes the cached balance after a committed
	// reversal. Deployments without a balance cache may leave it unset.
	invalidateBalance func(context.Context, int64) error
	// bark is configured before scanning starts. A nil service disables
	// user messages for both positive credit and provisional reversal.
	bark *notification.UserBarkService
}

// NewDepositCreditor binds the deposit database and the application's balance
// fulfillment service. The caller retains ownership of both connections and
// configures optional cache and notification callbacks before scanning starts.
func NewDepositCreditor(entClient *dbent.Client, fulfillment *payment.Fulfillment, db *sql.DB) *DepositCreditor {
	c := &DepositCreditor{entClient: entClient, db: db}
	if fulfillment != nil {
		c.fulfill = fulfillment.ExecuteCryptoDepositFulfillment
	}
	return c
}

// SetBalanceInvalidator installs the host's post-commit balance cache invalidation.
// Supply it before starting any scanner. A failure is logged and the committed
// reversal remains valid.
func (c *DepositCreditor) SetBalanceInvalidator(invalidate func(context.Context, int64) error) {
	c.invalidateBalance = invalidate
}

// SetBarkNotifier wires the per-user push channel used when a deposit is
// credited or reversed. Configure it before starting the scanner. A nil
// service disables messages, and delivery failures leave balances unchanged.
func (c *DepositCreditor) SetBarkNotifier(svc *notification.UserBarkService) {
	c.bark = svc
}

// CreditDeposit creates or recovers a paid balance order for a chain
// observation that the ledger has accepted. Each event gets a deterministic
// merchant number so concurrent inserts can reload the same persisted order.
// The winning order freezes the balance amount and, for a native transfer,
// its quote. A retry links that order before delegating balance fulfillment,
// retaining the original amount if configuration or a quote has changed.
func (c *DepositCreditor) CreditDeposit(ctx context.Context, req ledger.CreditRequest) error {
	if err := mode.CheckWritable(); err != nil {
		return err
	}
	if c == nil || c.entClient == nil || c.db == nil {
		return fmt.Errorf("cryptopay credit: payment or ledger storage unavailable")
	}
	req.TxHash = strings.ToLower(strings.TrimSpace(req.TxHash))
	if req.Chain == "" || req.TxHash == "" || req.UserID <= 0 || req.LogIndex < 0 || strings.TrimSpace(req.BlockHash) == "" {
		return fmt.Errorf("cryptopay credit: incomplete verified deposit identity")
	}
	if req.CreditUSDT <= 0 || math.IsNaN(req.CreditUSDT) || math.IsInf(req.CreditUSDT, 0) || req.Amount == nil || req.Amount.Sign() <= 0 || req.Decimals < 0 || req.Decimals > 18 {
		return fmt.Errorf("cryptopay credit: non-positive amount")
	}
	reminted, err := ledger.ReorgedTransferAlreadyCredited(ctx, c.db, req.Chain, req.TxHash,
		req.LogIndex, req.BlockHash)
	if err != nil {
		return err
	}
	if reminted {
		return ledger.ErrReorgedDuplicateTransfer
	}
	// Keep the source merchant identity per log for replay and the separate
	// transaction hash for explorer links. Legacy rows also used a derived
	// redeem credential, which recovery still recognizes.
	legacyCode, outTradeNo := depositOrderIdentity(req.Chain, req.TxHash, req.LogIndex)
	order, err := c.entClient.PaymentOrder.Query().Where(paymentorder.OutTradeNoEQ(outTradeNo)).Only(ctx)
	if err == nil {
		return c.resume(ctx, req, order, legacyCode)
	}
	if !dbent.IsNotFound(err) {
		return fmt.Errorf("cryptopay credit: find order: %w", err)
	}

	paidAmount := decimal.NewFromBigInt(req.Amount, -int32(req.Decimals))
	if req.Token == scanner.NativeToken {
		// New native order: the settlement basis is mandatory. A resumed order
		// never reaches this point — it short-circuits above on out_trade_no.
		price, err := decimal.NewFromString(req.NativePriceUSD)
		if err != nil || price.Sign() <= 0 || req.NativePriceAt.IsZero() {
			return fmt.Errorf("cryptopay credit: native deposit is missing its settlement price")
		}
	}
	// user_email/user_name are required columns (redundant copies so order
	// history survives user deletion); without them every insert fails and
	// the deposit loops in "pending" forever.
	user, err := c.entClient.User.Get(ctx, req.UserID)
	if err != nil {
		return fmt.Errorf("cryptopay credit: load user: %w", err)
	}
	// Some identity providers omit the username. Keep the required order
	// display snapshot present without modifying the corresponding user row.
	displayName := strings.TrimSpace(user.Username)
	if displayName == "" {
		displayName = fmt.Sprintf("user-%d", req.UserID)
	}
	paidAt := time.Now()

	// Freeze the source three-decimal amount before fulfillment. The same
	// order amount identifies its grant and any provisional reversal.
	creditAmount := decimal.NewFromFloat(req.CreditUSDT).Round(3).InexactFloat64()
	if !payment.IsValidCreditedBalance(creditAmount) {
		return fmt.Errorf("cryptopay credit: amount outside host balance range")
	}
	snapshot := map[string]any{
		"currency":            "USD",
		"provider_key":        payment.TypeCrypto,
		"raw_amount":          req.Amount.String(),
		"decimals":            req.Decimals,
		"log_index":           req.LogIndex,
		"chain":               req.Chain,
		"token":               req.Token,
		"from":                req.FromAddr,
		"to":                  req.ToAddr,
		"tx":                  req.TxHash,
		"block":               req.BlockNum,
		"block_hash":          req.BlockHash,
		"wallet_grant_atomic": true,
	}
	if req.Token == scanner.NativeToken {
		// Freeze the settlement basis in the order itself: "which price, and
		// from when" survives config changes and later audits.
		snapshot["native_price_usd"] = req.NativePriceUSD
		snapshot["native_price_at"] = req.NativePriceAt.UTC().Format(time.RFC3339)
		snapshot["native_price_source"] = req.NativePriceSource
	}
	// Keep the redeem credential independent of the public transfer hash.
	// The saved order supplies it on every retry, and the prefix plus random
	// text stays inside the host's 32-character credential limit.
	rechargeCode := "cd-" + rand.Text()
	order, err = c.entClient.PaymentOrder.Create().
		SetUserID(req.UserID).
		SetUserEmail(user.Email).
		SetUserName(displayName).
		SetAmount(creditAmount).
		SetPayAmount(paidAmount.Round(3).InexactFloat64()).
		SetFeeRate(0).
		SetRechargeCode(rechargeCode).
		SetOutTradeNo(outTradeNo).
		SetPaymentType(payment.TypeCrypto).
		SetPaymentTradeNo(req.TxHash).
		SetOrderType(payment.OrderTypeBalance).
		SetProviderKey(payment.TypeCrypto).
		SetStatus(payment.OrderStatusPaid).
		SetPaidAt(paidAt).
		// expires_at is required; the deposit already happened, so the
		// expiry is moot — anchor it at the paid time.
		SetExpiresAt(paidAt).
		// client_ip / src_host are required origin fields. A chain deposit
		// has no HTTP client, so mark the source as the crypto watcher.
		SetClientIP("crypto").
		SetSrcHost("crypto").
		SetProviderSnapshot(snapshot).
		Save(ctx)
	if err != nil {
		if !dbent.IsConstraintError(err) {
			return fmt.Errorf("cryptopay credit: create order: %w", err)
		}
		// Another scanner may have created the same order concurrently. Load
		// that exact identity; never treat an arbitrary constraint as success.
		order, err = c.entClient.PaymentOrder.Query().Where(paymentorder.OutTradeNoEQ(outTradeNo)).Only(ctx)
		if err != nil {
			return fmt.Errorf("cryptopay credit: reload concurrent order: %w", err)
		}
	}
	return c.resume(ctx, req, order, legacyCode)
}

// depositOrderIdentity preserves the source ledger keys used during recovery.
func depositOrderIdentity(chain, txHash string, logIndex int) (rechargeCode, outTradeNo string) {
	return ledger.OrderIdentity(chain, txHash, logIndex)
}

// resume validates the frozen order against the observed transfer and links
// it before invoking the balance executor. The deposit row rejects an order
// link if another order owns the event or the scanner has resolved it.
// A completed order finishes the ledger attempt without another grant.
// Fulfillment errors leave the existing identity available for retry, and
// Bark delivery is attempted only after completion is visible in storage.
func (c *DepositCreditor) resume(ctx context.Context, req ledger.CreditRequest, order *dbent.PaymentOrder, legacyCode string) error {
	validCode := billing.IsOrderBalanceCredential(order.RechargeCode) &&
		(order.RechargeCode == legacyCode || strings.HasPrefix(order.RechargeCode, "cd-"))
	if order.UserID != req.UserID || !payment.IsCryptoDepositOrder(paymentpostgres.OrderFromEntity(order)) || order.OrderType != payment.OrderTypeBalance ||
		!validCode || !strings.EqualFold(order.PaymentTradeNo, req.TxHash) ||
		!payment.CryptoDepositLogIndexMatches(order.ProviderSnapshot["log_index"], req.LogIndex) ||
		req.BlockNum > math.MaxInt64 || !payment.CryptoDepositLogIndexMatches(order.ProviderSnapshot["block"], int(req.BlockNum)) ||
		order.Amount <= 0 || math.IsNaN(order.Amount) || math.IsInf(order.Amount, 0) {
		return fmt.Errorf("%w: existing order identity mismatch", ledger.ErrCreditEvidence)
	}
	for key, want := range map[string]string{"chain": req.Chain, "token": req.Token, "from": req.FromAddr, "to": req.ToAddr, "tx": req.TxHash} {
		got, ok := order.ProviderSnapshot[key].(string)
		if !ok || !strings.EqualFold(got, want) {
			return fmt.Errorf("%w: existing order %s mismatch", ledger.ErrCreditEvidence, key)
		}
	}
	if order.ProviderSnapshot["raw_amount"] != req.Amount.String() ||
		!payment.CryptoDepositLogIndexMatches(order.ProviderSnapshot["decimals"], req.Decimals) {
		return fmt.Errorf("%w: existing order amount mismatch", ledger.ErrCreditEvidence)
	}
	if blockHash := payment.PsSnapshotStringValue(order.ProviderSnapshot["block_hash"]); blockHash != "" && !strings.EqualFold(blockHash, req.BlockHash) {
		return fmt.Errorf("%w: existing order block mismatch", ledger.ErrCreditEvidence)
	}
	grantCtx, err := paymentpostgres.PrepareCryptoDepositGrant(ctx, c.entClient, order, paymentpostgres.CryptoDepositGrant{
		Chain: req.Chain, TxHash: req.TxHash, LogIndex: req.LogIndex, BlockHash: req.BlockHash, Provisional: req.Provisional,
	})
	if errors.Is(err, paymentpostgres.ErrCryptoDepositEvidence) {
		return fmt.Errorf("%w: %v", ledger.ErrCreditEvidence, err)
	}
	if err != nil {
		return fmt.Errorf("cryptopay credit: recover original grant: %w", err)
	}
	if order.Status == payment.OrderStatusCompleted {
		return nil
	}
	if c.fulfill == nil {
		return fmt.Errorf("cryptopay credit: fulfillment unavailable")
	}
	if err := c.fulfill(grantCtx, order.ID); err != nil {
		if errors.Is(err, paymentpostgres.ErrCryptoDepositEvidence) {
			return fmt.Errorf("%w: %v", ledger.ErrCreditEvidence, err)
		}
		return fmt.Errorf("cryptopay credit: fulfillment: %w", err)
	}
	completed, err := c.entClient.PaymentOrder.Get(ctx, order.ID)
	if err != nil {
		return fmt.Errorf("cryptopay credit: verify completion: %w", err)
	}
	if completed.Status != payment.OrderStatusCompleted {
		return fmt.Errorf("cryptopay credit: order has not completed")
	}
	// Notify after the balance landed: a push that arrives before the credit
	// would send users looking at a balance that has not changed yet.
	if c.bark != nil {
		if err := c.bark.SendToUser(ctx, req.UserID, bark.Notification{
			Title: "充值到账",
			Body:  fmt.Sprintf("余额 +%.3f（%s）", order.Amount, req.TxHash),
			Group: "deposit",
		}); err != nil {
			// A failed push must never fail the credit that already landed.
			slog.Warn("[cryptopay] deposit push failed", "user", req.UserID, "error", err)
		}
	}
	return nil
}

// ReconcileDepositReorg applies the wallet-aware decision for a transfer
// already invalidated by the scanner. The store locks and classifies the
// original grant before persisting its terminal state. Only a committed
// deduction invalidates the cached balance and sends a user reversal notice.
func (c *DepositCreditor) ReconcileDepositReorg(ctx context.Context, target scanner.DepositReorg) (scanner.DepositReorgOutcome, error) {
	if err := mode.CheckWritable(); err != nil {
		return scanner.DepositReorgOutcome{}, err
	}
	if c == nil {
		return scanner.DepositReorgOutcome{}, fmt.Errorf("cryptopay reversal: deposit creditor unavailable")
	}
	chain := strings.TrimSpace(target.Chain)
	txHash := strings.ToLower(strings.TrimSpace(target.TxHash))
	legacyCode, outTradeNo := depositOrderIdentity(chain, txHash, target.LogIndex)
	result, err := paymentpostgres.RevokeProvisionalDepositCredit(ctx, c.entClient, paymentpostgres.ProvisionalDepositRevocation{
		Chain:        chain,
		TxHash:       txHash,
		LogIndex:     target.LogIndex,
		BlockHash:    target.BlockHash,
		BlockNumber:  target.BlockNumber,
		RevokeReason: target.Reason,
		OutTradeNo:   outTradeNo,
		LegacyCode:   legacyCode,
	})
	if err != nil {
		return scanner.DepositReorgOutcome{}, err
	}
	outcome := scanner.DepositReorgOutcome{Handled: result.Handled, Revoked: result.Revoked, ManualReview: result.ManualReview}
	if !result.Revoked {
		return outcome, nil
	}
	// Notify after the balance moved; a push that arrives first would send the
	// user looking at a balance that has not changed yet.
	if result.BalanceAfterKnown && c.invalidateBalance != nil {
		cacheCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if err := c.invalidateBalance(cacheCtx, result.UserID); err != nil {
			slog.Warn("[cryptopay] reversed deposit balance cache invalidation failed", "user", result.UserID, "error", err)
		}
		cancel()
	}
	if c.bark != nil && result.BalanceAfterKnown {
		body := fmt.Sprintf("一笔充值未最终确认，已回滚 %.3f（%s）。请重新转账或联系客服", result.Amount, strings.ToLower(txHash))
		if err := c.bark.SendToUser(ctx, result.UserID, bark.Notification{
			Title: "充值回滚",
			Body:  body,
			Group: "deposit",
		}); err != nil {
			slog.Warn("[cryptopay] provisional revoke push failed", "user", result.UserID, "error", err)
		}
	}
	return outcome, nil
}
