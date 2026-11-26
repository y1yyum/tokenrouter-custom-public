package config

import (
	"fmt"
	"strings"
	"time"
)

// key_rotation.go validates the site-specific cutoff and confirmation lifetime
// used by chain deposit screening. The cutoff has PostgreSQL timestamp precision
// so a stored confirmation can be compared with the configured instant exactly.

// defaultKeyRotationRiskAckHours applies when file or direct construction omits the lifetime.
const defaultKeyRotationRiskAckHours = 24

// KeyRotationGateCutoff returns the site's validated cutoff in UTC, or zero
// for an empty cutoff. It also validates the configured confirmation lifetime.
// Startup and the gate use the same rules so invalid input cannot silently
// deactivate screening or authorize a confirmation for an ambiguous instant.
func (s SecurityConfig) KeyRotationGateCutoff() (time.Time, error) {
	if hours := s.KeyRotationRiskAckLifetimeHours(); hours < 1 || hours > 720 {
		return time.Time{}, fmt.Errorf("security.key_rotation_risk_ack_hours must be between 1 and 720")
	}
	raw := strings.TrimSpace(s.KeyRotationCutoff)
	if raw == "" {
		return time.Time{}, nil
	}
	cutoff, err := time.Parse(time.RFC3339, raw)
	if err != nil || cutoff.IsZero() || !cutoff.Equal(cutoff.Truncate(time.Microsecond)) {
		return time.Time{}, fmt.Errorf("security.key_rotation_cutoff must be a nonzero RFC3339 timestamp with microsecond precision or coarser")
	}
	return cutoff.UTC(), nil
}

// KeyRotationRiskAckLifetimeHours defaults an omitted confirmation lifetime to 24 hours.
func (s SecurityConfig) KeyRotationRiskAckLifetimeHours() int {
	if s.KeyRotationRiskAckHours == 0 {
		return defaultKeyRotationRiskAckHours
	}
	return s.KeyRotationRiskAckHours
}
