// Package httpapi adapts deposits, wallet operations, and Bark settings to the panel HTTP API.
//
// Start here:
//   - routes.go: mounts the customer, administrator, and notification endpoints.
//   - security.go: applies operator, session, transport, and payment verification checks.
//   - handler.go: binds the shared services and handles channel configuration.
//
// File groups:
//   - deposit_*.go, address_*.go, and chain_status.go: expose deposits, address assignments, and chain status.
//   - manual_scan.go, scan_run.go, and sweep_*.go: manage settlement runs and report their progress.
//   - signer_handler.go, vault_handler.go, mnemonics.go, and private_key.go: handle signer and vault requests.
//   - grant_handler.go, review_mode.go, and audit.go: scope payment verification and attribute wallet actions.
//   - operation_notifications.go and user_bark_handler.go: connect wallet alerts and personal push settings.
package httpapi
