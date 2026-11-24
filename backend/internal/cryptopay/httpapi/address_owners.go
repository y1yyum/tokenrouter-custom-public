package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/TokenFlux/TokenRouter/internal/cryptopay"
	"github.com/TokenFlux/TokenRouter/internal/server/httpx"
)

// address_owners.go resolves deposit addresses to their assigned user records for
// the payment console. The wallet lookup limits the query and returns live owner
// identities. Handler diagnostics use fixed messages so database errors do not
// expose the identities fetched for the console.

// addressOwnerResolver is the part of cryptopay.Service the owners endpoint needs.
type addressOwnerResolver interface {
	LookupAddressOwners(ctx context.Context, addresses []string) (map[string]cryptopay.AddressOwner, error)
}

// AdminAddressOwners resolves deposit addresses to the users they were
// assigned to, so the console can show who a deposit or sweep belongs to.
// GET /api/v1/admin/crypto/address-owners?address=0x..&address=0x..
//
// The response carries user emails. They appear here and nowhere else: not in
// logs, not in error text.
func (h *CryptoHandler) AdminAddressOwners(c *gin.Context) {
	if h.service == nil {
		httpx.BadRequest(c, "crypto channel unavailable")
		return
	}
	serveAddressOwners(c, h.service)
}

// serveAddressOwners resolves the requested addresses and hides owner lookup failure details.
func serveAddressOwners(c *gin.Context, resolver addressOwnerResolver) {
	owners, err := resolver.LookupAddressOwners(c.Request.Context(), c.QueryArray("address"))
	switch {
	case errors.Is(err, cryptopay.ErrTooManyOwnerAddresses):
		httpx.BadRequest(c, "too many addresses")
		return
	case err != nil:
		// Fixed text on both sides. The cause is deliberately not printed: the
		// rows being read hold user identities.
		slog.WarnContext(c.Request.Context(), "[cryptopay] address owner lookup failed")
		httpx.Error(c, http.StatusInternalServerError, "address owner lookup failed")
		return
	}
	if owners == nil {
		owners = map[string]cryptopay.AddressOwner{}
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"owners": owners}})
}
