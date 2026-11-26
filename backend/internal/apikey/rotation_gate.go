package apikey

import (
	"context"
	"fmt"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/apperror"
	"github.com/TokenFlux/TokenRouter/internal/pkg/ipmatch"
)

// rotation_gate.go evaluates whether a user's active credentials require
// remediation before chain deposits. It measures the age of each credential by
// its latest issue time and accepts a parsed IP allowlist as another mitigation.
// An enabled gate uses current database state, separate from the gateway cache
// that may still hold an earlier view of the credential.
//
// A user's explicit confirmation can admit deposits until its expiration only
// for the same configured cutoff. Checking status has no write side effects.
// The admission path records decisions that blocked a request or relied on a
// confirmation before its caller can dispatch the deposit request.

const (
	// These values form the shared console contract for status and blocked deposit errors.
	KeyRotationRequiredCode    = "KEY_ROTATION_REQUIRED"
	KeyRotationRequiredMessage = "为保护您的账户安全，请先更换 API Key 或为其设置 IP 白名单后再充值/兑换"
	KeyRotationRemediationPath = "/keys"
	KeyRotationRiskAckDisabled = "KEY_ROTATION_RISK_ACK_DISABLED"
	keyRotationRiskAckMessage  = "风险确认跳过当前不可用，请更换 API Key 或设置 IP 白名单后再充值/兑换"

	// These reasons identify the policy branch that produced the displayed deposit gate status.
	keyRotationGateDisabled     = "gate_disabled"
	keyRotationNoActiveKeys     = "no_active_keys"
	keyRotationAllKeysRotated   = "all_keys_rotated"
	keyRotationAllKeysAllowlist = "all_keys_ip_allowlisted"
	keyRotationRequired         = "rotation_required"
	keyRotationRiskAcknowledged = "risk_acknowledged"
)

// KeyRotationGateStore reads current credential state and persists deposit-gate audits.
type KeyRotationGateStore interface {
	// ListActiveCredentials returns the user's active console-managed credential facts.
	ListActiveCredentials(ctx context.Context, userID int64) ([]KeyRotationCredential, error)
	// FindLatestValidRiskAck returns a strictly unexpired confirmation for this cutoff.
	FindLatestValidRiskAck(ctx context.Context, userID int64, cutoff, now time.Time) (*time.Time, error)
	// CreateRiskAck appends a confirmation and its audit together or returns an error.
	CreateRiskAck(ctx context.Context, actor KeyRotationActor, cutoff, now, expiresAt time.Time) error
	// RecordGateDecision records a blocked deposit or one admitted by a confirmation.
	RecordGateDecision(ctx context.Context, actor KeyRotationActor, status KeyRotationGateStatus) error
}

// KeyRotationCredential carries the credential facts used to evaluate deposit compliance.
type KeyRotationCredential struct {
	IssuedAt    time.Time // latest actual credential issue
	IPAllowlist []string  // stored IP allowlist
}

// KeyRotationActor carries the authenticated user and request metadata for audit.
type KeyRotationActor struct {
	UserID    int64  // authenticated credential owner
	Method    string // inbound HTTP method
	Path      string // registered route path
	RequestID string // request correlation ID
	ClientIP  string // trusted client address
	UserAgent string // supplied client identification
}

