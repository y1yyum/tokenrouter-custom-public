// Package addresses manages the HD-derived deposit address pool and the
// per-user assignment of addresses.
//
// Assignments are stored per chain FAMILY ("evm" | "solana"): one EVM
// address serves bsc/polygon/plasma, one Solana address serves solana.
//
// repo.go persists address ownership. service.go manages address allocation and rotation.
package addresses
