package payment

import (
	"context"
	"encoding/hex"
	"math"
	"math/big"
	"strings"

	"github.com/TokenFlux/TokenRouter/internal/pkg/apperror"
)

// crypto_deposit.go distinguishes direct chain deposits from provider checkout.
// Older orders may retain only the immutable transfer tuple. A complete
// tuple tied to the stored transaction reference prevents a generic provider
// query or administrative retry from treating such an order as a checkout.
//
// The scanner attaches a deposit to an order after its chain checks. Only
// its order-scoped fulfillment entry authorizes the shared balance grant;
// callbacks, generic recovery and refunds cannot establish that authority.

var (
	// ErrCryptoRetryRequiresLedger directs operators to recovery through the
	// chain scanner, which owns the transfer and confirmation checks.
	ErrCryptoRetryRequiresLedger = apperror.Forbidden("CRYPTO_RETRY_REQUIRES_LEDGER", "recover this deposit through the chain ledger")
	// ErrCryptoCheckoutUnsupported directs buyers to the dedicated deposit page.
	ErrCryptoCheckoutUnsupported = apperror.BadRequest("CRYPTO_CHECKOUT_UNSUPPORTED", "use the standalone deposit page for crypto deposits")
	// ErrCryptoWebhookUnsupported rejects external observations for direct deposits.
	ErrCryptoWebhookUnsupported = apperror.BadRequest("CRYPTO_WEBHOOK_UNSUPPORTED", "crypto deposits do not accept payment callbacks")
	// ErrCryptoRefundUnsupported directs reversals through chain reconciliation.
	ErrCryptoRefundUnsupported = apperror.Forbidden("CRYPTO_REFUND_UNSUPPORTED", "crypto deposits do not support payment refunds")
)

// cryptoDepositFulfillmentKey carries the scanner-authorized order identity.
type cryptoDepositFulfillmentKey struct{}

// ExecuteCryptoDepositFulfillment fulfills a balance order linked by the chain
// deposit creditor. Call only after chain verification and deposit linkage.
// The authorization applies to this order and the normal balance executor
// still owns its lease, redemption and rebate deduplication.
func (s *Fulfillment) ExecuteCryptoDepositFulfillment(ctx context.Context, orderID int64) error {
	return s.ExecuteBalanceFulfillment(context.WithValue(ctx, cryptoDepositFulfillmentKey{}, orderID), orderID)
}

// canFulfillCryptoDeposit checks whether the chain creditor selected this order.
func canFulfillCryptoDeposit(ctx context.Context, order *Order) bool {
	orderID, ok := ctx.Value(cryptoDepositFulfillmentKey{}).(int64)
	return ok && orderID == order.ID && order.OrderType == OrderTypeBalance
}

// isCryptoPaymentType accepts canonical and whitespace/case-variant stored labels.
func isCryptoPaymentType(value string) bool {
	return strings.EqualFold(strings.TrimSpace(value), TypeCrypto)
}

// CryptoDepositLogIndexMatches matches a nonnegative stored transfer log index.
// It accepts integer and JSON numeric representations used by payment snapshots.
func CryptoDepositLogIndexMatches(value any, logIndex int) bool {
	if logIndex < 0 {
		return false
	}
	index, ok := cryptoSnapshotUint(value, uint64(logIndex))
	return ok && index == uint64(logIndex)
}

// IsCryptoDepositOrder recognizes current and legacy persisted crypto markers.
// Crypto deposits are settled only by the protected chain scanner/ledger; the
// generic admin payment retry endpoint has neither its fresh 2FA boundary nor
// the chain-observation checks required before issuing balance credit.
func IsCryptoDepositOrder(order *Order) bool {
	if order == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(order.PaymentType), TypeCrypto) ||
		strings.EqualFold(strings.TrimSpace(refundStringValue(order.ProviderKey)), TypeCrypto) {
		return true
	}
	if snapshot := PsOrderProviderSnapshot(order); snapshot != nil && strings.EqualFold(snapshot.ProviderKey, TypeCrypto) {
		return true
	}
	return order.OrderType == OrderTypeBalance &&
		strings.EqualFold(strings.TrimSpace(order.PaymentTradeNo), PsSnapshotStringValue(order.ProviderSnapshot["tx"])) &&
		isLegacyCryptoDepositSnapshot(order.ProviderSnapshot)
}

