package identity

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	infraerrors "github.com/TokenFlux/TokenRouter/internal/pkg/apperror"
)

// crypto_security_grant.go signs short-lived payment grants and checks them
// against current account security and Redis. A grant authenticates one
// account and JWT session, and its signed state encodes the credentials and
// authenticator in use when the user verified the code.
//
// Signing alone leaves a grant valid until its timestamp expires. Redis also
// registers each issued grant and keeps short-lived logout markers, so a
// session can invalidate its outstanding grants without waiting for their
// signatures to expire. Verification requires each source to agree.

const (
	// CryptoGrantTTL bounds payment grant validity and the retention required
	// for logout markers to cover every grant existing at revocation.
	CryptoGrantTTL = 5 * time.Minute
)

// ErrCryptoGrantRequired prompts the client to discard an unusable payment
// grant and obtain new authenticator verification before a sensitive operation.
var ErrCryptoGrantRequired = infraerrors.Forbidden("CRYPTO_GRANT_REQUIRED", "payment verification expired or is invalid; verify two-factor authentication again")

// CryptoVerificationGrant is a fixed-lifetime payment capability. It is not a
// login token and cannot authorize account administration outside crypto routes.
type CryptoVerificationGrant struct {
	Credential string    `json:"credential"` // signed payment capability
	ExpiresAt  time.Time `json:"expires_at"` // absolute expiry timestamp
	ExpiresIn  int64     `json:"expires_in"` // configured lifetime in seconds
}

// cryptoGrantClaims contains the canonical JSON fields authenticated by the grant MAC.
type cryptoGrantClaims struct {
	Version    int    `json:"v"`      // signed claim schema
	UserID     int64  `json:"u"`      // current operator identifier
	Session    string `json:"s"`      // JWT session digest
	State      string `json:"a"`      // account proof fingerprint
	Nonce      string `json:"n"`      // random grant identifier
	IssuedAt   int64  `json:"iat"`    // Unix seconds
	IssuedAtMS int64  `json:"iat_ms"` // Unix milliseconds
	ExpiresAt  int64  `json:"exp"`    // Unix seconds
}

// CryptoGrantRevocationReader reads short-lived logout tombstones shared by
// the authenticator store and refresh-token revocation paths.
type CryptoGrantRevocationReader interface {
	// CryptoGrantRevocationTimes returns account and session logout times in
	// Unix milliseconds. Zero denotes an absent marker and errors prevent grant
	// verification.
	CryptoGrantRevocationTimes(context.Context, int64, string) (int64, int64, error)
}

// CryptoGrantUserRevocationKey identifies the Redis marker for account-wide grant revocation.
func CryptoGrantUserRevocationKey(userID int64) string {
	return fmt.Sprintf("crypto:grant:revoked:user:%d", userID)
}

// CryptoGrantSessionRevocationKey avoids persisting plaintext session IDs.
func CryptoGrantSessionRevocationKey(sessionID string) string {
	digest := sha256.Sum256([]byte(sessionID))
	return fmt.Sprintf("crypto:grant:revoked:session:%x", digest)
}

// CryptoSessionKey derives the same session binding for payment verification
// and logout. The caller supplies values from a validated access JWT.
// Older JWTs without a refresh family bind to the exact credential digest.
func CryptoSessionKey(sessionID, accessToken string) string {
	if sessionID != "" {
		if !validCryptoGrantSession(sessionID) {
			return ""
		}
		return sessionID
	}
	if accessToken == "" {
		return ""
	}
	return "jwt:" + HashToken(accessToken)
}

