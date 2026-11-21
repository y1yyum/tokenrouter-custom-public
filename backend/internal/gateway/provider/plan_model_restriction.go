package provider

import (
	"context"
	"time"

	providercore "github.com/TokenFlux/TokenRouter/internal/provider"
)

// plan_model_restriction.go carries the read-only verdict source from the HTTP
// entry to scheduler and send-time checks. Both read the durable authority so
// a delayed scheduler snapshot cannot re-enable a restricted provider/model.

// PlanModelRestrictionReader returns active plan owners for an actual model
// at the supplied instant. A read failure makes that candidate unavailable.
type PlanModelRestrictionReader func(context.Context, int64, string, time.Time) ([]providercore.ActivePlanModelRestriction, error)

// planModelRestrictionKey scopes the reader to a single admitted request.
type planModelRestrictionKey struct{}

// WithPlanModelRestrictions marks an HTTP Responses request for durable checks.
// Child contexts created for retries and authorized group fallback inherit it.
func WithPlanModelRestrictions(ctx context.Context, read PlanModelRestrictionReader) context.Context {
	return context.WithValue(ctx, planModelRestrictionKey{}, read)
}

// PlanModelRejection resolves the provider's actual model for the current
// group-mapped route. Missing owners admit the candidate. Repository failures
// and active owners carry separate reasons for read-only diagnostics.
func PlanModelRejection(ctx context.Context, candidate *ExecutionProvider, routingModel string) string {
	if ctx == nil || candidate == nil || candidate.Record.Platform != providercore.PlatformOpenAI {
		return ""
	}
	read, _ := ctx.Value(planModelRestrictionKey{}).(PlanModelRestrictionReader)
	if read == nil {
		return ""
	}
	actual := ExecutionModelPolicy(candidate).UpstreamModel(ctx, routingModel)
	if actual == "" {
		return "plan_model_unresolved"
	}
	owners, err := read(ctx, candidate.Record.ID, actual, time.Now())
	if err != nil {
		return "plan_model_read_failed"
	}
	if len(owners) > 0 {
		return "plan_model_restricted"
	}
	return ""
}
