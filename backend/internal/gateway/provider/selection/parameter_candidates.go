package selection

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"

	gatewayprovider "github.com/TokenFlux/TokenRouter/internal/gateway/provider"
	"github.com/TokenFlux/TokenRouter/internal/gateway/requestpolicy"
	schedulercore "github.com/TokenFlux/TokenRouter/internal/scheduler"
)

// parameter_candidates.go retains parameter evidence across one scheduling
// pass. Preferences and alternate scheduling branches can inspect the same
// candidate more than once. Any verified compatible route or unresolved route
// prevents a field rejection because its failure may reflect capacity or an
// incomplete record.
//
// The selector keeps its ordinary group and protocol gates. The ledger only
// classifies exhaustion after those gates, including immutable continuation
// affinity. It keeps earlier compatible observations when a later read fails,
// so losing access to that record cannot change availability into a client
// field error. Forwarding independently checks the final route before dispatch.

// parameterSelectionKey identifies state for this logical scheduling pass.
type parameterSelectionKey struct{}

// parameterSelection records the frozen requirements and evidence of exhaustion.
type parameterSelection struct {
	intent       requestpolicy.Intent        // original matched intent
	requirements []requestpolicy.Requirement // original field constraints
	// mu protects possible and rejections when any candidate writes evidence.
	// possible is monotonic, while rejections accumulates the paths needed to
	// produce a deterministic field error at the end of selection.
	mu         sync.Mutex          // serializes candidate observations
	possible   bool                // any candidate not proven incompatible
	rejections map[string]struct{} // distinct incompatible field paths
}

// withParameterSelection captures constraints once for all candidates in a
// scheduling pass. A nested pass inherits the outer observations. Exclusions
// from earlier attempts prevent declaring the entire original pool incompatible.
func withParameterSelection(ctx context.Context, excluded int) (context.Context, *parameterSelection) {
	if state, _ := ctx.Value(parameterSelectionKey{}).(*parameterSelection); state != nil {
		return ctx, state
	}
	intent, _ := requestpolicy.FromContext(ctx)
	requirements := requestpolicy.Requirements(intent)
	if len(requirements) == 0 {
		return ctx, nil
	}
	state := &parameterSelection{intent: intent, requirements: requirements, possible: excluded > 0}
	return context.WithValue(ctx, parameterSelectionKey{}, state), state
}

// parameterSelectionActive reports whether this pass checks strict candidate requirements.
func parameterSelectionActive(ctx context.Context) bool {
	state, _ := ctx.Value(parameterSelectionKey{}).(*parameterSelection)
	return state != nil
}

// observeParameterUnavailable prevents a partial pool read or transient gate
// from proving universal incompatibility. The caller keeps its original
// rejection and uses this observation only if selection eventually fails.
func observeParameterUnavailable(ctx context.Context) {
	state, _ := ctx.Value(parameterSelectionKey{}).(*parameterSelection)
	if state != nil {
		state.observe(gatewayprovider.ParameterDecision{Support: gatewayprovider.ParameterUnknown})
	}
}

// observeParameterGateFailure treats runtime and quota gates as availability evidence.
func observeParameterGateFailure(ctx context.Context, reason string) {
	switch reason {
	case "quota_exceeded", "window_cost_exceeded", "rpm_exceeded", "scheduling_threshold",
		"gemini_quota_exhausted", "runtime_blocked", "proxy_stream_quarantined",
		"quota_auto_pause":
		observeParameterUnavailable(ctx)
	}
}

// parameterCandidateReason records strict capability evidence on an otherwise
// authorized route. It returns a scheduler reason for unsupported or unknown
// candidates, both of which must stay outside the dispatchable pool.
func parameterCandidateReason(ctx context.Context, candidate *gatewayprovider.ExecutionProvider, model string) string {
	state, _ := ctx.Value(parameterSelectionKey{}).(*parameterSelection)
	if state == nil {
		return ""
	}
	decision := gatewayprovider.CheckParameterCandidate(ctx, candidate, model, state.intent, state.requirements)
	state.observe(decision)
	switch decision.Support {
	case gatewayprovider.ParameterSupported:
		return ""
	case gatewayprovider.ParameterUnsupported:
		return requestpolicy.CandidateUnsupportedReason
	default:
		return requestpolicy.CandidateUnavailableReason
	}
}

// observe records one result without erasing any evidence that a route might
// serve the request. Concurrent or nested ranking passes can add new paths,
// but a failed read or a later exclusion cannot retroactively prove why a
// previously possible route failed. The field set is only used for terminal
// classification, so repeated visits to the same route cannot alter the
// published client message.
func (s *parameterSelection) observe(decision gatewayprovider.ParameterDecision) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if decision.Support != gatewayprovider.ParameterUnsupported {
		s.possible = true
		return
	}
	if s.rejections == nil {
		s.rejections = make(map[string]struct{})
	}
	s.rejections[decision.Path] = struct{}{}
}

// failure classifies exhaustion without replacing errors from authorization
// or provider storage. A field 400 requires a nonempty set of rejected
// candidates with no other observed or previously attempted route. If an
// otherwise eligible candidate was supported, unknown, or unavailable, the
// client's next action is governed by availability. Sorting keeps the field
// message independent of the order in which schedulers checked routes.
func (s *parameterSelection) failure(err error) error {
	if s == nil || err != nil && !errors.Is(err, schedulercore.ErrNoAvailableProviders) {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.possible || len(s.rejections) == 0 {
		return requestpolicy.CandidateUnavailable()
	}
	fields := make([]string, 0, len(s.rejections))
	for path := range s.rejections {
		fields = append(fields, path)
	}
	slices.Sort(fields)
	return requestpolicy.CandidateUnsupported(strings.Join(fields, ", "))
}
