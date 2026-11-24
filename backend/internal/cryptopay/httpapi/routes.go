package httpapi

import "github.com/gin-gonic/gin"

// routes.go binds the fixed crypto deposit and administration endpoints. The
// application supplies groups with its common authentication, rate limits, and
// audit policy. The crypto groups enforce their additional local instance and
// wallet access rules before dispatching to the imported handlers.
// The administrator table supplies both the direct-only declarations and the
// mounted routes, keeping the wallet console outside delegated management.

const (
	// cryptoAdminGroup locates the shared operator guard and defines the suffix
	// used to declare each full management path.
	cryptoAdminGroup = "/crypto"
)

// cryptoAdminRoute binds one direct administrator route to its wallet handler.
type cryptoAdminRoute struct {
	method  string          // registered HTTP method
	path    string          // crypto-relative Gin pattern
	handler gin.HandlerFunc // bound payment handler
}

// RegisterCryptoRoutes mounts the four customer deposit endpoints.
// Pass the /api/v1 group after the host JWT, backend-mode, panel, and audit
// middleware. CryptoUserAccess checks local readiness before the site's
// credential rotation policy runs. The self-service status and acknowledgment
// routes are mounted separately so a blocked user can inspect the decision.
func RegisterCryptoRoutes(authenticated *gin.RouterGroup, cryptoHandler *CryptoHandler, depositHandler *CryptoDepositHandler, rotationGuard gin.HandlerFunc) {
	crypto := authenticated.Group("/crypto")
	crypto.Use(cryptoHandler.CryptoUserAccess, rotationGuard)
	crypto.GET("/deposit-addresses", cryptoHandler.GetDepositAddresses)
	crypto.GET("/deposit-info", depositHandler.GetDepositInfo)
	crypto.GET("/deposit-network-status", depositHandler.GetDepositNetworkStatus)
	crypto.GET("/deposit-history", depositHandler.GetDepositHistory)
}

// RegisterCryptoAdminRoutes mounts wallet and deposit operations for direct HTTP
// access. Pass the /api/v1/admin group with the host Admin, Panel, and Audit
// middleware. CryptoAdminSecurity then checks the JWT principal and payment
// operator permissions before any registered handler runs.
func RegisterCryptoAdminRoutes(admin *gin.RouterGroup, cryptoHandler *CryptoHandler) {
	crypto := admin.Group(cryptoAdminGroup, cryptoHandler.CryptoAdminSecurity)
	for _, route := range cryptoAdminRoutes(cryptoHandler) {
		crypto.Handle(route.method, route.path, route.handler)
	}
}

// DeclareDirectOnlyAdminRoutes marks the wallet routes in the host catalogue.
// Call during application initialization with the host's direct-only declaration
// function. Binding the methods to an empty handler supplies the route patterns
// without consulting runtime dependencies.
func DeclareDirectOnlyAdminRoutes(declare func(method, absolutePath string)) {
	for _, route := range cryptoAdminRoutes(&CryptoHandler{}) {
		declare(route.method, "/api/v1/admin"+cryptoAdminGroup+route.path)
	}
}

