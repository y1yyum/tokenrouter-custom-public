package identity

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/cryptopay/mode"
	infraerrors "github.com/TokenFlux/TokenRouter/internal/pkg/apperror"
)

// crypto_security.go authorizes operators and coordinates their payment TOTP
// enrollment and proof. Deployment configuration names the eligible account
// IDs, while each request still checks the current administrator record. This
// keeps ordinary role administration separate from access to payment operations.
//
// The installation key protects session, proof, and credential digests. Redis
// shares attempt budgets, lock ownership, and consumption across processes.
// Enrollment writes the same factor that native account authentication reads,
// so overlapping requests must use the same lease and compare the current
// account before changing that factor.

const (
	// cryptoReplayTTL covers the complete three-step TOTP acceptance window,
	// while totpAttemptsTTL covers enrollment requests for the same account.
	cryptoReplayTTL = 90 * time.Second
	totpAttemptsTTL = 15 * time.Minute
)

var (
	// These errors preserve the payment HTTP contract for eligibility, proof,
	// replay, throttling, and temporary failure to verify the authenticator.
	ErrCryptoTotpRequired     = infraerrors.Forbidden("CRYPTO_TOTP_REQUIRED", "a fresh six-digit payment TOTP code is required")
	ErrCryptoTotpNotSetup     = infraerrors.Forbidden("CRYPTO_TOTP_NOT_SETUP", "bind TOTP before performing payment operations")
	ErrCryptoTotpReplayed     = infraerrors.Conflict("CRYPTO_TOTP_REPLAYED", "this verification code or setup token has already been used")
	ErrCryptoTotpRateLimited  = infraerrors.TooManyRequests("CRYPTO_TOTP_RATE_LIMITED", "too many payment verification attempts")
	ErrCryptoTotpUnavailable  = infraerrors.ServiceUnavailable("CRYPTO_TOTP_REQUIRED", "payment verification unavailable")
	ErrCryptoAdminRequired    = infraerrors.Forbidden("CRYPTO_ADMIN_REQUIRED", "an active administrator JWT session is required")
	ErrCryptoOperatorRequired = infraerrors.Forbidden("CRYPTO_OPERATOR_REQUIRED", "this account is not a configured payment administrator")
)

// CryptoTotpStore provides atomic, shared replay and enrollment controls.
type CryptoTotpStore interface {
	// IncrementCryptoAttempt claims one slot in a fixed window. The first
	// claim starts the TTL; a missing expiry or a storage failure denies proof.
	IncrementCryptoAttempt(ctx context.Context, key string, ttl time.Duration) (int, error)
	// SetCryptoValueIfAbsent claims an expiring replay or ownership marker once.
	SetCryptoValueIfAbsent(ctx context.Context, key, value string, ttl time.Duration) (bool, error)
	// CryptoValueExists checks whether a shared registration or replay marker remains.
	CryptoValueExists(ctx context.Context, key string) (bool, error)
	// ReleaseCryptoValue removes the lock only when the owner matches. An
	// expired or replaced lock is already released from the caller's perspective.
	ReleaseCryptoValue(ctx context.Context, key, value string) error
}

// CryptoSecurityStatus describes payment enrollment readiness, independently of
// the global login-TOTP and step-up switches.
type CryptoSecurityStatus struct {
	Enabled        bool `json:"enabled"`         // current account enrollment
	FeatureEnabled bool `json:"feature_enabled"` // payment verifier readiness
	CanSetup       bool `json:"can_setup"`       // allows new enrollment
	ReadOnly       bool `json:"read_only"`       // process review policy
}

// CryptoSecurityService reuses TotpService's user secret, encryption, enrollment
// and verification. Redis is mandatory; there is no local fallback or sudo grant.
type CryptoSecurityService struct {
	totp      *TotpService       // shared native authenticator
	store     CryptoTotpStore    // shared replay and grant storage
	key       []byte             // decoded installation key
	operators map[int64]struct{} // process startup account allowlist
}

