// Package scanner watches configured chains for incoming deposits.
//
// worker.go coordinates polling. evm.go reads EVM deposits and reconcile.go revisits observations before finality.
package scanner
