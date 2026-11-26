package selection

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/TokenFlux/TokenRouter/internal/apikey"
	"github.com/TokenFlux/TokenRouter/internal/egress"
	gatewayprovider "github.com/TokenFlux/TokenRouter/internal/gateway/provider"
	"github.com/TokenFlux/TokenRouter/internal/gateway/requestpolicy"
	"github.com/TokenFlux/TokenRouter/internal/gateway/requeststate"
	"github.com/TokenFlux/TokenRouter/internal/gateway/session"
	"github.com/TokenFlux/TokenRouter/internal/pkg/apperror"
	"github.com/TokenFlux/TokenRouter/internal/protocol"
	providercore "github.com/TokenFlux/TokenRouter/internal/provider"
	"github.com/TokenFlux/TokenRouter/internal/routing"
	"github.com/TokenFlux/TokenRouter/internal/settings"
)

// parameterSelectionSettings supplies the saved strict configuration for a selector.
type parameterSelectionSettings struct {
	settings.Repository        // unused settings operations
	value               string // exact saved policy JSON
}

// GetValue returns the stored entry policy to the refresh reader.
func (repo parameterSelectionSettings) GetValue(context.Context, string) (string, error) {
	return repo.value, nil
}

// TestStrictParameterSelection exercises both schedulers and ordinary session
// affinity. Final model aliases, unknown evidence and incompatibility each
// determine whether the route can enter the slot or wait pool.
func TestStrictParameterSelection(t *testing.T) {
	const source = protocol.ProtocolAnthropicMessages
	for _, mode := range []routing.GroupSchedulerType{routing.GroupSchedulerTypeBasic, routing.GroupSchedulerTypeAdvanced} {
		for _, selector := range []string{"compatible", "generic"} {
			t.Run(string(mode)+"/"+selector, func(t *testing.T) {
				group := &routing.Group{ID: 81, Hydrated: true, Status: routing.StatusActive, SchedulerType: mode, AllowedProtocols: []protocol.ProtocolID{source}}
				ctx := parameterSelectionContext(t, group, source, requestpolicy.EndpointMessages)
				denied := parameterSelectionProvider(1, group.ID, "native-a", source, false)
				supported := parameterSelectionProvider(2, group.ID, "native-b", source, true)
				unknown := parameterSelectionProvider(3, group.ID, "native-c", source, true)
				unknown.Record.Credentials["model_mapping"] = map[string]any{"client": "different-wire-name"}
				unknown.Record.Credentials["model_whitelist"] = []string{"different-wire-name"}
				selectRoute := func(values ...gatewayprovider.ExecutionProvider) (*gatewayprovider.SelectionResult, error) {
					repo := &mixedGroupProviders{values: values}
					if selector == "generic" {
						cache := &mockGatewayCacheForPlatform{sessionBindings: map[string]int64{"sticky": 1}}
						svc := NewGeneric(GenericDependencies{Reads: Reads{Providers: repo}, Shared: Shared{Cache: cache}}, DefaultOptions())
						return svc.SelectProviderWithLoadAwareness(ctx, &group.ID, "sticky", "client", nil)
					}
					cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:sticky": 1}}
					svc := NewCompatible(CompatibleDependencies{Reads: Reads{Providers: repo}, Shared: Shared{Cache: cache}}, DefaultOptions())
					chosen, _, err := svc.SelectProviderWithScheduler(ctx, &group.ID, "", "sticky", "client", nil, egress.OpenAIUpstreamTransportAny, false)
					return chosen, err
				}
				chosen, err := selectRoute(denied, supported)
				require.NoError(t, err)
				require.Equal(t, int64(2), chosen.Provider.Record.ID)
				if chosen.ReleaseFunc != nil {
					chosen.ReleaseFunc()
				}
				chosen, err = selectRoute(denied, unknown)
				require.Nil(t, chosen)
				require.Equal(t, requestpolicy.CandidateUnavailableReason, apperror.Reason(err))
				chosen, err = selectRoute(denied)
				require.Nil(t, chosen)
				require.Equal(t, requestpolicy.CandidateUnsupportedReason, apperror.Reason(err))
				require.Contains(t, err.Error(), "temperature")
			})
		}
	}
}

