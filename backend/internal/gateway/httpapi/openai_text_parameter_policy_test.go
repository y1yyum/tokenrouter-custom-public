package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/TokenFlux/TokenRouter/internal/apikey"
	"github.com/TokenFlux/TokenRouter/internal/gateway/requestpolicy"
	"github.com/TokenFlux/TokenRouter/internal/infra/telemetry"
	"github.com/TokenFlux/TokenRouter/internal/settings"
	settingstest "github.com/TokenFlux/TokenRouter/internal/settings/testkit"
)

// openai_text_parameter_policy_test.go checks the entry identity across
// authentication rewrites that occur before the text handler reads its body.

// TestParameterPolicyUsesAuthenticatedGroupAndOriginalModel applies a real key
// model redirect and then supplies a different effective key group. The original
// request still matches, with its captured bytes and original schema error.
func TestParameterPolicyUsesAuthenticatedGroupAndOriginalModel(t *testing.T) {
	const raw = `{"model":"alias","input":"hello","tools":[{"type":"function","name":"lookup","parameters":5}]}`
	const groupID = int64(11)
	const fallbackGroupID = int64(22)
	for _, tc := range []struct {
		name  string // entry group scenario
		group *int64 // group bound at authentication
		match bool   // expected rollout enrollment
	}{
		{name: "auth_group", group: new(groupID), match: true},
		{name: "auth_group_missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, handler, c, response, _ := newOpenAITextEntryProbe(t, raw)
			repo := settingstest.NewMemory()
			require.NoError(t, repo.Set(t.Context(), requestpolicy.SettingKey, `{"revision":1,"mode":"strict","rules":[{"entry_group_id":11,"inbound_endpoint":"/v1/responses","requested_model":"alias","mode":"strict","strict_percent":100}]}`))
			handler.options.ParameterPolicy = requestpolicy.New(settings.New(repo))
			require.NoError(t, handler.options.ParameterPolicy.Refresh(t.Context()))
			c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), telemetry.ClientRequestID, "logical-policy"))
			authenticated := &apikey.APIKey{GroupID: tc.group, ModelMapping: map[string]string{"alias": "mapped"}}
			bindParameterEntryGroup(c, authenticated)
			ApplyAPIKeyModelRedirect(c, authenticated)
			model, err := CompositeModelFromRequest(c.Request)
			require.NoError(t, err)
			require.Equal(t, "mapped", model)
			p.key.GroupID = new(fallbackGroupID)
			handler.Responses(c)
			intent, found := requestpolicy.FromContext(c.Request.Context())
			require.Equal(t, tc.match, found)
			if !tc.match {
				require.False(t, strings.Contains(response.Body.String(), "tools[0].parameters"))
				return
			}
			require.Equal(t, http.StatusBadRequest, response.Code)
			require.Contains(t, response.Body.String(), "tools[0].parameters")
			require.Equal(t, groupID, intent.EntryGroupID)
			require.Equal(t, "alias", intent.RequestedModel)
			require.Equal(t, raw, intent.RawJSON())
			require.Nil(t, p.call)
		})
	}
}
