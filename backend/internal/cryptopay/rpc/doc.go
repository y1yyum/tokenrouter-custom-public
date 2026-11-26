// Package rpc provides a rotating JSON-RPC client pool for the deposit
// scanners. Endpoints come from admin settings; the pool never hardcodes
// any provider.
//
// pool.go selects endpoints and records their health. errors.go classifies failures for retries and diagnostics.
package rpc