// NewCryptoSecurityService joins the native authenticator to payment security
// during application construction. Call it before serving requests. Missing
// persistent key or shared storage leaves payment proof verification unavailable.
func NewCryptoSecurityService(totp *TotpService, persistentKey string) *CryptoSecurityService {
	s := &CryptoSecurityService{totp: totp, operators: cryptoOperatorIDs(os.Getenv("CRYPTO_ADMIN_USER_IDS"))}
	s.key, _ = hex.DecodeString(persistentKey)
	if totp != nil {
		s.store, _ = totp.cache.(CryptoTotpStore)
		totp.cryptoSecurity = s
	}
	return s
}

// cryptoOperatorIDs pins payment authority at process startup, independently
// of mutable admin roles. Otherwise a stolen admin JWT or machine key could
// create another administrator, enroll a fresh authenticator and bypass 2FA.
// Missing or malformed deployment configuration authorizes nobody.
func cryptoOperatorIDs(raw string) map[int64]struct{} {
	entries := strings.Split(raw, ",")
	// Cap configuration size before allocating the account allowlist.
	if len(entries) > 64 {
		return nil
	}
	ids := make(map[int64]struct{}, len(entries))
	for _, entry := range entries {
		value := strings.TrimSpace(entry)
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != value {
			return nil
		}
		ids[id] = struct{}{}
	}
	return ids
}

// ready reports whether crypto proof can use the native factor and shared store.
func (s *CryptoSecurityService) ready() bool {
	return s != nil && s.totp != nil && s.totp.encryptor != nil && s.totp.cache != nil && s.store != nil && len(s.key) == 32
}

// CurrentAdmin reads the repository on every request. Cached roles and JWT roles
// cannot retain access after deactivation, soft deletion or demotion.
func (s *CryptoSecurityService) CurrentAdmin(ctx context.Context, userID int64) (*User, error) {
	if s == nil || s.totp == nil || s.totp.userRepo == nil || userID <= 0 {
		return nil, ErrCryptoAdminRequired
	}
	user, err := s.totp.userRepo.GetByID(ctx, userID)
	if errors.Is(err, ErrUserNotFound) {
		return nil, ErrCryptoAdminRequired
	}
	if err != nil {
		return nil, ErrCryptoTotpUnavailable
	}
	if user == nil || user.ID != userID || user.DeletedAt != nil || !user.IsAdmin() || !user.IsActive() {
		return nil, ErrCryptoAdminRequired
	}
	return user, nil
}

// CurrentOperator checks the deployment allowlist and reloads the administrator.
// The caller supplies the JWT account ID after authentication and may authorize
// only the account returned by the current-role and active-state check.
func (s *CryptoSecurityService) CurrentOperator(ctx context.Context, userID int64) (*User, error) {
	if s == nil {
		return nil, ErrCryptoAdminRequired
	}
	if !s.isOperator(userID) {
		return nil, ErrCryptoOperatorRequired
	}
	return s.CurrentAdmin(ctx, userID)
}

// isOperator reports whether startup configuration names this account ID.
func (s *CryptoSecurityService) isOperator(userID int64) bool {
	if s == nil {
		return false
	}
	_, allowed := s.operators[userID]
	return allowed
}

// Status reports enrollment and payment proof readiness to the current
// operator. The payment verifier uses the account's factor independently of
// the global TOTP login and step-up switches.
func (s *CryptoSecurityService) Status(ctx context.Context, userID int64) (*CryptoSecurityStatus, error) {
	u, err := s.CurrentOperator(ctx, userID)
	if err != nil {
		return nil, err
	}
	enabled := u.TotpEnabled && u.TotpSecretEncrypted != nil && *u.TotpSecretEncrypted != ""
	return &CryptoSecurityStatus{
		Enabled: enabled, FeatureEnabled: s.ready(),
		CanSetup: !u.TotpEnabled && s.ready() && !mode.ReadOnly(),
		ReadOnly: mode.ReadOnly(),
	}, nil
}

