// Package cryptopay implements the built-in crypto deposit channel:
// per-user HD deposit addresses, on-chain scanning, crediting and sweeps.
//
// Everything in this package is additive and disabled by default; when
// CryptoConfig.Enabled is false the process behaves exactly as before.
//
// Start with config.go for validation and service.go for runtime ownership. Deposit confirmation joins chain evidence to the payment ledger in deposit_confirm.go.
package cryptopay
