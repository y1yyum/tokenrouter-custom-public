package provider

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/TokenFlux/TokenRouter/internal/gateway/requestpolicy"
	"github.com/TokenFlux/TokenRouter/internal/protocol"
	providercore "github.com/TokenFlux/TokenRouter/internal/provider"
	"github.com/TokenFlux/TokenRouter/internal/provider/modelrules"
)

// TestParameterWireRouteEvidence varies the mapped model, wire protocol and
// provenance independently. Earlier eligibility cannot authorize a different
// wire route, and absence of evidence stays distinguishable from a rejection.
func TestParameterWireRouteEvidence(t *testing.T) {
	intent := requestpolicy.Intent{Mode: requestpolicy.ModeStrict, Endpoint: requestpolicy.EndpointResponses}
	required := []requestpolicy.Requirement{{Key: "temperature", Path: "temperature", Value: requestpolicy.Value{Origin: requestpolicy.OriginExplicit, Raw: "0"}}}
	const wire = protocol.ProtocolOpenAIResponses
	fields := map[string]*bool{"temperature": new(true)}
	row := map[string]any{
		"model": "candidate-a", "inbound": string(wire), "outbound": string(wire),
		"source": "wire-fixture", "version": "1", "fields": fields,
	}
	target := NewExecutionProvider(&providercore.Record{Extra: map[string]any{
		providercore.RequestParameterEvidenceExtraKey: []map[string]any{row},
	}})
	check := func(model string, outbound protocol.ProtocolID) ParameterSupport {
		return CheckParameterWireRoute(t.Context(), target, model, outbound, intent, required).Support
	}
	require.Equal(t, ParameterSupported, check("candidate-a", wire))
	require.Equal(t, ParameterUnknown, check("candidate-b", wire))
	require.Equal(t, ParameterUnknown, check("candidate-a", protocol.ProtocolAnthropicMessages))
	fields["temperature"] = nil
	require.Equal(t, ParameterUnknown, check("candidate-a", wire))
	fields["temperature"] = new(false)
	require.Equal(t, ParameterUnsupported, check("candidate-a", wire))
	row["version"] = ""
	require.Equal(t, ParameterUnknown, check("candidate-a", wire))
}

// TestParameterWireBudgetBounds checks budget mode with known or missing
// limits and rejects budgets incompatible with the adaptive-only encoding.
func TestParameterWireBudgetBounds(t *testing.T) {
	resolution := modelrules.Resolution{
		Version: "fixture-1",
		Rules:   modelrules.ExecutionRules{ThinkingMode: "budget"},
	}
	require.NoError(t, json.Unmarshal([]byte(`[{"type":"budget_tokens","min":1024,"max":4096}]`), &resolution.Attributes.ReasoningOptions))
	const wire = protocol.ProtocolAnthropicMessages
	req := requestpolicy.Requirement{Key: "thinking.budget_tokens", Value: requestpolicy.Value{Origin: requestpolicy.OriginExplicit, Raw: "2048"}}
	require.Equal(t, ParameterSupported, localParameterSupport(req, resolution, string(wire), wire))
	req.Value.Raw = "4097"
	require.Equal(t, ParameterUnsupported, localParameterSupport(req, resolution, string(wire), wire))
	(*resolution.Attributes.ReasoningOptions)[0].Max = nil
	require.Equal(t, ParameterUnknown, localParameterSupport(req, resolution, string(wire), wire))
	resolution.Rules.ThinkingMode = "adaptive"
	require.Equal(t, ParameterUnsupported, localParameterSupport(req, resolution, string(wire), wire))
}