// IssueRiskGrant consumes one fresh, replay-protected code and issues a payment
// grant bound to this administrator and the authenticated JWT session. Redis
// registration is mandatory, so signed tokens alone cannot bypass revocation.
func (s *CryptoSecurityService) IssueRiskGrant(ctx context.Context, userID int64, sessionID, code string) (*CryptoVerificationGrant, error) {
	if _, err := s.CurrentOperator(ctx, userID); err != nil {
		return nil, err
	}
	if !validCryptoGrantSession(sessionID) {
		return nil, ErrCryptoAdminRequired
	}
	if _, ok := s.store.(CryptoGrantRevocationReader); !ok {
		return nil, ErrCryptoTotpUnavailable
	}
	// Record issuance before proof and Redis registration so a logout racing
	// with either phase can revoke the resulting grant by its earlier timestamp.
	issuedAt := s.totp.now().UTC()
	user, err := s.verifyAdminRiskUser(ctx, userID, code)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, ErrCryptoTotpUnavailable
	}
	// JWT-like expiry claims are in seconds; logout ordering uses milliseconds.
	now := issuedAt.Truncate(time.Second)
	claims := cryptoGrantClaims{
		Version: 1, UserID: userID,
		Session:  s.digest("grant-session", userID, sessionID),
		State:    s.grantState(user),
		Nonce:    base64.RawURLEncoding.EncodeToString(nonce),
		IssuedAt: now.Unix(), IssuedAtMS: issuedAt.UnixMilli(), ExpiresAt: now.Add(CryptoGrantTTL).Unix(),
	}
	credential, err := s.signRiskGrant(claims)
	if err != nil {
		return nil, ErrCryptoTotpUnavailable
	}
	// No plaintext authenticator, JWT, or grant is stored in Redis. This record
	// has an absolute expiry and is never refreshed by grant use.
	ttl := time.Unix(claims.ExpiresAt, 0).Sub(s.totp.now())
	if ttl <= 0 || ttl > CryptoGrantTTL {
		return nil, ErrCryptoTotpUnavailable
	}
	ok, err := s.store.SetCryptoValueIfAbsent(ctx, s.grantStoreKey(userID, credential), "1", ttl)
	if err != nil || !ok {
		return nil, ErrCryptoTotpUnavailable
	}
	return &CryptoVerificationGrant{
		Credential: credential, ExpiresAt: time.Unix(claims.ExpiresAt, 0).UTC(),
		ExpiresIn: int64(CryptoGrantTTL / time.Second),
	}, nil
}

// VerifyRiskGrant checks both the server signature and its shared registration.
// A fresh repository read invalidates grants after password/2FA changes,
// deactivation, demotion, deletion, or loss of payment-operator authority.
func (s *CryptoSecurityService) VerifyRiskGrant(ctx context.Context, userID int64, sessionID, credential string) error {
	user, err := s.CurrentOperator(ctx, userID)
	if err != nil {
		return err
	}
	if !s.ready() {
		return ErrCryptoTotpUnavailable
	}
	if !user.TotpEnabled || user.TotpSecretEncrypted == nil || *user.TotpSecretEncrypted == "" {
		return ErrCryptoTotpNotSetup
	}
	if !validCryptoGrantSession(sessionID) || len(credential) > 2048 {
		return ErrCryptoGrantRequired
	}
	parts := strings.Split(credential, ".")
	if len(parts) != 3 || parts[0] != "cpg1" {
		return ErrCryptoGrantRequired
	}
	provided, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil || len(provided) != sha256.Size || !hmac.Equal(provided, s.grantMAC(parts[1])) {
		return ErrCryptoGrantRequired
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return ErrCryptoGrantRequired
	}
	var claims cryptoGrantClaims
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&claims) != nil || decoder.Decode(new(any)) != io.EOF {
		return ErrCryptoGrantRequired
	}
	canonical, err := json.Marshal(claims)
	if err != nil || !bytes.Equal(payload, canonical) {
		return ErrCryptoGrantRequired
	}
	nonce, err := base64.RawURLEncoding.Strict().DecodeString(claims.Nonce)
	now := s.totp.now()
	if err != nil || len(nonce) != 32 || claims.Version != 1 || claims.UserID != userID ||
		claims.IssuedAt <= 0 || claims.IssuedAt > now.Unix() || claims.ExpiresAt <= now.Unix() || claims.ExpiresAt-claims.IssuedAt != int64(CryptoGrantTTL/time.Second) ||
		claims.IssuedAtMS/1000 != claims.IssuedAt || claims.IssuedAtMS > now.UnixMilli() ||
		!hmac.Equal([]byte(claims.Session), []byte(s.digest("grant-session", userID, sessionID))) ||
		!hmac.Equal([]byte(claims.State), []byte(s.grantState(user))) {
		return ErrCryptoGrantRequired
	}
	revocations, ok := s.store.(CryptoGrantRevocationReader)
	if !ok {
		return ErrCryptoTotpUnavailable
	}
	userRevokedAt, sessionRevokedAt, err := revocations.CryptoGrantRevocationTimes(ctx, userID, sessionID)
	if err != nil || userRevokedAt < 0 || sessionRevokedAt < 0 {
		return ErrCryptoTotpUnavailable
	}
	// Equality is deliberately revoked: a capability and logout in the same
	// millisecond cannot be ordered safely, so verification fails closed.
	if claims.IssuedAtMS <= userRevokedAt || claims.IssuedAtMS <= sessionRevokedAt {
		return ErrCryptoGrantRequired
	}
	exists, err := s.store.CryptoValueExists(ctx, s.grantStoreKey(userID, credential))
	if err != nil {
		return ErrCryptoTotpUnavailable
	}
	if !exists {
		return ErrCryptoGrantRequired
	}
	return nil
}

