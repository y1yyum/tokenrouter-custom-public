package identity

import (
	"context"
	"crypto/subtle"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"github.com/TokenFlux/TokenRouter/internal/pkg/apperror"
)

// totp_payment_account.go connects the native authenticator to payment proof.
// Payment enrollment and general administrator enrollment share a Redis lease
// because both can replace the same pending setup. A setup stores the session
// digest with the encrypted seed and requires both that session and the
// authenticator proof on completion.
//
// Redis consumes the proof before the database update. A database failure
// therefore requires new enrollment proof. The database predicate still
// protects against replacing an enabled factor after a lease expires.
// Verification uses the actual matched time step so crossing a period
// boundary cannot produce a second claim for the same code.

var (
	// ErrUserSecurityStateChanged reports a concurrent change to account state
	// that prevented the conditional authenticator write from taking effect.
	ErrUserSecurityStateChanged = apperror.Conflict("USER_SECURITY_STATE_CHANGED", "account security state changed")
)

// totpAtomicBinder installs an authenticator under the current administrator state.
type totpAtomicBinder interface {
	// BindTotpIfDisabled sets ciphertext and enablement together for a current
	// active administrator. A missing predicate match reports changed state,
	// so a caller cannot replace an authenticator after another request enables it.
	BindTotpIfDisabled(ctx context.Context, userID int64, encryptedSecret string) error
}

// totpConditionalDisabler clears only the authenticator whose state was verified.
type totpConditionalDisabler interface {
	// DisableTotpWithExpectedRole compares the supplied role and encrypted
	// secret with the active account before clearing its factor. A concurrent
	// role or secret change requires the caller to obtain new verification.
	DisableTotpWithExpectedRole(ctx context.Context, userID int64, role string, secret *string) error
}

// InitiateSetupForSession starts account enrollment with the native identity
// proof. When payment verification is configured, administrator enrollment
// also requires the validated JWT session key and shares the payment setup lease.
func (s *TotpService) InitiateSetupForSession(ctx context.Context, userID int64, emailCode, password, sessionID string) (*TotpSetupResponse, error) {
	if !s.settingService.IsTotpEnabled(ctx) {
		return nil, ErrTotpNotEnabled
	}
	binding, unlock, err := s.lockAdminEnrollment(ctx, userID, sessionID, "setup", 5)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.initiateSetup(ctx, userID, emailCode, password, binding, false)
}

// CompleteSetupForSession completes native enrollment with the authenticator
// code and setup token. Bound administrator challenges require the same
// authenticated session key and consume proof with the shared payment controls.
func (s *TotpService) CompleteSetupForSession(ctx context.Context, userID int64, code, setupToken, sessionID string) error {
	if !s.settingService.IsTotpEnabled(ctx) {
		return ErrTotpNotEnabled
	}
	binding, unlock, err := s.lockAdminEnrollment(ctx, userID, sessionID, "enable", 10)
	if err != nil {
		return err
	}
	defer unlock()
	if binding == "" {
		return s.CompleteSetup(ctx, userID, code, setupToken)
	}
	return s.completeSetup(ctx, userID, code, setupToken, binding, func(counter int64) error {
		return s.cryptoSecurity.consume(ctx, userID, counter, code)
	})
}

// lockAdminEnrollment selects and locks the bound path for a current
// administrator. With no active payment verifier or for an ordinary account,
// the returned empty binding and no-op cleanup select native enrollment.
func (s *TotpService) lockAdminEnrollment(ctx context.Context, userID int64, sessionID, action string, max int) (string, func(), error) {
	security := s.cryptoSecurity
	if !security.ready() {
		return "", func() {}, nil
	}
	user, err := s.userRepo.GetByID(ctx, userID)
	if err != nil || user == nil {
		return "", nil, ErrTotpUnavailable
	}
	if !user.IsAdmin() {
		return "", func() {}, nil
	}
	if !validCryptoGrantSession(sessionID) || !user.IsActive() || user.DeletedAt != nil {
		return "", nil, ErrCryptoAdminRequired
	}
	if err := security.limit(ctx, userID, action, max, totpAttemptsTTL); err != nil {
		return "", nil, err
	}
	unlock, err := security.lockSetup(ctx, userID)
	if err != nil {
		return "", nil, err
	}
	return security.digest("session", userID, sessionID), unlock, nil
}