// cryptoAdminRoutes supplies the method, pattern, and handler for each wallet
// endpoint. Both the direct-only declarations and Gin registration use this
// table so that adding a route here also declares its delegation policy.
func cryptoAdminRoutes(cryptoHandler *CryptoHandler) []cryptoAdminRoute {
	return []cryptoAdminRoute{
		{"GET", "/security/status", cryptoHandler.AdminCryptoSecurityStatus},
		{"POST", "/security/verify", cryptoHandler.AdminCryptoSecurityVerify},
		{"POST", "/security/operation-intent", cryptoHandler.AdminWalletOperationIntent},
		{"POST", "/security/setup", cryptoHandler.AdminCryptoSecuritySetup},
		{"POST", "/security/enable", cryptoHandler.AdminCryptoSecurityEnable},
		{"GET", "/config", cryptoHandler.AdminGetConfig},
		{"GET", "/config/private", cryptoHandler.AdminGetPrivateConfig},
		{"PUT", "/config", cryptoHandler.AdminUpdateConfig},
		{"GET", "/addresses", cryptoHandler.AdminListAddresses},
		{"GET", "/address-owners", cryptoHandler.AdminAddressOwners},
		{"POST", "/addresses/:id/status", cryptoHandler.AdminSetAddressStatus},
		{"POST", "/addresses/:id/rotate", cryptoHandler.AdminRotateAddress},
		{"POST", "/addresses/:id/export-private-key", cryptoHandler.AdminExportAddressPrivateKey},
		{"GET", "/sweep/plan", cryptoHandler.AdminSweepPlan},
		{"GET", "/sweep/readiness", cryptoHandler.AdminSweepReadiness},
		{"POST", "/sweep/run", cryptoHandler.AdminRunSweep},
		{"GET", "/sweep/runs", cryptoHandler.AdminSweepRuns},
		{"GET", "/sweep/runs/:id", cryptoHandler.AdminSweepRunStatus},
		{"GET", "/sweep/records", cryptoHandler.AdminSweepRecords},
		{"GET", "/sweep/blocked", cryptoHandler.AdminListBlockedSweeps},
		{"POST", "/sweep/records/:id/reconcile", cryptoHandler.AdminReconcileSweep},
		{"GET", "/deposits", cryptoHandler.AdminListDeposits},
		{"POST", "/deposits/confirm", cryptoHandler.AdminConfirmDeposits},
		{"POST", "/scan/run", cryptoHandler.AdminRunScan},
		{"GET", "/scan/runs", cryptoHandler.AdminScanRuns},
		{"GET", "/scan/runs/:id", cryptoHandler.AdminScanRunStatus},
		{"POST", "/scan/runs/:id/cancel", cryptoHandler.AdminCancelScanRun},
		{"GET", "/wallet/status", cryptoHandler.AdminWalletStatus},
		{"GET", "/wallet/security", cryptoHandler.AdminWalletSecurity},
		{"POST", "/wallet/security/recover", cryptoHandler.AdminRecoverWalletSecurity},
		{"POST", "/wallet/initialize", cryptoHandler.AdminInitializeWallet},
		{"POST", "/wallet/export-mnemonics", cryptoHandler.AdminExportMnemonics},
		{"POST", "/wallet/hot-mnemonic", cryptoHandler.AdminImportHotMnemonic},
		{"GET", "/pool", cryptoHandler.AdminPoolStatus},
		{"GET", "/chains/status", cryptoHandler.AdminChainStatus},
		{"GET", "/hot-wallet", cryptoHandler.AdminHotWalletStatus},
		{"GET", "/hot-wallet/address", cryptoHandler.AdminHotWalletAddress},
		{"GET", "/deposits.csv", cryptoHandler.AdminExportDeposits},
		// The Shamir 2-of-3 setup stores sealed share A. Verification and
		// unlock receive an operator share for use during the request.
		{"GET", "/signer/status", cryptoHandler.AdminSignerStatus},
		{"GET", "/signer/risk-notice", cryptoHandler.AdminSignerRiskNotice},
		{"POST", "/signer/setup", cryptoHandler.AdminSignerSetup},
		{"POST", "/signer/resplit", cryptoHandler.AdminSignerResplit},
		{"POST", "/signer/verify-share", cryptoHandler.AdminSignerVerifyShare},
		// Unlocking keeps the data key resident only for the bounded 15-minute
		// lease; it still requires a recovery share plus a freshly typed code.
		{"POST", "/signer/unlock", cryptoHandler.AdminSignerUnlock},
		{"POST", "/signer/auto-sweep", cryptoHandler.AdminSignerAutoSweep},
		// Moving a pinned sweep destination is deliberately not part of the
		// generic config save: it needs a fresh code, the risk phrase and an
		// explicit acknowledgement, and it is the only audited destination path.
		{"POST", "/signer/sweep-destination", cryptoHandler.AdminUpdateSweepDestination},
		{"POST", "/rpc/test", cryptoHandler.AdminTestRPC},
		{"GET", "/rpc/candidates", cryptoHandler.AdminChainlistCandidates},
		{"POST", "/rpc/auto-pick", cryptoHandler.AdminAutoPickRPC},
	}
}

// RegisterUserBarkRoutes mounts the authenticated user's own notification settings.
// Pass the /api/v1 group after the host JWT, backend-mode, panel, and audit
// middleware. The request subject supplies the only account identifier.
func RegisterUserBarkRoutes(authenticated *gin.RouterGroup, bark *UserBarkHandler) {
	user := authenticated.Group("/user")
	user.Use(CryptoAdminTestModeGuard())
	user.GET("/bark", bark.GetBarkSettings)
	user.PUT("/bark", bark.UpdateBarkSettings)
}