// KeyRotationGateStatus describes the gate and remediation for one signed-in user.
type KeyRotationGateStatus struct {
	Enabled         bool      `json:"enabled"`              // cutoff currently applies
	Required        bool      `json:"required"`             // deposits need remediation
	Cutoff          time.Time `json:"cutoff"`               // configured UTC issue boundary
	Reason          string    `json:"reason"`               // machine-readable policy decision
	ErrorCode       string    `json:"error_code,omitempty"` // reason code while blocked
	Message         string    `json:"message,omitempty"`    // remediation while blocked
	RemediationPath string    `json:"remediation_path"`     // console key management path

	// Counts cover the current active credentials only. Every credential counts
	// once, and a pending credential lacks both a recent issue time and a usable allowlist.
	ActiveKeyCount    int `json:"active_key_count"`    // total evaluated credentials
	CompliantKeyCount int `json:"compliant_key_count"` // credentials meeting policy
	PendingKeyCount   int `json:"pending_key_count"`   // credentials requiring remediation

	// A confirmation changes a pending decision only while the feature is enabled
	// and the confirmation matches the cutoff and remains within its lifetime.
	RiskAckEnabled   bool       `json:"risk_ack_enabled"`              // confirmation currently offered
	RiskAckHours     int        `json:"risk_ack_hours"`                // configured lifetime in hours
	RiskAckExpiresAt *time.Time `json:"risk_ack_expires_at,omitempty"` // expiration of qualifying confirmation
}

// KeyRotationRiskAckResult reports the lifetime persisted for an explicit confirmation.
type KeyRotationRiskAckResult struct {
	ExpiresAt time.Time `json:"expires_at"` // persisted UTC expiration
	Hours     int       `json:"hours"`      // confirmation lifetime in hours
}

// KeyRotationGate applies the configured cutoff to the user's deposit eligibility.
type KeyRotationGate struct {
	store     KeyRotationGateStore // current credential and confirmation storage
	cutoff    time.Time            // configured deployment boundary
	ackOn     bool                 // operator preference for confirmations
	ackHours  int                  // confirmation lifetime in hours
	configErr error                // invalid startup configuration, if any
}

// NewKeyRotationGate binds the current store and a process configuration snapshot.
// Startup normally validates the configuration first. The gate retains validation
// errors from direct construction so a malformed cutoff cannot permit deposits.
func NewKeyRotationGate(store KeyRotationGateStore, cutoff time.Time, ackOn bool, ackHours int, cutoffErr error) *KeyRotationGate {
	return &KeyRotationGate{
		store:     store,
		cutoff:    cutoff,
		ackOn:     ackOn,
		ackHours:  ackHours,
		configErr: cutoffErr,
	}
}

// Evaluate reads the signed-in user's eligibility for display without auditing
// a deposit admission. Every active credential must satisfy the issue-time or
// allowlist rule unless the user supplied a current confirmation.
func (g *KeyRotationGate) Evaluate(ctx context.Context, userID int64) (KeyRotationGateStatus, error) {
	return g.evaluate(ctx, userID, time.Now().UTC())
}

// Check evaluates eligibility for a deposit request and persists policy audits.
// A confirmation authorizes the request only after its use has been recorded.
// The HTTP caller stops on either a required decision or an error.
func (g *KeyRotationGate) Check(ctx context.Context, actor KeyRotationActor) (KeyRotationGateStatus, error) {
	status, err := g.Evaluate(ctx, actor.UserID)
	if err != nil {
		return status, err
	}
	if status.Required || status.RiskAckExpiresAt != nil {
		if err := g.store.RecordGateDecision(ctx, actor, status); err != nil {
			return status, fmt.Errorf("record key rotation gate decision: %w", err)
		}
	}
	return status, nil
}

// Acknowledge appends an explicit confirmation for the cutoff currently in force.
// The caller verifies affirmative consent before calling. The returned expiration
// can qualify subsequent deposits only when the confirmation and audit both persist.
func (g *KeyRotationGate) Acknowledge(ctx context.Context, actor KeyRotationActor) (KeyRotationRiskAckResult, error) {
	var result KeyRotationRiskAckResult
	if err := g.validateActor(actor.UserID); err != nil {
		return result, err
	}
	// PostgreSQL stores microseconds. Truncating the clock keeps the returned
	// expiry, stored expiry, and audit time aligned across acknowledgements.
	now := time.Now().UTC().Truncate(time.Microsecond)
	if !g.activeAt(now) || !g.ackOn {
		return result, apperror.Forbidden(KeyRotationRiskAckDisabled, keyRotationRiskAckMessage)
	}
	if g.store == nil {
		return result, fmt.Errorf("key rotation gate storage unavailable")
	}
	expiresAt := now.Add(time.Duration(g.ackHours) * time.Hour)
	if err := g.store.CreateRiskAck(ctx, actor, g.cutoff, now, expiresAt); err != nil {
		return result, fmt.Errorf("persist key rotation risk acknowledgement: %w", err)
	}
	return KeyRotationRiskAckResult{ExpiresAt: expiresAt, Hours: g.ackHours}, nil
}