// grantState derives a keyed fingerprint from the current login credentials
// and authenticator. The caller supplies a user with an enrolled secret, and
// any change to the inputs requires a new payment grant.
func (s *CryptoSecurityService) grantState(user *User) string {
	// Password and encrypted-secret fingerprints never leave the HMAC boundary.
	// Hash each independently framed field to avoid both delimiter ambiguities
	// and fallible timestamp serialization in this authorization boundary.
	enabledAt := ""
	if user.TotpEnabledAt != nil {
		enabledAt = user.TotpEnabledAt.UTC().Format(time.RFC3339Nano)
	}
	return s.digest("grant-state", user.ID,
		s.digest("grant-email", user.ID, strings.ToLower(strings.TrimSpace(user.Email)))+
			s.digest("grant-password", user.ID, user.PasswordHash)+
			s.digest("grant-version", user.ID, fmt.Sprintf("%d", ResolvedTokenVersion(user)))+
			s.digest("grant-authenticator", user.ID, *user.TotpSecretEncrypted)+
			s.digest("grant-enabled-at", user.ID, enabledAt))
}

// validCryptoGrantSession checks that a supplied JWT session key is printable and bounded.
func validCryptoGrantSession(sessionID string) bool {
	if sessionID == "" || len(sessionID) > 512 {
		return false
	}
	for _, character := range []byte(sessionID) {
		if character <= 32 || character >= 127 {
			return false
		}
	}
	return true
}

// grantMAC authenticates canonical encoded claims in the payment-specific signature domain.
func (s *CryptoSecurityService) grantMAC(payload string) []byte {
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte("crypto-payment-grant-v1\x00" + payload))
	return mac.Sum(nil)
}

// signRiskGrant encodes claims and adds the version prefix and signature.
func (s *CryptoSecurityService) signRiskGrant(claims cryptoGrantClaims) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return "cpg1." + encoded + "." + base64.RawURLEncoding.EncodeToString(s.grantMAC(encoded)), nil
}

// grantStoreKey identifies a Redis registration without retaining the signed credential.
func (s *CryptoSecurityService) grantStoreKey(userID int64, credential string) string {
	return fmt.Sprintf("crypto:totp:{%d}:grant:%s", userID, s.digest("grant-token", userID, credential))
}
