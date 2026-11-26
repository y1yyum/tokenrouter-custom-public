// Package wallet embeds the encrypted deposit and gas wallets in the gateway.
// It never opens a listener or uses the former signer's policy or budget store.
//
// manager.go owns signer state. vault.go handles encrypted wallet envelopes and sign_evm.go signs EVM transactions.
package wallet