// TestStrictParameterBoundResponseAndFallback exercises immutable provider
// affinity and an authorized group replacement. The fallback context keeps
// the original cohort and requires destination evidence for its mapped route.
func TestStrictParameterBoundResponseAndFallback(t *testing.T) {
	const source = protocol.ProtocolOpenAIResponses
	for _, mode := range []routing.GroupSchedulerType{routing.GroupSchedulerTypeBasic, routing.GroupSchedulerTypeAdvanced} {
		t.Run(string(mode), func(t *testing.T) {
			entry := &routing.Group{ID: 81, Hydrated: true, Status: routing.StatusActive, SchedulerType: mode, AllowedProtocols: []protocol.ProtocolID{source}}
			fallback := &routing.Group{ID: 82, Hydrated: true, Status: routing.StatusActive, SchedulerType: mode, AllowedProtocols: []protocol.ProtocolID{source}}
			ctx := parameterSelectionContext(t, entry, source, requestpolicy.EndpointResponses)
			owner := parameterSelectionProvider(1, entry.ID, "wire-owner", source, false)
			alternative := parameterSelectionProvider(2, entry.ID, "wire-other", source, true)
			destination := parameterSelectionProvider(3, fallback.ID, "wire-fallback", source, true)
			store := session.NewOpenAIWSStateStore(nil)
			require.NoError(t, store.BindResponseProvider(ctx, entry.ID, "resp_bound", owner.Record.ID, time.Minute))
			repo := &mixedGroupProviders{values: []gatewayprovider.ExecutionProvider{owner, alternative, destination}}
			svc := NewCompatible(CompatibleDependencies{Reads: Reads{Providers: repo}, Responses: store}, DefaultOptions())
			selected, _, err := svc.SelectProviderWithScheduler(ctx, &entry.ID, "resp_bound", "", "client", nil, egress.OpenAIUpstreamTransportAny, false)
			require.Nil(t, selected)
			require.Equal(t, requestpolicy.CandidateUnsupportedReason, apperror.Reason(err), "%v", err)
			id, err := store.GetResponseProvider(ctx, entry.ID, "resp_bound")
			require.NoError(t, err)
			require.Equal(t, owner.Record.ID, id)
			_, _, err = svc.SelectProviderWithScheduler(ctx, &fallback.ID, "", "", "client", nil, egress.OpenAIUpstreamTransportAny, false)
			require.ErrorIs(t, err, apikey.ErrGroupNotAllowed)
			fallbackCtx := requeststate.WithGroup(ctx, fallback)
			fallbackCtx = requeststate.WithRoutePlan(fallbackCtx, gatewayprovider.NewRoutePlanner(nil).PlanRoute(fallbackCtx, fallback, &fallback.ID, "client"))
			frozen, ok := requestpolicy.FromContext(fallbackCtx)
			require.True(t, ok)
			require.Equal(t, entry.ID, frozen.EntryGroupID)
			selected, _, err = svc.SelectProviderWithScheduler(fallbackCtx, &fallback.ID, "", "", "client", nil, egress.OpenAIUpstreamTransportAny, false)
			require.NoError(t, err)
			require.Equal(t, destination.Record.ID, selected.Provider.Record.ID)
			if selected.ReleaseFunc != nil {
				selected.ReleaseFunc()
			}
		})
	}
}

// parameterSelectionContext enrolls one strict request before attaching its
// admitted group plan. Later selection receives the original value and the
// current routing identities as independent inputs.
func parameterSelectionContext(t *testing.T, group *routing.Group, source protocol.ProtocolID, endpoint string) context.Context {
	t.Helper()
	config := requestpolicy.Config{Revision: 1, Mode: requestpolicy.ModeStrict, Rules: []requestpolicy.Rule{{
		Entry: requestpolicy.Entry{EntryGroupID: group.ID, Endpoint: endpoint, RequestedModel: "client"},
		Mode:  requestpolicy.ModeStrict, StrictPercent: 100,
	}}}
	data, err := json.Marshal(config)
	require.NoError(t, err)
	service := requestpolicy.New(settings.New(parameterSelectionSettings{value: string(data)}))
	require.NoError(t, service.Refresh(t.Context()))
	ctx, matched := service.Admit(t.Context(), group.ID, endpoint, "request", []byte(`{"model":"client","temperature":0}`))
	require.True(t, matched)
	ctx = requeststate.WithClientProtocol(requeststate.WithGroup(ctx, group), source)
	plan := gatewayprovider.NewRoutePlanner(nil).PlanRoute(ctx, group, &group.ID, "client")
	return requeststate.WithRoutePlan(ctx, plan)
}

// parameterSelectionProvider declares one fixture's exact mapped protocol
// support. The priority mirrors provider ID to make unsupported routes the
// preferred choice unless parameter screening removes them.
func parameterSelectionProvider(id, group int64, model string, wire protocol.ProtocolID, support bool) gatewayprovider.ExecutionProvider {
	platform := providercore.PlatformOpenAI
	if wire == protocol.ProtocolAnthropicMessages {
		platform = providercore.PlatformAnthropic
	}
	value := mixedGroupProvider(id, platform, model, group)
	value.Record.Credentials["model_mapping"] = map[string]any{"client": model}
	value.Record.Priority = int(id)
	value.Record.Extra = map[string]any{providercore.RequestParameterEvidenceExtraKey: []map[string]any{{
		"model": model, "inbound": string(wire), "outbound": string(wire),
		"source": "selector-fixture", "version": "1",
		"fields": map[string]bool{"temperature": support},
	}}}
	return value
}
