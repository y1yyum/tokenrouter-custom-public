// Package nativeprice resolves the USD price of a chain's native coin.
//
// Native deposits must be credited at the market price observed when the
// deposit is settled: a constant multiplier cannot tell 1 ETH from 1 USDT.
// The provider is deliberately fail-closed. It returns an error instead of a
// default, a zero or a quote older than MaxQuoteAge, and the ledger leaves the
// deposit pending for the next pass. Crediting on a guessed price is worse
// than crediting a few minutes late.
//
// provider.go obtains and caches native-token quotes used to value deposits.
package nativeprice
