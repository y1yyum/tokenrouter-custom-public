package mode

import (
	"encoding/hex"
	"os"
	"strconv"
	"strings"
)

// instance.go reads the local deployment choices used by the payment console
// and background workers. Multiple installations can share deposit settings
// while selecting a single worker host. The installation encryption key comes
// from TokenRouter's persistent security configuration.

// InstanceEnabled reports whether this installation owns crypto worker and
// administrator duties. Unset configuration enables the source default and an
// invalid explicit value disables those duties on this process.
func InstanceEnabled() bool {
	raw := strings.TrimSpace(os.Getenv("CRYPTO_INSTANCE_ENABLED"))
	if raw == "" {
		return true
	}
	enabled, err := strconv.ParseBool(raw)
	return err == nil && enabled
}

// WorkersAllowed reports whether this process may run proactive payment work.
// The isolated administrator view suppresses worker activity even when its
// operator explicitly enables individual interactive writes.
func WorkersAllowed() bool {
	if TestInstance() {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SUB2API_DISABLE_BACKGROUND_JOBS"))) {
	case "1", "true", "yes", "on":
		return false
	default:
		return true
	}
}

// PersistentKey returns the configured installation key used to open the
// wallet vault. An implicit or malformed key leaves wallet operations
// unavailable. The persisted spelling is preserved for key derivation.
func PersistentKey(raw string, configured bool) string {
	if !configured {
		return ""
	}
	raw = strings.TrimSpace(raw)
	key, err := hex.DecodeString(raw)
	valid := err == nil && len(key) == 32
	clear(key)
	if !valid {
		return ""
	}
	return raw
}

// UserAPIReady reports whether this process can serve the deposit page with
// the configured installation key. Worker ownership is independent of this
// check, allowing several authorized frontend instances to share one scanner.
func UserAPIReady(rawKey string, configured bool) bool {
	return !ReadOnly() && PersistentKey(rawKey, configured) != ""
}
