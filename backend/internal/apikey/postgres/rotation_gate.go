package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/apikey"
)

// rotation_gate.go loads the minimum stored credential facts and persists
// risk confirmations and deposit decisions. The credential read uses the
// owner on the API key row and never selects its secret. A new confirmation
// and its management audit share one transaction so a confirmation cannot
// take effect if its audit failed. Decision audits use the same management
// table and complete before a confirmation-dependent deposit can proceed.

const (
	// These actions match the fixed source and identify confirmation creation, denial, and temporary admission in audit searches.
	keyRotationAckAction     = "KEY_ROTATION_RISK_ACK"
	keyRotationBlockedAction = "user.recharge.blocked_key_rotation"
	keyRotationAllowedAction = "user.recharge.allowed_key_rotation_risk_ack"
)

// KeyRotationGateStore uses the host PostgreSQL database for deposit eligibility.
type KeyRotationGateStore struct {
	db *sql.DB // shared application database connection pool
}

// keyRotationAuditWriter accepts the pool or the confirmation's open transaction.
type keyRotationAuditWriter interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// NewKeyRotationGateStore binds current credentials, confirmations, and audits to PostgreSQL.
func NewKeyRotationGateStore(db *sql.DB) *KeyRotationGateStore {
	return &KeyRotationGateStore{db: db}
}

// ListActiveCredentials loads all active console-managed keys for the owner.
// The SQL read bypasses cached authentication and excludes hidden managed
// keys, which console users cannot rotate or update.
func (s *KeyRotationGateStore) ListActiveCredentials(ctx context.Context, userID int64) ([]apikey.KeyRotationCredential, error) {
	if s.db == nil {
		return nil, fmt.Errorf("key rotation gate database unavailable")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT GREATEST(created_at, key_rotated_at), COALESCE(ip_whitelist, '[]'::jsonb)
		FROM api_keys
		WHERE user_id = $1 AND status = $2 AND deleted_at IS NULL
		  AND managed_by IS NULL AND NOT team_owner_disabled`, userID, apikey.StatusActive)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var keys []apikey.KeyRotationCredential
	for rows.Next() {
		var key apikey.KeyRotationCredential
		var allowlist []byte
		if err := rows.Scan(&key.IssuedAt, &allowlist); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(allowlist, &key.IPAllowlist); err != nil {
			return nil, fmt.Errorf("decode API key allowlist: %w", err)
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// FindLatestValidRiskAck returns the furthest unexpired acknowledgement for the
// current cutoff. A confirmation for a different cutoff cannot take precedence,
// even when that confirmation has a later expiration.
func (s *KeyRotationGateStore) FindLatestValidRiskAck(ctx context.Context, userID int64, cutoff, now time.Time) (*time.Time, error) {
	var expiresAt time.Time
	err := s.db.QueryRowContext(ctx, `
		SELECT expires_at
		FROM key_rotation_risk_acks
		WHERE user_id = $1 AND expires_at > $2 AND cutoff_snapshot = $3
		ORDER BY expires_at DESC
		LIMIT 1`, userID, now, cutoff).Scan(&expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	utc := expiresAt.UTC()
	return &utc, nil
}

// CreateRiskAck appends an acknowledgement and its corresponding audit record.
// Audit insertion shares the same transaction, and a failure returns before
// commit so subsequent deposit checks cannot observe an unaudited acknowledgement.
func (s *KeyRotationGateStore) CreateRiskAck(ctx context.Context, actor apikey.KeyRotationActor, cutoff, now, expiresAt time.Time) error {
	if s.db == nil {
		return fmt.Errorf("key rotation gate database unavailable")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// An unsuccessful insert or cancelled request leaves neither the confirmation
	// nor the audit visible to another transaction after returning to the client.
	defer func() { _ = tx.Rollback() }()
	var id int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO key_rotation_risk_acks
			(user_id, acknowledged_at, expires_at, client_ip, user_agent, cutoff_snapshot, created_at)
		VALUES ($1, $2, $3, LEFT($4, 64), LEFT($5, 512), $6, $2)
		RETURNING id`, actor.UserID, now, expiresAt, actor.ClientIP, actor.UserAgent, cutoff).Scan(&id)
	if err != nil {
		return fmt.Errorf("insert risk acknowledgement: %w", err)
	}
	extra := map[string]any{
		"ack_id":      id,
		"auth_method": "jwt",
		"cutoff":      cutoff,
		"expires_at":  expiresAt,
		"ip":          actor.ClientIP,
		"user_id":     actor.UserID,
	}
	if err := writeKeyRotationAudit(ctx, tx, actor, keyRotationAckAction, http.StatusOK, extra); err != nil {
		return fmt.Errorf("insert risk acknowledgement audit: %w", err)
	}
	return tx.Commit()
}

// RecordGateDecision stores a denial or a confirmation-dependent admission.
// Admission refers to the gate decision itself, since the deposit handler has
// not run when the row is written.
func (s *KeyRotationGateStore) RecordGateDecision(ctx context.Context, actor apikey.KeyRotationActor, status apikey.KeyRotationGateStatus) error {
	extra := map[string]any{
		"active_key_count":  status.ActiveKeyCount,
		"cutoff":            status.Cutoff,
		"ip":                actor.ClientIP,
		"path":              actor.Path,
		"pending_key_count": status.PendingKeyCount,
		"reason":            status.Reason,
		"user_id":           actor.UserID,
	}
	if status.Required {
		extra["error_code"] = status.ErrorCode
		extra["remediation_path"] = status.RemediationPath
		return writeKeyRotationAudit(ctx, s.db, actor, keyRotationBlockedAction, http.StatusForbidden, extra)
	}
	extra["risk_ack_expires_at"] = status.RiskAckExpiresAt
	return writeKeyRotationAudit(ctx, s.db, actor, keyRotationAllowedAction, http.StatusOK, extra)
}

// writeKeyRotationAudit persists the source-compatible action in host audit logs.
// Request metadata excludes the request body and credential. PostgreSQL limits
// path, client IP, and correlation fields to the host audit schema's widths.
func writeKeyRotationAudit(ctx context.Context, db keyRotationAuditWriter, actor apikey.KeyRotationActor, action string, code int, extra map[string]any) error {
	data, err := json.Marshal(extra)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO audit_logs
			(actor_user_id, auth_method, action, method, path, request_id,
			 client_ip, user_agent, status_code, extra)
		VALUES ($1, 'jwt', $2, $3, LEFT($4, 512), LEFT($5, 64),
			LEFT($6, 64), LEFT($7, 512), $8, $9::jsonb)`,
		actor.UserID, action, actor.Method, actor.Path, actor.RequestID,
		actor.ClientIP, actor.UserAgent, code, string(data))
	return err
}
