package app

import (
	"context"
	"database/sql"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/TokenFlux/TokenRouter/ent"
	"github.com/TokenFlux/TokenRouter/internal/apikey"
	keyhttp "github.com/TokenFlux/TokenRouter/internal/apikey/httpapi"
	keypostgres "github.com/TokenFlux/TokenRouter/internal/apikey/postgres"
	"github.com/TokenFlux/TokenRouter/internal/app/lifecycle"
	"github.com/TokenFlux/TokenRouter/internal/billing"
	"github.com/TokenFlux/TokenRouter/internal/config"
	"github.com/TokenFlux/TokenRouter/internal/cryptopay"
	"github.com/TokenFlux/TokenRouter/internal/cryptopay/addresses"
	cryptohttp "github.com/TokenFlux/TokenRouter/internal/cryptopay/httpapi"
	"github.com/TokenFlux/TokenRouter/internal/identity"
	"github.com/TokenFlux/TokenRouter/internal/notification"
	"github.com/TokenFlux/TokenRouter/internal/payment"
)

// crypto_payments.go connects chain observations to the same payment, identity
// and notification services that the rest of the application uses. A credited
// deposit enters the payment order fulfillment path. A reversal invalidates the
// cached balance used for gateway eligibility. The wallet's operator alert has
// a specific recipient independent of personal deposit notifications.
//
// Construction prepares the components and guards. The registered lifecycle
// hook starts the scanner with both notification channels attached and stops
// it before Redis and PostgreSQL close. Route groups reuse the panel session,
// rate-limiter and audit middleware while the deposit handlers enforce their
// additional payment permissions and signing rules.

// cryptoRouteMount registers deposit and wallet-management routes under the
// panel API using the application's already assembled authentication guards.
type cryptoRouteMount func(*gin.RouterGroup, httpRouteSecurity)

// provideDepositRotationGate binds the user's current credential state to the
// site's configured cutoff and acknowledgment policy. The status, acknowledgment
// and deposit endpoints consult the same repository and decision rules.
func provideDepositRotationGate(db *sql.DB, cfg *config.Config) *keyhttp.KeyRotationGateHandler {
	store := keypostgres.NewKeyRotationGateStore(db)
	return keyhttp.NewKeyRotationGateHandler(apikey.NewKeyRotationGate(store, cfg.Security))
}

// provideWalletBark binds encrypted device preferences to the same identity
// encryption implementation used for account security secrets.
func provideWalletBark(db *sql.DB, encryptor identity.SecretEncryptor) *notification.UserBarkService {
	return notification.NewUserBarkService(db, notification.AdaptEncryptor(encryptor))
}

// provideDepositCreditor attaches the chain ledger to the existing order
// fulfillment engine. Notifications and gateway balance invalidation follow
// the corresponding committed financial operation.
// @project-doc docs/domains/payments_and_entitlements.md#onchain_balance_deposits
func provideDepositCreditor(client *ent.Client, payments *payment.Runtime, db *sql.DB, eligibility *billing.Eligibility, keys *apikey.APIKeyService, bark *notification.UserBarkService) *cryptopay.DepositCreditor {
	creditor := cryptopay.NewDepositCreditor(client, payments.OrderLifecycle.Fulfillment, db)
	creditor.SetBalanceInvalidator(func(ctx context.Context, userID int64) error {
		// Gateway eligibility and authenticated API keys each retain wallet
		// information. Attempt both invalidations after the database commits.
		err := eligibility.InvalidateUserBalance(ctx, userID)
		keys.InvalidateAuthCacheByUserID(ctx, userID)
		return err
	})
	creditor.SetBarkNotifier(bark)
	return creditor
}

// provideChainPayments prepares the host-owned chain runtime. Its scanner
// starts only after the application has attached the operator notification
// sinks. Shutdown releases the wallet and stops its producers before storage.
func provideChainPayments(db *sql.DB, store *cryptopay.ServiceSettingStore, creditor *cryptopay.DepositCreditor, addressService *addresses.Service, cfg *config.Config, rdb *redis.Client, bark *notification.UserBarkService, mailer *notification.Mailer, manager *lifecycle.Manager) *cryptopay.Service {
	runtime := cryptopay.NewManagedService(db, store, creditor, addressService, cfg, rdb)
	// The upstream wallet design routes private security alerts to its
	// primary administrator account. Other depositors receive only their own
	// balance notifications through the creditor above.
	runtime.SetPushNotifier(notification.NewOwnerPushAdapter(bark, 1))
	manager.Register(lifecycle.Hook{
		Name: "ChainDeposits", StartOrder: 950, StopOrder: shutdownStageProducers,
		Start: func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			runtime.SetEventNotifier(cryptopay.NewEventNotifier(store, mailer))
			runtime.Start(ctx)
			return ctx.Err()
		},
		Stop: func(context.Context) error {
			runtime.Close()
			return nil
		},
	})
	return runtime
}

// provideCryptoRouteMount binds buyer and administrator routes to the same
// wallet runtime. User preferences are scoped to the signed-in account. Each
// administrator action also passes the crypto handler's operator checks.
func provideCryptoRouteMount(runtime *cryptopay.Service, addressService *addresses.Service, store *cryptopay.ServiceSettingStore, authority *identity.CryptoSecurityService, cfg *config.Config, bark *notification.UserBarkService, rotation *keyhttp.KeyRotationGateHandler) cryptoRouteMount {
	management := cryptohttp.NewCryptoHandler(addressService, store, runtime, authority, cfg)
	deposits := cryptohttp.NewCryptoDepositHandler(runtime)
	preferences := cryptohttp.NewUserBarkHandler(bark, cfg)
	return func(v1 *gin.RouterGroup, security httpRouteSecurity) {
		user := v1.Group("", security.JWT, security.BackendUser, security.Panel.Global(), security.Audit)
		admin := v1.Group("/admin", security.Admin, security.Panel.Global(), security.Audit)
		rotation.Register(user)
		cryptohttp.RegisterCryptoRoutes(user, management, deposits, rotation.Guard)
		cryptohttp.RegisterCryptoAdminRoutes(admin, management)
		cryptohttp.RegisterUserBarkRoutes(user, preferences)
	}
}
