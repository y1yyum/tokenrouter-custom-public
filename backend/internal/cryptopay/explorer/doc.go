// Package explorer talks to Etherscan-compatible account APIs (Etherscan v2,
// BscScan v2, and any host exposing the same module/action contract).
//
// It exists because a native-coin (ETH/BNB) transfer is only visible in a
// transaction, never in a log. Reading every block with full transaction bodies
// costs one request per block — 28800 a day on BSC — which is what a provider
// package counts. One txlist call per address returns every ordinary
// transaction that touched that address, so the request count scales with the
// number of watched addresses and the polling interval instead of with the
// chain's block rate.
//
// txlist.go reads transaction pages for one address at a time.
package explorer
