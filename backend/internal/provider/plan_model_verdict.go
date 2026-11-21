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

// ModelQualityReport keeps raw counts so operators can reproduce every ratio.
// Usage and terminal provider errors describe observed user traffic. Synthetic
// outcomes come from the audit ledger that survives result retention.
type ModelQualityReport struct {
	From         time.Time           `json:"from"`          // inclusive UTC start
	Until        time.Time           `json:"until"`         // exclusive UTC end and expiry reference
	ProviderID   int64               `json:"provider_id"`   // selected provider
	Model        string              `json:"model"`         // actual upstream identity
	Success      int64               `json:"success"`       // observed successful user requests
	Failed       int64               `json:"failed"`        // observed terminal provider failures
	CacheSamples int64               `json:"cache_samples"` // successes with positive normalized input
	CacheHits    int64               `json:"cache_hits"`    // successes with cache reads
	InputTokens  int64               `json:"input_tokens"`  // normalized input including creation and reads
	ReadTokens   int64               `json:"read_tokens"`   // cached input tokens read
	Graded       int64               `json:"graded"`        // audited pass and fail results
	Wrong        int64               `json:"wrong"`         // audited failing results
	Owners       []ModelQualityOwner `json:"owners"`        // latest applied result per owning plan
}

// ModelQualityOwner identifies the durable audit record even after pruning.
type ModelQualityOwner struct {
	PlanID         int64      `json:"plan_id"`         // schedule that owns this decision
	ResultID       int64      `json:"result_id"`       // original persisted probe ID
	RequestedModel string     `json:"requested_model"` // literal plan input at execution
	Reason         string     `json:"reason"`          // original grading explanation
	StartedAt      time.Time  `json:"started_at"`      // probe start
	ExpiresAt      *time.Time `json:"expires_at"`      // optional deadline
	State          string     `json:"state"`           // restricted, expired or passed
}
