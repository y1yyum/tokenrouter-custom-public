package provider

import (
	"context"
	"sync"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/gateway/requeststate"
	providercore "github.com/TokenFlux/TokenRouter/internal/provider"
)

// plan_model_restriction.go checks the scheduled verdict for an OpenAI wire
// model. A scheduler can inspect the same group and candidate several times.
// Each outbound HTTP attempt and WS turn rereads committed verdicts, even if
// the scheduler already admitted that provider from its request-local map.

// PlanModelRestrictionReader returns active plan owners for an actual model.
type PlanModelRestrictionReader func(context.Context, int64, string, time.Time) ([]providercore.ActivePlanModelRestriction, error)

// planModelRestrictionKey identifies one admitted request's quality checks.
type planModelRestrictionKey struct{}

// planModelTarget distinguishes destination groups and final provider mappings.
type planModelTarget struct {
	group, provider int64  // admitted group and candidate ID
	model           string // resolved outbound model
}

// planModelGate retains selection observations within the request only.
type planModelGate struct {
	read       PlanModelRestrictionReader // durable authority
	compact    *CompactModels             // legacy Compact overrides, including the global fallback
	bound      bool                       // an existing response owner forbids switching on a late rejection
	mu         sync.Mutex                 // protects candidate observations during concurrent selection
	candidates map[planModelTarget]string // selection reasons by destination
}

// WithPlanModelRestrictions installs ordinary model checks on the request.
func WithPlanModelRestrictions(ctx context.Context, read PlanModelRestrictionReader) context.Context {
	return read.WithRequest(ctx, nil, false)
}

// WithRequest binds endpoint-specific mapping and continuation semantics.
func (read PlanModelRestrictionReader) WithRequest(ctx context.Context, compact *CompactModels, bound bool) context.Context {
	if read == nil {
		return ctx
	}
	return context.WithValue(ctx, planModelRestrictionKey{}, &planModelGate{read: read, compact: compact, bound: bound, candidates: make(map[planModelTarget]string)})
}

// HasPlanModelRestrictions avoids decoding the outbound model when the gate is disabled.
func HasPlanModelRestrictions(ctx context.Context) bool {
	return ctx != nil && ctx.Value(planModelRestrictionKey{}) != nil
}

// modelGate returns the enabled policy for an OpenAI execution candidate.
func modelGate(ctx context.Context, p *ExecutionProvider) *planModelGate {
	if ctx == nil || p == nil || p.Record.Platform != providercore.PlatformOpenAI {
		return nil
	}
	gate, _ := ctx.Value(planModelRestrictionKey{}).(*planModelGate)
	return gate
}

// rejection distinguishes unresolved wire identity, storage failure and an active owner.
func (g *planModelGate) rejection(ctx context.Context, id int64, actual string) string {
	if actual == "" {
		return "plan_model_unresolved"
	}
	owners, err := g.read(ctx, id, actual, time.Now())
	if err != nil {
		return "plan_model_read_failed"
	}
	if len(owners) > 0 {
		return "plan_model_restricted"
	}
	return ""
}

// PlanModelRejection evaluates the candidate's route using this endpoint's model rules.
func PlanModelRejection(ctx context.Context, candidate *ExecutionProvider, routingModel string) string {
	gate := modelGate(ctx, candidate)
	if gate == nil {
		return ""
	}
	actual := PlanModelTarget(ctx, candidate, routingModel, gate.compact)
	key := planModelTarget{provider: candidate.Record.ID, model: actual}
	if group, ok := requeststate.GroupFromContext(ctx); ok && group != nil {
		key.group = group.ID
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if reason, ok := gate.candidates[key]; ok {
		return reason
	}
	reason := gate.rejection(ctx, key.provider, key.model)
	gate.candidates[key] = reason
	return reason
}

// CheckPlanModelSend reads again for the actual wire model, including local retries.
// The bound flag tells the HTTP loop whether this request can switch providers.
func CheckPlanModelSend(ctx context.Context, p *ExecutionProvider, actual string) (reason string, bound bool) {
	if gate := modelGate(ctx, p); gate != nil {
		return gate.rejection(ctx, p.Record.ID, actual), gate.bound
	}
	return "", false
}

// PlanModelTarget resolves the same wire identity for selection and read-only
// diagnostics, including provider and process-wide legacy Compact overrides.
func PlanModelTarget(ctx context.Context, candidate *ExecutionProvider, routingModel string, compact *CompactModels) string {
	policy := ExecutionModelPolicy(candidate).WithContext(ctx)
	actual := policy.OpenAIUpstream(routingModel, compact != nil)
	if compact != nil {
		if override := compact.Recovery(candidate).ResolveModel(routingModel); override != "" {
			actual = override
		}
	}
	return actual
}
