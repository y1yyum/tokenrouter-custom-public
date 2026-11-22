package mcp_test

import (
	"context"

	"github.com/TokenFlux/TokenRouter/internal/identity"
)

// tokenRepository supplies an immutable administrator grant for MCP authentication.
type tokenRepository struct {
	identity.MCPTokenRepository                   // unused credential persistence methods
	token                       identity.MCPToken // fixed management permission grant
	digest                      string            // accepted credential SHA-256 digest
}

// userReader supplies the active administrator owning the fixture token.
type userReader struct{}

// Get returns a grant copy for the fixture's exact token identity.
// The fixture keeps permission lists immutable;
// callers receive a separate token value for request-local authorization metadata.
func (r tokenRepository) Get(_ context.Context, id int64) (*identity.MCPToken, error) {
	if id != r.token.ID {
		return nil, identity.ErrMCPTokenNotFound
	}
	value := r.token
	return &value, nil
}

// FindHash validates the fixture digest before returning its management grant.
func (r tokenRepository) FindHash(ctx context.Context, digest string) (*identity.MCPToken, error) {
	if digest != r.digest {
		return nil, identity.ErrMCPTokenInvalid
	}
	return r.Get(ctx, r.token.ID)
}

// Touch accepts admission observations without modifying the immutable test grant.
func (tokenRepository) Touch(context.Context, int64) error { return nil }

// GetByID returns an active administrator for the supplied fixture identity.
func (userReader) GetByID(_ context.Context, id int64) (*identity.User, error) {
	return &identity.User{ID: id, Role: identity.RoleAdmin, Status: identity.StatusActive}, nil
}
