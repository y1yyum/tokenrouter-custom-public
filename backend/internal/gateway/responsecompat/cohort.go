package responsecompat

import (
	"context"
	"time"
)

// cohort.go defines the two request populations used to evaluate compatibility
// policies. The first matched rule owns the cohort even when the final provider
// changes. The end of the HTTP request supplies the final result independently
// of the number of upstream attempts or billing commands.

const (
	ResponsesPhase = 1 // enrollment after mapping a native Responses candidate
	ParameterPhase = 2 // enrollment at the original cross-protocol entry

	// Outcomes are persisted client-visible classifications for both phases.
	OutcomeCompleted    = "completed"
	OutcomeIncomplete   = "incomplete"
	OutcomeRefused      = "refused"
	OutcomeParameter400 = "parameter_400"
	OutcomeOverload503  = "overload_503"
	OutcomeProtocol502  = "protocol_502"
	OutcomeTimeout504   = "timeout_504"
	OutcomeClientGone   = "client_gone"
	OutcomeOtherFailure = "other_failure"

	// Rejection sources distinguish the stages where a final parameter rejection is decided.
	RejectionEntry     = "entry"
	RejectionCandidate = "candidate"
	RejectionUpstream  = "upstream"
)

// Cohort identifies the first rule and arm chosen for a local logical request.
// Parameter policy uses the requested model and entry endpoint in Tuple and
// leaves its provider ID zero because selection has not occurred yet.
type Cohort struct {
	Phase        int
	RequestID    string
	EnrolledAt   time.Time
	Revision     int64
	Arm          Mode
	Tuple        Tuple
	ClientStream bool
}

// CohortResult freezes the single client-visible classification. Applied
// records whether the enrolled policy actually changed any outbound attempt.
// Duration measures ingress to client completion using the caller's clock.
type CohortResult struct {
	Outcome              string
	Applied              bool
	TargetParameterError bool
	RejectionSource      string // phase two parameter rejection stage
	// FinalRevision holds the last Responses policy evaluation. Zero means
	// the evaluation had no fresh authoritative snapshot.
	FinalRevision int64
	FinishedAt    time.Time
	Duration      time.Duration
}

// CohortRecorder persists first admission and one final result. Finish receives
// the enrollment as well so that it can repair a missing in-flight record
// following a transient monitoring failure without duplicating the request.
type CohortRecorder interface {
	Enroll(context.Context, Cohort) error
	Finish(context.Context, Cohort, CohortResult) error
}
