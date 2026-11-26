package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/TokenFlux/TokenRouter/internal/payment"
)

// crypto.go presents the direct chain deposit channel to the payment system.
// It reports pending for provider queries and defines the checkout, callback,
// and refund rejection responses. Only chain reconciliation can confirm these
// deposits or reverse an earlier provisional credit.

// Crypto identifies existing balance orders created by the standalone
// deposit scanner/ledger pipeline. It cannot create checkout payments:
// address deposits have no association with checkout or subscription orders.
type Crypto struct {
	currency string // display currency
}

// NewCrypto reads the optional display currency and defaults it to USD.
// A malformed currency returns an error. Scanner, signer and sweep settings
// belong to the separate chain deposit service.
func NewCrypto(_ string, config map[string]string) (*Crypto, error) {
	currency := strings.ToUpper(strings.TrimSpace(config["currency"]))
	if currency == "" {
		currency = "USD"
	}
	if _, err := payment.NormalizePaymentCurrency(currency); err != nil {
		return nil, fmt.Errorf("crypto config currency: %w", err)
	}
	return &Crypto{currency: currency}, nil
}

// Name returns the label for this direct deposit provider.
func (c *Crypto) Name() string { return "Crypto" }

// ProviderKey returns the stable key for direct deposit orders.
func (c *Crypto) ProviderKey() string { return payment.TypeCrypto }

// SupportedTypes returns the single method this provider represents.
func (c *Crypto) SupportedTypes() []payment.PaymentType {
	return []payment.PaymentType{payment.TypeCrypto}
}

// MerchantIdentityMetadata returns an independent map containing the display currency.
func (c *Crypto) MerchantIdentityMetadata() map[string]string {
	return map[string]string{"currency": c.currency}
}

// CreatePayment refuses orders that the deposit scanner cannot settle.
func (c *Crypto) CreatePayment(_ context.Context, _ payment.CreatePaymentRequest) (*payment.CreatePaymentResponse, error) {
	return nil, payment.ErrCryptoCheckoutUnsupported
}

// QueryOrder reports pending: crypto orders settle through the scanner,
// never through a pull query.
func (c *Crypto) QueryOrder(_ context.Context, tradeNo string) (*payment.QueryOrderResponse, error) {
	return &payment.QueryOrderResponse{TradeNo: tradeNo, Status: payment.ProviderStatusPending}, nil
}

// VerifyNotification rejects callbacks: the crypto channel is settled
// internally by the ledger, so any inbound webhook claiming to be one is
// either a mistake or an attack.
func (c *Crypto) VerifyNotification(_ context.Context, _ string, _ map[string]string) (*payment.PaymentNotification, error) {
	return nil, payment.ErrCryptoWebhookUnsupported
}

// Refund is unsupported: on-chain payments cannot be pulled back.
func (c *Crypto) Refund(_ context.Context, _ payment.RefundRequest) (*payment.RefundResponse, error) {
	return nil, payment.ErrCryptoRefundUnsupported
}
