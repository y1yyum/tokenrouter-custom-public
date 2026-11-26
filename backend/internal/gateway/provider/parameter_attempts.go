package provider

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/tidwall/gjson"

	"github.com/TokenFlux/TokenRouter/internal/gateway/requestpolicy"
	"github.com/TokenFlux/TokenRouter/internal/upstream/openai"
)

// parameter_attempts.go owns mutable attempt state for strict text requests.
// Responses and Messages share the existing six-ticket allowance. Revisiting
// a provider, model and endpoint uses its previous repair history even when
// the effective group has changed. Verified limit support is scoped to that
// same wire identity, and the output reader uses the latest omission decision.
//
// Only the HTTP entry creates the state after matching a strict rule. Selection
// saves the proved limits before the transport sees the final tool set. Each
// admitted repair reserves a ticket before sending its body, so parallel
// checks cannot overspend if request preparation or exchange exits early.

// parameterAttemptsKey identifies mutable execution state on an admitted request.
type parameterAttemptsKey struct{}

// parameterAttemptKey identifies one provider's mapped text protocol route.
type parameterAttemptKey struct {
	provider int64  // selected provider record
	endpoint string // outbound protocol endpoint
	model    string // mapped wire model
}

// parameterAttempts owns route-local histories that spend a shared budget.
type parameterAttempts struct {
	// The mutex guards both maps, while the repair states and shared budget
	// guard their own counters. Response readers only consult the atomic limit.
	mu           sync.Mutex                                                       // protects both route maps
	budget       openai.ResponsesRejectedFieldRetryBudget                         // shared six-ticket allowance
	repairs      map[parameterAttemptKey]*openai.ResponsesRejectedFieldRetryState // candidate repair histories
	limitSupport map[parameterAttemptKey]map[string]*bool                         // verified candidate tool-limit facts
	omittedLimit atomic.Int64                                                     // omitted limit on latest send
}

// WithParameterAttempts installs mutable attempt state for an enrolled strict
// request. All routing fallbacks and detached stream contexts share that state.
func WithParameterAttempts(ctx context.Context) context.Context {
	if _, strict := requestpolicy.StrictIntent(ctx); !strict || parameterAttemptsFrom(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, parameterAttemptsKey{}, &parameterAttempts{
		repairs:      make(map[parameterAttemptKey]*openai.ResponsesRejectedFieldRetryState),
		limitSupport: make(map[parameterAttemptKey]map[string]*bool),
	})
}

// ParameterRepairState returns the candidate's state and remembers its initial
// body. A nil result leaves legacy callers with their existing repair policy.
func ParameterRepairState(ctx context.Context, provider int64, endpoint string, initial []byte) *openai.ResponsesRejectedFieldRetryState {
	attempts := parameterAttemptsFrom(ctx)
	if attempts == nil {
		return nil
	}
	key := parameterAttemptKey{provider, endpoint, gjson.GetBytes(initial, "model").String()}
	attempts.mu.Lock()
	defer attempts.mu.Unlock()
	state := attempts.repairs[key]
	if state == nil {
		state = openai.NewOpenAIResponsesRejectedFieldRetryStateWithBudget(initial, &attempts.budget)
		attempts.repairs[key] = state
	} else {
		state.Remember(initial)
	}
	return state
}

// AllowMessagesParameterRepair spends one shared ticket only after checking
// the fully prepared candidate and the frozen message controls.
func AllowMessagesParameterRepair(ctx context.Context, provider int64, initial, proposed []byte) bool {
	if _, strict := requestpolicy.StrictIntent(ctx); !strict {
		return true
	}
	return requestpolicy.AllowsMessagesRepair(ctx, initial, proposed) &&
		ParameterRepairState(ctx, provider, requestpolicy.EndpointMessages, initial).Allow(proposed)
}

// recordParameterToolLimitSupport retains exact Responses capability facts
// for the send check, which is the first point that sees injected tools.
func recordParameterToolLimitSupport(ctx context.Context, provider int64, model string, fields map[string]*bool) {
	if attempts := parameterAttemptsFrom(ctx); attempts != nil {
		attempts.mu.Lock()
		defer attempts.mu.Unlock()
		attempts.limitSupport[parameterAttemptKey{provider, requestpolicy.EndpointResponses, model}] = fields
	}
}

// ParameterToolLimitSupported requires positive facts for the original and
// prepared tool sets when transport must send the cap to this candidate.
func ParameterToolLimitSupported(ctx context.Context, provider int64, model string, original string, prepared []byte, inherited bool) bool {
	attempts := parameterAttemptsFrom(ctx)
	if attempts == nil {
		return false
	}
	attempts.mu.Lock()
	fields := attempts.limitSupport[parameterAttemptKey{provider, requestpolicy.EndpointResponses, model}]
	attempts.mu.Unlock()
	if supported := fields["max_tool_calls"]; supported == nil || !*supported {
		return false
	}
	for _, scope := range requestpolicy.ToolLimitScopes(original, prepared, inherited) {
		if supported := fields[scope]; supported == nil || !*supported {
			return false
		}
	}
	return true
}

// RecordParameterToolLimit records the original cap omitted by the current send.
func RecordParameterToolLimit(ctx context.Context, limit int64) {
	if attempts := parameterAttemptsFrom(ctx); attempts != nil {
		attempts.omittedLimit.Store(limit)
	}
}

// ParameterToolLimitOmitted returns the cap to echo in successful Responses output.
func ParameterToolLimitOmitted(ctx context.Context) int64 {
	if attempts := parameterAttemptsFrom(ctx); attempts != nil {
		return attempts.omittedLimit.Load()
	}
	return 0
}

// parameterAttemptsFrom reads the shared state from a request or attempt context.
func parameterAttemptsFrom(ctx context.Context) *parameterAttempts {
	attempts, _ := ctx.Value(parameterAttemptsKey{}).(*parameterAttempts)
	return attempts
}
