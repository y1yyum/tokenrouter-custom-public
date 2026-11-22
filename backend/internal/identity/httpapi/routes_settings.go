package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/TokenFlux/TokenRouter/internal/identity/httpapi/authctx"
	"github.com/TokenFlux/TokenRouter/internal/server/adminapi"
)

// routes_settings.go registers administrator endpoints for reading,
// regenerating, and deleting the management API key.
// The stored management credential has separate status, regeneration, and
// deletion endpoints under administrator settings. Route registration binds
// those operations to one endpoint interface and relies on the settings group
// to apply its authentication and audit middleware.

// IdentitySettingsEndpoints defines administrator HTTP operations for the
// stored management credential.
type IdentitySettingsEndpoints interface {
	// GetAdminAPIKey writes credential existence and a masked display value to
	// the panel response. The caller installs administrator authentication before
	// allowing a request to reach it.
	GetAdminAPIKey(*gin.Context)
	// RegenerateAdminAPIKey saves a replacement management credential and returns
	// its full value once generated. The caller restricts this route to
	// authenticated administrator requests before handler dispatch.
	RegenerateAdminAPIKey(*gin.Context)
	// DeleteAdminAPIKey removes the configured management credential and reports
	// completion through the panel envelope. The caller restricts this route to
	// authenticated administrator requests before handler dispatch.
	DeleteAdminAPIKey(*gin.Context)
}

// RegisterIdentitySettingsRoutes adds management-key status, regeneration, and
// deletion endpoints to administrator settings. The caller installs
// authentication and auditing on the supplied group before exposing the
// credential operations. The per-route guard runs inside audit so a verified
// site key receives an attributed denial before any credential handler runs.
func RegisterIdentitySettingsRoutes(adminSettings *gin.RouterGroup, endpoint IdentitySettingsEndpoints) {
	adminapi.Register(adminSettings, ManagementOperations["settings.admin_api_key.list"], authorizeKeyManagement, endpoint.GetAdminAPIKey)
	adminapi.Register(adminSettings, ManagementOperations["settings.admin_api_key.regenerate.create"], authorizeKeyManagement, endpoint.RegenerateAdminAPIKey)
	adminapi.Register(adminSettings, ManagementOperations["settings.admin_api_key.delete"], authorizeKeyManagement, endpoint.DeleteAdminAPIKey)
}

// authorizeKeyManagement admits administrator sessions and authorized internal
// MCP calls to the site key lifecycle. The parent group authenticates callers
// first so missing identity fails closed and audit can attribute key denials.
//
// @project-doc docs/domains/identity_and_tenancy.md#authentication_boundaries
func authorizeKeyManagement(c *gin.Context) {
	principal, ok := authctx.GetPrincipal(c)
	if !ok || (principal.CredentialKind != "jwt" && principal.CredentialKind != "mcp_token") {
		AbortWithError(c, http.StatusForbidden, "FORBIDDEN", "Use an administrator session to manage the site API key")
		return
	}
	c.Next()
}
