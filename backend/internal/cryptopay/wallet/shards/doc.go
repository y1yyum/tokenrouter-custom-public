// Package shards implements a versioned 2-of-3 Shamir secret sharing scheme
// over GF(256). It is the wire-compatible sibling of the browser implementation
// in the admin frontend: both sides must produce identical share strings.
//
// It is deliberately not SLIP-39: SLIP-39 adds a passphrase, Feistel
// encryption and a 1024-word wordlist, and no audited Go + browser pair
// exists for byte-identical output. This scheme keeps the security-relevant
// properties (GF(256) Shamir sharing, threshold 2 of 3, versioned share with a
// checksum, order-independent recovery) while staying small enough to audit on
// both sides.
//
// Share text format:
//
//	S2V1-XXXX-XXXX-... (base32, RFC 4648, no padding, grouped in fours)
//
// Decoded payload:
//
//	version(1) | threshold(1) | total(1) | index(1) | dataLen(1) | data(n) | checksum(4)
//
// The checksum is the first four bytes of SHA-256 over everything before it.
// A share is only useful together with one other share from the same split;
// a single share reveals nothing about the secret.
//
// shards.go splits, verifies and combines recovery shares.
package shards