// limit reserves a payment attempt in the action's Redis window. Requests
// beyond max, with an unusable counter, or during a storage outage cannot
// continue to proof validation.
func (s *CryptoSecurityService) limit(ctx context.Context, userID int64, action string, max int, ttl time.Duration) error {
	if !s.ready() {
		return ErrCryptoTotpUnavailable
	}
	key := fmt.Sprintf("crypto:totp:{%d}:attempts:%s", userID, action)
	n, err := s.store.IncrementCryptoAttempt(ctx, key, ttl)
	if err != nil || n < 1 {
		return ErrCryptoTotpUnavailable
	}
	if n > max {
		return ErrCryptoTotpRateLimited
	}
	return nil
}

// digest binds one value to its purpose and account under the installation key.
func (s *CryptoSecurityService) digest(purpose string, userID int64, value string) string {
	mac := hmac.New(sha256.New, s.key)
	fmt.Fprintf(mac, "%s\x00%d\x00%s", purpose, userID, value)
	return hex.EncodeToString(mac.Sum(nil))
}

// consume registers one use of the code for its matched time step. A lost
// claim reports replay and an unavailable shared store prevents proof admission.
func (s *CryptoSecurityService) consume(ctx context.Context, userID int64, counter int64, code string) error {
	// Counter is the matched TOTP time step, not the request arrival bucket.
	digest := s.digest("code", userID, fmt.Sprintf("%d:%s", counter, code))
	key := fmt.Sprintf("crypto:totp:{%d}:used:%s", userID, digest)
	ok, err := s.store.SetCryptoValueIfAbsent(ctx, key, "1", cryptoReplayTTL)
	if err != nil {
		return ErrCryptoTotpUnavailable
	}
	if !ok {
		return ErrCryptoTotpReplayed
	}
	return nil
}

// cryptoTotpError translates native proof errors into the payment HTTP contract.
func cryptoTotpError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrTotpNotSetup):
		return ErrCryptoTotpNotSetup
	case errors.Is(err, ErrTotpTooManyAttempts):
		return ErrCryptoTotpRateLimited
	case errors.Is(err, ErrTotpInvalidCode), errors.Is(err, ErrTotpSetupExpired):
		return ErrCryptoTotpRequired
	case errors.Is(err, ErrTotpAlreadyEnabled), errors.Is(err, ErrUserSecurityStateChanged):
		return ErrCryptoTotpReplayed
	case errors.Is(err, ErrCryptoAdminRequired), errors.Is(err, ErrCryptoTotpRequired), errors.Is(err, ErrCryptoTotpReplayed):
		return err
	default:
		return ErrCryptoTotpUnavailable
	}
}

// VerifyRisk requires a new code for each operation, even when login TOTP or
// step-up is disabled or a valid step-up grant already exists.
func (s *CryptoSecurityService) VerifyRisk(ctx context.Context, userID int64, code string) error {
	if _, err := s.CurrentOperator(ctx, userID); err != nil {
		return err
	}
	return s.verifyAdminRisk(ctx, userID, code)
}

// verifyAdminRisk consumes a fresh code for the current administrator account.
func (s *CryptoSecurityService) verifyAdminRisk(ctx context.Context, userID int64, code string) error {
	_, err := s.verifyAdminRiskUser(ctx, userID, code)
	return err
}

