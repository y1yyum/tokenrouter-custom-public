// Package notify delivers crypto wallet operation events to the operator's
// channels: a generic JSON webhook (Bark-compatible) and email.
//
// Delivery is strictly best effort. A crypto signature, sweep or configuration
// change must never fail or stall because a phone or a mail server is down, so
// events are handed to a bounded queue and delivered by a background worker.
// Delivery failures are logged and dropped; they never reach the caller.
//
// notify.go delivers chain alerts to the configured sinks.
package notify
