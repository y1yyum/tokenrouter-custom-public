package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	gatewayprovider "github.com/TokenFlux/TokenRouter/internal/gateway/provider"
	"github.com/TokenFlux/TokenRouter/internal/gateway/requestpolicy"
	"github.com/TokenFlux/TokenRouter/internal/gateway/requeststate"
	"github.com/TokenFlux/TokenRouter/internal/pkg/apperror"
	"github.com/TokenFlux/TokenRouter/internal/protocol"
	"github.com/TokenFlux/TokenRouter/internal/provider"
	"github.com/TokenFlux/TokenRouter/internal/routing"
	"github.com/TokenFlux/TokenRouter/internal/settings"
	settingstest "github.com/TokenFlux/TokenRouter/internal/settings/testkit"
)

// TestStrictParameterRecheckStopsChangedModel revalidates a provider after its
// alias maps to a different outbound identity. Dispatch stops with 503 before
// using the unverified model, leaving the original strict intent intact.
func TestStrictParameterRecheckStopsChangedModel(t *testing.T) {
	const source = protocol.ProtocolOpenAIResponses
	const raw = `{"model":"client","input":"hello","temperature":0}`
	repo := settingstest.NewMemory()
	require.NoError(t, repo.Set(t.Context(), requestpolicy.SettingKey, `{"revision":1,"mode":"strict","rules":[{"entry_group_id":11,"inbound_endpoint":"/v1/responses","requested_model":"client","mode":"strict","strict_percent":100}]}`))
	service := requestpolicy.New(settings.New(repo))
	require.NoError(t, service.Refresh(t.Context()))
	ctx, matched := service.Admit(t.Context(), 11, requestpolicy.EndpointResponses, "route-drift", []byte(raw))
	require.True(t, matched)
	group := &routing.Group{ID: 11, Status: routing.StatusActive, Hydrated: true, AllowedProtocols: []protocol.ProtocolID{source}}
	ctx = requeststate.WithClientProtocol(requeststate.WithGroup(ctx, group), source)
	ctx = requeststate.WithRoutePlan(ctx, gatewayprovider.NewRoutePlanner(nil).PlanRoute(ctx, group, &group.ID, "client"))
	intent, _ := requestpolicy.FromContext(ctx)
	record := provider.Record{
		ID: 12, Platform: provider.PlatformOpenAI, Type: provider.ProviderTypeAPIKey,
		Credentials: map[string]any{"model_mapping": map[string]any{"client": "wire-before"}},
		Extra: map[string]any{provider.RequestParameterEvidenceExtraKey: []map[string]any{{
			"model": "wire-before", "inbound": string(source), "outbound": string(source),
			"source": "dispatch-fixture", "version": "1", "fields": map[string]bool{"temperature": true},
		}}},
	}
	chosen := gatewayprovider.NewExecutionProvider(&record)
	require.Equal(t, gatewayprovider.ParameterSupported, gatewayprovider.CheckParameterCandidate(ctx, chosen, "client", intent, requestpolicy.Requirements(intent)).Support)
	record.Credentials = map[string]any{"model_mapping": map[string]any{"client": "wire-after"}}
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequestWithContext(ctx, http.MethodPost, requestpolicy.EndpointResponses, nil)
	executor := &UnifiedTextExecutor{}
	result, err := executor.Responses(ctx, c, gatewayprovider.NewExecutionProvider(&record), []byte(raw))
	require.Nil(t, result)
	require.Equal(t, requestpolicy.CandidateUnavailableReason, apperror.Reason(err))
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	require.True(t, IsResponseCommitted(c))
}
