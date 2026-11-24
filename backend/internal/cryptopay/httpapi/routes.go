package httpapi

import "github.com/gin-gonic/gin"

// routes.go binds the fixed crypto deposit and administration endpoints. The
// application supplies groups with its common authentication, rate limits, and
// audit policy. The crypto groups enforce their additional local instance and
// wallet access rules before dispatching to the imported handlers.

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

// RegisterCryptoAdminRoutes registers the admin crypto management endpoints,
// including channel configuration, wallets, and settlement. Pass the /api/v1/admin
// group after the host Admin, Panel, and Audit middleware. The crypto gate then
// checks the actual JWT principal and the operation-specific authorization.
func RegisterCryptoAdminRoutes(admin *gin.RouterGroup, cryptoHandler *CryptoHandler) {
	crypto := admin.Group("/crypto")
	crypto.Use(cryptoHandler.CryptoAdminSecurity)
	{
		crypto.GET("/security/status", cryptoHandler.AdminCryptoSecurityStatus)
		crypto.POST("/security/verify", cryptoHandler.AdminCryptoSecurityVerify)
		crypto.POST("/security/operation-intent", cryptoHandler.AdminWalletOperationIntent)
		crypto.POST("/security/setup", cryptoHandler.AdminCryptoSecuritySetup)
		crypto.POST("/security/enable", cryptoHandler.AdminCryptoSecurityEnable)
		crypto.GET("/config", cryptoHandler.AdminGetConfig)
		crypto.GET("/config/private", cryptoHandler.AdminGetPrivateConfig)
		crypto.PUT("/config", cryptoHandler.AdminUpdateConfig)
		crypto.GET("/addresses", cryptoHandler.AdminListAddresses)
		crypto.GET("/address-owners", cryptoHandler.AdminAddressOwners)
		crypto.POST("/addresses/:id/status", cryptoHandler.AdminSetAddressStatus)
		crypto.POST("/addresses/:id/rotate", cryptoHandler.AdminRotateAddress)
		crypto.POST("/addresses/:id/export-private-key", cryptoHandler.AdminExportAddressPrivateKey)
		crypto.GET("/sweep/plan", cryptoHandler.AdminSweepPlan)
		crypto.GET("/sweep/readiness", cryptoHandler.AdminSweepReadiness)
		crypto.POST("/sweep/run", cryptoHandler.AdminRunSweep)
		crypto.GET("/sweep/runs", cryptoHandler.AdminSweepRuns)
		crypto.GET("/sweep/runs/:id", cryptoHandler.AdminSweepRunStatus)
		crypto.GET("/sweep/records", cryptoHandler.AdminSweepRecords)
		crypto.GET("/sweep/blocked", cryptoHandler.AdminListBlockedSweeps)
		crypto.POST("/sweep/records/:id/reconcile", cryptoHandler.AdminReconcileSweep)
		crypto.GET("/deposits", cryptoHandler.AdminListDeposits)
		crypto.POST("/deposits/confirm", cryptoHandler.AdminConfirmDeposits)
		crypto.POST("/scan/run", cryptoHandler.AdminRunScan)
		crypto.GET("/scan/runs", cryptoHandler.AdminScanRuns)
		crypto.GET("/scan/runs/:id", cryptoHandler.AdminScanRunStatus)
		crypto.POST("/scan/runs/:id/cancel", cryptoHandler.AdminCancelScanRun)
		crypto.GET("/wallet/status", cryptoHandler.AdminWalletStatus)
		crypto.GET("/wallet/security", cryptoHandler.AdminWalletSecurity)
		crypto.POST("/wallet/security/recover", cryptoHandler.AdminRecoverWalletSecurity)
		crypto.POST("/wallet/initialize", cryptoHandler.AdminInitializeWallet)
		crypto.POST("/wallet/export-mnemonics", cryptoHandler.AdminExportMnemonics)
		crypto.POST("/wallet/hot-mnemonic", cryptoHandler.AdminImportHotMnemonic)
		crypto.GET("/pool", cryptoHandler.AdminPoolStatus)
		crypto.GET("/chains/status", cryptoHandler.AdminChainStatus)
		crypto.GET("/hot-wallet", cryptoHandler.AdminHotWalletStatus)
		crypto.GET("/hot-wallet/address", cryptoHandler.AdminHotWalletAddress)
		crypto.GET("/deposits.csv", cryptoHandler.AdminExportDeposits)
		// The Shamir 2-of-3 setup stores sealed share A. Verification and
		// unlock receive an operator share for use during the request.
		crypto.GET("/signer/status", cryptoHandler.AdminSignerStatus)
		crypto.GET("/signer/risk-notice", cryptoHandler.AdminSignerRiskNotice)
		crypto.POST("/signer/setup", cryptoHandler.AdminSignerSetup)
		crypto.POST("/signer/resplit", cryptoHandler.AdminSignerResplit)
		crypto.POST("/signer/verify-share", cryptoHandler.AdminSignerVerifyShare)
		// Unlocking keeps the data key resident only for the bounded 15-minute
		// lease; it still requires a recovery share plus a freshly typed code.
		crypto.POST("/signer/unlock", cryptoHandler.AdminSignerUnlock)
		crypto.POST("/signer/auto-sweep", cryptoHandler.AdminSignerAutoSweep)
		// Moving a pinned sweep destination is deliberately not part of the
		// generic config save: it needs a fresh code, the risk phrase and an
		// explicit acknowledgement, and it is the only audited destination path.
		crypto.POST("/signer/sweep-destination", cryptoHandler.AdminUpdateSweepDestination)
		crypto.POST("/rpc/test", cryptoHandler.AdminTestRPC)
		crypto.GET("/rpc/candidates", cryptoHandler.AdminChainlistCandidates)
		crypto.POST("/rpc/auto-pick", cryptoHandler.AdminAutoPickRPC)
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