// evaluate applies the fixed-source status rules using a single policy instant.
// Before the configured cutoff applies, it reports a disabled gate without
// reading any credential records. An active gate counts each credential once
// even when the issue-time and allowlist rules both match. When every credential
// is compliant, it reports the allowlist reason only if all have a usable list.
// Only pending credentials require reading the optional confirmation.
func (g *KeyRotationGate) evaluate(ctx context.Context, userID int64, now time.Time) (KeyRotationGateStatus, error) {
	var status KeyRotationGateStatus
	if err := g.validateActor(userID); err != nil {
		return status, err
	}
	status = KeyRotationGateStatus{
		Enabled:         g.activeAt(now),
		Cutoff:          g.cutoff,
		RemediationPath: KeyRotationRemediationPath,
		RiskAckHours:    g.ackHours,
	}
	status.RiskAckEnabled = status.Enabled && g.ackOn
	if !status.Enabled {
		status.Reason = keyRotationGateDisabled
		return status, nil
	}
	if g.store == nil {
		return status, fmt.Errorf("key rotation gate storage unavailable")
	}

	keys, err := g.store.ListActiveCredentials(ctx, userID)
	if err != nil {
		return status, fmt.Errorf("list active API credentials: %w", err)
	}
	status.ActiveKeyCount = len(keys)
	if status.ActiveKeyCount == 0 {
		status.Reason = keyRotationNoActiveKeys
		return status, nil
	}

	allAllowlisted := true
	for _, key := range keys {
		rules := ipmatch.CompileIPRules(key.IPAllowlist)
		allowlisted := len(rules.IPs) > 0 || len(rules.CIDRs) > 0
		if allowlisted || !key.IssuedAt.Before(g.cutoff) {
			status.CompliantKeyCount++
		}
		allAllowlisted = allAllowlisted && allowlisted
	}
	status.PendingKeyCount = status.ActiveKeyCount - status.CompliantKeyCount
	if status.PendingKeyCount == 0 {
		status.Reason = keyRotationAllKeysRotated
		if allAllowlisted {
			status.Reason = keyRotationAllKeysAllowlist
		}
		return status, nil
	}

	if status.RiskAckEnabled {
		expiresAt, err := g.store.FindLatestValidRiskAck(ctx, userID, g.cutoff, now)
		if err != nil {
			return status, fmt.Errorf("read key rotation risk acknowledgement: %w", err)
		}
		if expiresAt != nil && expiresAt.After(now) {
			status.Reason = keyRotationRiskAcknowledged
			status.RiskAckExpiresAt = expiresAt
			return status, nil
		}
	}
	status.Required = true
	status.Reason = keyRotationRequired
	status.ErrorCode = KeyRotationRequiredCode
	status.Message = KeyRotationRequiredMessage
	return status, nil
}

// validateActor rejects missing authentication or an unusable gate configuration.
func (g *KeyRotationGate) validateActor(userID int64) error {
	if userID <= 0 {
		return apperror.Unauthorized("UNAUTHORIZED", "User not authenticated")
	}
	if g == nil {
		return fmt.Errorf("key rotation gate unavailable")
	}
	return g.configErr
}

// activeAt reports whether the site's configured cutoff currently applies.
func (g *KeyRotationGate) activeAt(now time.Time) bool {
	return !g.cutoff.IsZero() && !now.Before(g.cutoff)
}