// isLegacyCryptoDepositSnapshot recognizes the immutable transfer tuple
// written by the deposit creditor before orders carried a provider key.
// Require all event fields, because payment transaction numbers and isolated
// snapshot keys can also appear in ordinary checkout orders.
func isLegacyCryptoDepositSnapshot(snapshot map[string]any) bool {
	if snapshot == nil {
		return false
	}
	chain := strings.ToLower(PsSnapshotStringValue(snapshot["chain"]))
	switch chain {
	case "bsc", "ethereum", "polygon", "plasma", "arbitrum", "base", "xlayer":
	default:
		return false
	}
	for _, key := range []string{"token", "tx", "raw_amount", "from", "to"} {
		if PsSnapshotStringValue(snapshot[key]) == "" {
			return false
		}
	}
	if (PsSnapshotStringValue(snapshot["token"]) != "native" && !isCryptoSnapshotHex(snapshot["token"], 20)) || !isCryptoSnapshotHex(snapshot["tx"], 32) ||
		!isCryptoSnapshotHex(snapshot["from"], 20) || !isCryptoSnapshotHex(snapshot["to"], 20) {
		return false
	}
	rawAmount := PsSnapshotStringValue(snapshot["raw_amount"])
	amount, ok := new(big.Int).SetString(rawAmount, 10)
	if !ok || amount.Sign() <= 0 || amount.String() != rawAmount {
		return false
	}
	logIndex, hasLogIndex := snapshot["log_index"]
	blockNumber, hasBlock := snapshot["block"]
	decimals, hasDecimals := snapshot["decimals"]
	if !hasLogIndex || !hasBlock || !hasDecimals {
		return false
	}
	if _, ok := cryptoSnapshotUint(logIndex, uint64(^uint(0)>>1)); !ok {
		return false
	}
	if _, ok := cryptoSnapshotUint(blockNumber, ^uint64(0)); !ok {
		return false
	}
	if _, ok := cryptoSnapshotUint(decimals, 255); !ok {
		return false
	}
	return true
}

// cryptoSnapshotUint reads a nonnegative snapshot integer within the supplied
// bound. Accept native integer types and JSON float64 values only when they
// represent an integer, preserving the uint64 bound before conversion.
func cryptoSnapshotUint(value any, max uint64) (uint64, bool) {
	switch number := value.(type) {
	case int:
		if number < 0 {
			return 0, false
		}
		return cryptoSnapshotUint(uint64(number), max)
	case int32:
		if number < 0 {
			return 0, false
		}
		return cryptoSnapshotUint(uint64(number), max)
	case int64:
		if number < 0 {
			return 0, false
		}
		return cryptoSnapshotUint(uint64(number), max)
	case uint:
		return cryptoSnapshotUint(uint64(number), max)
	case uint32:
		return cryptoSnapshotUint(uint64(number), max)
	case uint64:
		return number, number <= max
	case float64:
		if math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || math.Trunc(number) != number {
			return 0, false
		}
		integer, _ := new(big.Float).SetFloat64(number).Int(nil)
		if integer.Sign() < 0 || integer.BitLen() > 64 || integer.Cmp(new(big.Int).SetUint64(max)) > 0 {
			return 0, false
		}
		return integer.Uint64(), true
	default:
		return 0, false
	}
}

// isCryptoSnapshotHex checks the exact byte length of a prefixed hex string.
func isCryptoSnapshotHex(value any, byteLength int) bool {
	text := PsSnapshotStringValue(value)
	if len(text) != 2+byteLength*2 || !strings.HasPrefix(strings.ToLower(text), "0x") {
		return false
	}
	_, err := hex.DecodeString(text[2:])
	return err == nil
}
