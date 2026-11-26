// Package ledger turns observed on-chain transfers into credited balances.
//
// Safety rails:
//   - unique (chain, tx_hash, log_index) makes crediting idempotent;
//   - transfers FROM our own addresses (sweeps, gas top-ups) are excluded so
//     the system never credits itself.
//
// identity.go builds deposit identifiers; ledger.go records credit and reversal decisions.
package ledger
