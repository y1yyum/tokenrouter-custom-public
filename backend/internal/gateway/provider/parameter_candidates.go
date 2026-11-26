package provider

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/TokenFlux/TokenRouter/internal/gateway/requestpolicy"
	"github.com/TokenFlux/TokenRouter/internal/gateway/requeststate"
	"github.com/TokenFlux/TokenRouter/internal/protocol"
	providercore "github.com/TokenFlux/TokenRouter/internal/provider"
	"github.com/TokenFlux/TokenRouter/internal/provider/modelrules"
)

// parameter_candidates.go checks original client constraints against the
// concrete provider route. Provider-specific declarations are bounded by the
// mapped outbound model and both protocol identities. This keeps evidence for
// a native encoder from authorizing a different bridge or a provider alias
// that happens to share a client model name.
//
// The model resolver already describes some local encoding restrictions.
// Those restrictions take precedence over declarations about what the remote
// provider accepts. Additional facts require a source and revision, and
// missing facts leave a route unverified. Forwarding can call the same check
// again when effort handling or hydrated provider state changes the route.

const (
	// These outcomes distinguish missing evidence, established support, and a proven incompatibility.
	ParameterUnknown ParameterSupport = iota
	ParameterSupported
	ParameterUnsupported
)

// ParameterSupport classifies the evidence for an entire candidate route.
type ParameterSupport uint8

// ParameterDecision states the result and the first actionable client field.
type ParameterDecision struct {
	Support ParameterSupport // whole route verdict
	Path    string           // original field location
}

// parameterEvidence describes the verified constraint surface of one provider route.
type parameterEvidence struct {
	Model    string           `json:"model"`    // mapped upstream model
	Inbound  string           `json:"inbound"`  // inbound protocol identity
	Outbound string           `json:"outbound"` // outbound protocol identity
	Source   string           `json:"source"`   // evidence source identifier
	Version  string           `json:"version"`  // evidence source revision
	Fields   map[string]*bool `json:"fields"`   // supported or rejected constraint keys
}

// CheckParameterCandidate resolves the attempt and applies the same evidence
// rules used by final dispatch. Callers pass the model entering the provider
// mapping layer. Requests without constraints skip route and catalogue reads.
func CheckParameterCandidate(ctx context.Context, target *ExecutionProvider, model string, intent requestpolicy.Intent, requirements []requestpolicy.Requirement) ParameterDecision {
	if len(requirements) == 0 {
		return ParameterDecision{Support: ParameterSupported}
	}
	resolved, err := ProviderForProtocolAttempt(ctx, target)
	if err != nil || resolved.Route.Protocol() == "" {
		return ParameterDecision{Support: ParameterUnknown, Path: requirements[0].Path}
	}
	policy := ExecutionModelPolicy(resolved).WithContext(ctx)
	switch {
	case resolved.Record.Platform == providercore.PlatformAntigravity:
		if intent.Endpoint != requestpolicy.EndpointMessages {
			// The OpenAI bridge can synthesize a thinking mode before Antigravity
			// mapping. A client-side model alone cannot establish that wire identity.
			return ParameterDecision{Support: ParameterUnknown, Path: requirements[0].Path}
		}
		var thinking string
		if raw := intent.Lookup("thinking", "type"); raw.Origin == requestpolicy.OriginExplicit {
			if err := json.Unmarshal([]byte(raw.Raw), &thinking); err != nil {
				return ParameterDecision{Support: ParameterUnknown, Path: requirements[0].Path}
			}
		}
		modelCtx := requeststate.WithThinkingEnabled(ctx, thinking == "enabled" || thinking == "adaptive")
		model = policy.FinalAntigravityModel(modelCtx, model)
	case resolved.Route.Protocol() == protocol.ProtocolGeminiGenerateContent:
		model = policy.Mapped(model)
	default:
		model = policy.UpstreamModel(ctx, model)
	}
	return CheckParameterWireRoute(ctx, resolved, model, resolved.Route.Protocol(), intent, requirements)
}

