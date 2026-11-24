//go:build wireinject

package app

import (
	"github.com/TokenFlux/TokenRouter/internal/cryptopay"
	"github.com/TokenFlux/TokenRouter/internal/identity"
	"github.com/google/wire"

	"github.com/TokenFlux/TokenRouter/internal/payment"
	paymentpostgres "github.com/TokenFlux/TokenRouter/internal/payment/postgres"
)

// assembly_payment_wire.go registers payment runtime and HTTP constructors and
// binds the payment load balancer.
// Payment runtime, public endpoints, and administrator handlers share
// persisted provider instances and the application provider registry. Wire
// binds DefaultLoadBalancer to payment.LoadBalancer, so order processing
// receives selection backed by the same instance store and configured
// encryption key.

var (
	// paymentAssemblyProviders supplies payment runtime, expiry handling, and
	// public, administrator, and webhook HTTP constructors for application routes.
	// Deposit handlers share the host payment runtime and receive their scoped
	// authority, credential policy and notification delivery at construction.
	paymentAssemblyProviders = wire.NewSet(
		cryptopay.NewAddressService,
		cryptopay.NewServiceSettingStore,
		identity.NewCryptoSecurityService,
		provideWalletBark,
		provideDepositRotationGate,
		provideDepositCreditor,
		provideChainPayments,
		provideCryptoRouteMount,
		providePaymentExpiry,
		providePaymentHTTP,
		providePaymentAdminHTTP,
		providePaymentWebhookHTTP,
		providePaymentConfigCore,
		providePaymentRuntime,
		paymentProviders,
	)

	// paymentProviders supplies persisted instances, encryption, and registry
	// construction, then binds the default payment load balancer.
	paymentProviders = wire.NewSet(
		paymentpostgres.NewInstanceStore,
		providePaymentEncryptionKey,
		providePaymentRegistry,
		providePaymentLoadBalancer,
		wire.Bind(new(payment.LoadBalancer), new(*payment.DefaultLoadBalancer)),
	)
)