// verifyAdminRiskUser reloads the current administrator and consumes proof
// for that snapshot. A grant fingerprints it, and disabling a factor passes
// its role and secret to the conditional account write.
func (s *CryptoSecurityService) verifyAdminRiskUser(ctx context.Context, userID int64, code string) (*User, error) {
	u, err := s.CurrentAdmin(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !u.TotpEnabled || u.TotpSecretEncrypted == nil || *u.TotpSecretEncrypted == "" {
		return nil, ErrCryptoTotpNotSetup
	}
	if err := s.limit(ctx, userID, "verify", 30, time.Minute); err != nil {
		return nil, err
	}
	err = s.totp.verifyPaymentCode(ctx, u, code, func(counter int64) error {
		return s.consume(ctx, userID, counter, code)
	})
	if err != nil {
		return nil, cryptoTotpError(err)
	}
	return u, nil
}

// lockSetup leases enrollment across processes using a random owner token.
// Competing setup, enable, or disable calls cannot mutate the factor while
// the lease is held. Callers defer cleanup, which checks ownership and leaves
// a later claimant's lease intact. The database condition on enable or disable
// still applies if a request outlives its lease, and an interrupted cleanup can
// rely on Redis expiry.
func (s *CryptoSecurityService) lockSetup(ctx context.Context, userID int64) (func(), error) {
	if !s.ready() {
		return nil, ErrCryptoTotpUnavailable
	}
	token, err := GenerateRandomToken(32)
	if err != nil {
		return nil, ErrCryptoTotpUnavailable
	}
	key := fmt.Sprintf("crypto:totp:{%d}:setup-lock", userID)
	ok, err := s.store.SetCryptoValueIfAbsent(ctx, key, token, 30*time.Second)
	if err != nil {
		return nil, ErrCryptoTotpUnavailable
	}
	if !ok {
		return nil, ErrCryptoTotpReplayed
	}
	return func() {
		// Expiry is the safe fallback if cleanup fails or the caller disconnects.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		_ = s.store.ReleaseCryptoValue(cleanup, key, token)
	}, nil
}

// InitiateSetup binds payment TOTP enrollment to the authenticated JWT session.
// The setup token and first TOTP code prove possession of the new authenticator.
func (s *CryptoSecurityService) InitiateSetup(ctx context.Context, userID int64, sessionID string) (*TotpSetupResponse, error) {
	if _, err := s.CurrentOperator(ctx, userID); err != nil {
		return nil, err
	}
	if err := s.limit(ctx, userID, "setup", 5, totpAttemptsTTL); err != nil {
		return nil, err
	}
	if !validCryptoGrantSession(sessionID) {
		return nil, ErrCryptoAdminRequired
	}
	unlock, err := s.lockSetup(ctx, userID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	result, err := s.totp.initiateSetup(ctx, userID, "", "", s.digest("session", userID, sessionID), true)
	return result, cryptoTotpError(err)
}

// Enable completes the shared enrollment with a six-digit code, never a
// client-supplied enabled flag, and consumes both the code and setup capability.
func (s *CryptoSecurityService) Enable(ctx context.Context, userID int64, sessionID, code, setupToken string) error {
	if _, err := s.CurrentOperator(ctx, userID); err != nil {
		return err
	}
	if err := s.limit(ctx, userID, "enable", 10, totpAttemptsTTL); err != nil {
		return err
	}
	if !validCryptoGrantSession(sessionID) {
		return ErrCryptoAdminRequired
	}
	unlock, err := s.lockSetup(ctx, userID)
	if err != nil {
		return err
	}
	defer unlock()
	usedToken := fmt.Sprintf("crypto:totp:{%d}:setup-used:%s", userID, s.digest("setup-token", userID, sessionID+":"+setupToken))
	used, err := s.store.CryptoValueExists(ctx, usedToken)
	if err != nil {
		return ErrCryptoTotpUnavailable
	}
	if used {
		return ErrCryptoTotpReplayed
	}
	return cryptoTotpError(s.totp.completeSetup(ctx, userID, code, setupToken, s.digest("session", userID, sessionID), func(counter int64) error {
		if err := s.consume(ctx, userID, counter, code); err != nil {
			return err
		}
		ok, err := s.store.SetCryptoValueIfAbsent(ctx, usedToken, "1", totpSetupTTL)
		if err != nil {
			return ErrCryptoTotpUnavailable
		}
		if !ok {
			return ErrCryptoTotpReplayed
		}
		return nil
	}))
}