// CheckParameterWireRoute checks capabilities for a model and protocol that
// have already been resolved by the caller. Unknown leaves the route
// ineligible unless all constraints can be established. One negative fact
// suffices to disqualify a route even when another field lacks evidence.
//
// Existing model rules can constrain the local encoder independently of the
// provider's declared evidence. Checking them against the frozen input keeps
// a generated thinking flag from authorizing removal of an explicit control.
func CheckParameterWireRoute(ctx context.Context, target *ExecutionProvider, model string, outbound protocol.ProtocolID, intent requestpolicy.Intent, requirements []requestpolicy.Requirement) ParameterDecision {
	if len(requirements) == 0 {
		return ParameterDecision{Support: ParameterSupported}
	}
	inbound := parameterInbound(intent.Endpoint)
	if target == nil || model == "" || inbound == "" || outbound == "" {
		return ParameterDecision{Support: ParameterUnknown, Path: requirements[0].Path}
	}
	evidence := parameterEvidenceFor(target, model, inbound, outbound)
	resolution, err := ResolveModelAttributes(ctx, target, model, nil, string(outbound))
	if err != nil {
		return ParameterDecision{Support: ParameterUnknown, Path: requirements[0].Path}
	}
	resolution = resolution.ForRequest(ModelRequestState([]byte(intent.RawJSON())))
	decision := ParameterDecision{Support: ParameterSupported}
	for _, req := range requirements {
		if !requestpolicy.CacheRepresentable(intent, req, parameterInboundEndpoint(outbound)) {
			return ParameterDecision{Support: ParameterUnsupported, Path: req.Path}
		}
		local := localParameterSupport(req, resolution, inbound, outbound)
		if local == ParameterUnsupported {
			return ParameterDecision{Support: ParameterUnsupported, Path: req.Path}
		}
		supported := evidence[req.Key]
		if req.Key == "max_tool_calls" && outbound == protocol.ProtocolOpenAIResponses {
			recordParameterToolLimitSupport(ctx, target.Record.ID, model, evidence)
		}
		if req.Key == "max_tool_calls" && (outbound != protocol.ProtocolOpenAIResponses || target.Record.AllowsResponsesRequestRewrite()) {
			if _, mayOmit := requestpolicy.UnusedToolLimit(intent, []byte(intent.RawJSON())); mayOmit {
				// Final preparation can still inject built-in tools. The wire check
				// requires either actual omission or positive native limit evidence.
				continue
			}
		}
		if supported != nil && !*supported {
			return ParameterDecision{Support: ParameterUnsupported, Path: req.Path}
		}
		verified := supported != nil || local == ParameterSupported
		for _, scope := range req.Scopes {
			fact := evidence[scope]
			if fact != nil && !*fact {
				return ParameterDecision{Support: ParameterUnsupported, Path: req.Path}
			}
			verified = verified && fact != nil
		}
		if req.Key == "max_tool_calls" && outbound != protocol.ProtocolOpenAIResponses {
			verified = false
		}
		if !verified && decision.Support == ParameterSupported {
			decision = ParameterDecision{Support: ParameterUnknown, Path: req.Path}
		}
	}
	return decision
}

// parameterInboundEndpoint returns the canonical field-policy name for a wire protocol.
func parameterInboundEndpoint(outbound protocol.ProtocolID) string {
	switch outbound {
	case protocol.ProtocolAnthropicMessages:
		return requestpolicy.EndpointMessages
	case protocol.ProtocolOpenAIChatCompletions:
		return requestpolicy.EndpointChat
	case protocol.ProtocolOpenAIResponses:
		return requestpolicy.EndpointResponses
	default:
		return ""
	}
}

// parameterEvidenceFor returns a single unambiguous declaration for the exact
// tuple. Duplicate rows and rows with incomplete provenance leave the tuple
// unresolved. Fields outside the tuple cannot affect this attempt.
func parameterEvidenceFor(target *ExecutionProvider, model, inbound string, outbound protocol.ProtocolID) map[string]*bool {
	raw, ok := target.Record.Extra[providercore.RequestParameterEvidenceExtraKey]
	if !ok {
		return nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var entries []parameterEvidence
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil
	}
	var match *parameterEvidence
	for i := range entries {
		entry := &entries[i]
		if entry.Model != model || entry.Inbound != inbound || entry.Outbound != string(outbound) {
			continue
		}
		if match != nil || strings.TrimSpace(entry.Source) == "" || strings.TrimSpace(entry.Version) == "" {
			return nil
		}
		match = entry
	}
	if match == nil {
		return nil
	}
	return match.Fields
}

// localParameterSupport applies encoding facts provided by modelrules. Empty
// declarations remain unknown. Budget support requires a native Messages
// route with a declared budget encoding and a value in its model bounds.
func localParameterSupport(req requestpolicy.Requirement, resolution modelrules.Resolution, inbound string, outbound protocol.ProtocolID) ParameterSupport {
	attrs := resolution.Attributes
	switch req.Key {
	case "temperature":
		if attrs.Temperature != nil && !*attrs.Temperature {
			return ParameterUnsupported
		}
	case "thinking.budget_tokens":
		if outbound != protocol.ProtocolAnthropicMessages {
			return ParameterUnknown
		}
		switch resolution.Rules.ThinkingMode {
		case "adaptive", "adaptive_between_tools":
			return ParameterUnsupported
		case "budget", "adaptive_or_budget":
			if inbound == string(outbound) && resolution.Version != "" {
				minimum, maximum := attrs.ReasoningBudget()
				count, err := strconv.Atoi(req.Value.Raw)
				if err == nil && count > 0 {
					if (minimum != nil && count < *minimum) || (maximum != nil && count > *maximum) {
						return ParameterUnsupported
					}
					if minimum != nil && maximum != nil {
						return ParameterSupported
					}
				}
			}
		}
	}
	return ParameterUnknown
}

// parameterInbound maps the admitted endpoint to its matching protocol identity.
func parameterInbound(endpoint string) string {
	switch endpoint {
	case requestpolicy.EndpointMessages:
		return string(protocol.ProtocolAnthropicMessages)
	case requestpolicy.EndpointChat:
		return string(protocol.ProtocolOpenAIChatCompletions)
	case requestpolicy.EndpointResponses:
		return string(protocol.ProtocolOpenAIResponses)
	default:
		return ""
	}
}
