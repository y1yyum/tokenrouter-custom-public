// Package sweeper plans and (later) executes address sweeps.
//
// Phase 4 scope: threshold evaluation and plan generation. Execution and
// gas top-up are handled by the durable executor.
//
// planner.go builds a sweep plan and executor.go submits it. recovery.go reconciles unfinished jobs.
package sweeper
