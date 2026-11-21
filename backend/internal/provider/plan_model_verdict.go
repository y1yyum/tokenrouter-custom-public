package provider

import "time"

// plan_model_verdict.go carries explicit quality decisions between the scorer,
// durable result storage and read-only scheduling consumers. Transport errors
// have no verdict. The store resolves the plan owner and start watermark from
// the already saved test result.

// PlanModelVerdict describes a grade for a saved successful test exchange.
// A pass clears only this plan's restriction for the same actual model.
type PlanModelVerdict struct {
	ResultID       int64      // retained scheduled result that supplied the response
	PlanID         int64      // plan that produced the saved result
	ProviderID     int64      // provider that actually served the probe
	PolicyVersion  int64      // monotonic version of the quality policy
	RequestedModel string     // plan's input model at probe start
	UpstreamModel  string     // actual outbound model observed by the probe
	Restricted     bool       // true for a failing quality grade
	Reason         string     // diagnostic reason for the grade
	ExpiresAt      *time.Time // optional deadline for applying this restriction
}

// ActivePlanModelRestriction identifies one owner of a current model restriction.
// Read callers can show every active owner without changing provider health.
type ActivePlanModelRestriction struct {
	PlanModelVerdict
	StartedAt time.Time // persisted probe start used for ordering concurrent results
}
