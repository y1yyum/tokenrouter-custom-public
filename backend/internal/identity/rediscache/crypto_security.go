package rediscache

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/TokenFlux/TokenRouter/internal/identity"
)

// crypto_security.go stores the shared payment proof and logout state in Redis.
// An attempt counter sets its expiry in the same script as the first increment,
// so an interrupted request cannot leave an unbounded budget window. Replay
// markers and enrollment locks are expiring single-writer claims. Lock
// release compares the stored owner because an expired lease can be replaced
// while its original request is still finishing.
//
// Refresh-family and account revocations publish timestamps that payment
// verification reads separately from login data. Revoking a grant requires
// only the account or session key even when the refresh index is empty.

var (
	// These scripts keep attempt reservation, owner-matched lock release,
	// and monotonic logout marker updates atomic across application processes.
	cryptoTotpAttemptScript = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
if n == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
if redis.call('PTTL', KEYS[1]) < 0 then return -1 end
return n
`)
	cryptoTotpReleaseScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0
`)
	cryptoGrantRevokeScript = redis.NewScript(`
local previous = redis.call('GET', KEYS[1])
local current = ARGV[1]
if previous and tonumber(previous) > tonumber(current) then current = previous end
redis.call('SET', KEYS[1], current, 'PX', ARGV[2])
return 1
`)
)

// IncrementCryptoAttempt reserves a payment attempt in an expiring window.
// The first increment starts the TTL. An existing counter without expiry
// returns a negative result so proof validation can deny the request.
func (c *TotpCache) IncrementCryptoAttempt(ctx context.Context, key string, ttl time.Duration) (int, error) {
	count, err := cryptoTotpAttemptScript.Run(ctx, c.rdb, []string{key}, ttl.Milliseconds()).Int()
	if err != nil {
		return 0, fmt.Errorf("increment crypto TOTP attempts: %w", err)
	}
	return count, nil
}

// SetCryptoValueIfAbsent claims a replay marker, lock, or grant registration
// for the supplied TTL. An existing key returns false and retains its value
// and expiry.
func (c *TotpCache) SetCryptoValueIfAbsent(ctx context.Context, key, value string, ttl time.Duration) (bool, error) {
	return c.rdb.SetNX(ctx, key, value, ttl).Result()
}

// CryptoValueExists reports whether a payment replay or registration marker remains.
func (c *TotpCache) CryptoValueExists(ctx context.Context, key string) (bool, error) {
	count, err := c.rdb.Exists(ctx, key).Result()
	return count != 0, err
}

// CryptoGrantRevocationTimes returns account and session logout timestamps
// in Unix milliseconds. Missing markers are zero. Malformed values and Redis
// failures are errors because either prevents a valid revocation check.
func (c *TotpCache) CryptoGrantRevocationTimes(ctx context.Context, userID int64, sessionID string) (int64, int64, error) {
	keys := []string{identity.CryptoGrantUserRevocationKey(userID), identity.CryptoGrantSessionRevocationKey(sessionID)}
	values, err := c.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return 0, 0, fmt.Errorf("read payment grant revocations: %w", err)
	}
	if len(values) != len(keys) {
		return 0, 0, fmt.Errorf("invalid payment grant revocation response")
	}
	var times [2]int64
	for index, value := range values {
		if value == nil {
			continue
		}
		raw, ok := value.(string)
		if !ok {
			return 0, 0, fmt.Errorf("invalid payment grant revocation")
		}
		stamp, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || stamp <= 0 || strconv.FormatInt(stamp, 10) != raw {
			return 0, 0, fmt.Errorf("invalid payment grant revocation")
		}
		times[index] = stamp
	}
	return times[0], times[1], nil
}

// ReleaseCryptoValue releases a payment setup lease only for its owner.
// An absent or differently owned key is already released for this request
// and remains unchanged.
func (c *TotpCache) ReleaseCryptoValue(ctx context.Context, key, value string) error {
	return cryptoTotpReleaseScript.Run(ctx, c.rdb, []string{key}, value).Err()
}

// revokeCryptoGrants records a logout for one account or session. The script
// retains the newest timestamp across concurrent writes, and the lifetime
// covers every payment grant issued before that revocation.
func (c *refreshTokenCache) revokeCryptoGrants(ctx context.Context, key string) error {
	return cryptoGrantRevokeScript.Run(ctx, c.rdb, []string{key}, time.Now().UnixMilli(), identity.CryptoGrantTTL.Milliseconds()).Err()
}