// completeSetup consumes a bound challenge and conditionally installs its
// authenticator. The caller holds the shared setup lease and supplies the
// expected session digest and replay consumer after authenticating the JWT.
//
// The account and setup record must still be eligible at completion. A
// repository without the conditional write cannot enable this factor.
// Removing the setup before the write ensures a cache failure cannot enable
// an authenticator while leaving its challenge available for another attempt.
func (s *TotpService) completeSetup(ctx context.Context, userID int64, code, token, binding string, consume func(int64) error) error {
	binder, ok := s.userRepo.(totpAtomicBinder)
	if !ok {
		return ErrTotpUnavailable
	}
	if err := s.reserveCryptoVerifyAttempt(ctx, userID); err != nil {
		return err
	}
	session, err := s.cache.GetSetupSession(ctx, userID)
	if err != nil {
		return ErrTotpUnavailable
	}
	now := s.now()
	// The timestamp check keeps expired or implausibly future challenges from
	// being accepted even if they survive beyond their configured Redis lifetime.
	if session == nil || !session.SecretEncrypted || binding == "" || session.CryptoSession != binding ||
		now.Sub(session.CreatedAt) >= totpSetupTTL || session.CreatedAt.After(now.Add(time.Minute)) ||
		subtle.ConstantTimeCompare([]byte(session.SetupToken), []byte(token)) != 1 {
		return ErrTotpSetupExpired
	}
	user, err := s.cryptoSecurity.CurrentAdmin(ctx, userID)
	if err != nil {
		return err
	}
	if user.TotpEnabled {
		return ErrTotpAlreadyEnabled
	}
	secret, err := s.encryptor.Decrypt(session.Secret)
	if err != nil {
		return ErrTotpUnavailable
	}
	counter, valid := validateTotpCode(code, secret, now)
	if !valid {
		return ErrTotpInvalidCode
	}
	// Each claimant consumes the matched counter before the database sees an
	// enrollment update, and cache failures stop the update before persistence.
	if err := consume(counter); err != nil {
		return err
	}
	if err := s.cache.DeleteSetupSession(ctx, userID); err != nil {
		return ErrTotpUnavailable
	}
	if err := s.cache.ClearVerifyAttempts(ctx, userID); err != nil {
		return ErrTotpUnavailable
	}
	return binder.BindTotpIfDisabled(ctx, userID, session.Secret)
}

// disablePaymentTotp consumes a new authenticator code before clearing the
// same factor and role that were verified. The enrollment lease coordinates
// with setup, and the conditional write protects changes after lease expiry.
func (s *TotpService) disablePaymentTotp(ctx context.Context, userID int64, code string) error {
	security := s.cryptoSecurity
	unlock, err := security.lockSetup(ctx, userID)
	if err != nil {
		return err
	}
	defer unlock()
	disabler, ok := s.userRepo.(totpConditionalDisabler)
	if !ok {
		return ErrTotpUnavailable
	}
	user, err := security.verifyAdminRiskUser(ctx, userID, code)
	if err != nil {
		return err
	}
	// A persisted change to the encrypted factor also invalidates outstanding
	// payment grants, which compare the current account state on each use.
	return disabler.DisableTotpWithExpectedRole(ctx, userID, user.Role, user.TotpSecretEncrypted)
}

// verifyPaymentCode verifies the current account secret and atomically
// consumes its matched time step. It reserves an account attempt before
// validation, so concurrent attempts each count toward the failure budget.
func (s *TotpService) verifyPaymentCode(ctx context.Context, user *User, code string, consume func(int64) error) error {
	if err := s.reserveCryptoVerifyAttempt(ctx, user.ID); err != nil {
		return err
	}
	secret, err := s.encryptor.Decrypt(*user.TotpSecretEncrypted)
	if err != nil {
		return ErrTotpUnavailable
	}
	counter, valid := validateTotpCode(code, secret, s.now())
	if !valid {
		return ErrTotpInvalidCode
	}
	// Consume before clearing attempts so replay of a known code cannot reset
	// the account budget while another caller is attempting verification.
	if err := consume(counter); err != nil {
		return err
	}
	if err := s.cache.ClearVerifyAttempts(ctx, user.ID); err != nil {
		return ErrTotpUnavailable
	}
	return nil
}

// reserveCryptoVerifyAttempt claims a slot in the existing account budget.
// A read or increment failure denies proof, and the increment result closes
// the race between callers that observed the same available slot.
func (s *TotpService) reserveCryptoVerifyAttempt(ctx context.Context, userID int64) error {
	attempts, err := s.cache.GetVerifyAttempts(ctx, userID)
	if err != nil {
		return ErrTotpUnavailable
	}
	if attempts >= maxTotpAttempts {
		return ErrTotpTooManyAttempts
	}
	attempts, err = s.cache.IncrementVerifyAttempts(ctx, userID)
	if err != nil || attempts < 1 {
		return ErrTotpUnavailable
	}
	if attempts > maxTotpAttempts {
		return ErrTotpTooManyAttempts
	}
	return nil
}

// validateTotpCode returns the exact counter of a matching six-digit SHA-1
// code. It tries the current step, then one step on either side, so replay
// keys follow the time step of the proof itself.
func validateTotpCode(code, secret string, now time.Time) (int64, bool) {
	if len(code) != 6 {
		return 0, false
	}
	for i := range code {
		if code[i] < '0' || code[i] > '9' {
			return 0, false
		}
	}
	// A thirty-second period matches the defaults used by enrollment.
	counter := now.Unix() / 30
	for _, offset := range []int64{0, -1, 1} {
		candidate := counter + offset
		valid, err := totp.ValidateCustom(code, secret, time.Unix(candidate*30, 0), totp.ValidateOpts{
			Period: 30, Skew: 0, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
		})
		if err == nil && valid {
			return candidate, true
		}
	}
	return 0, false
}
