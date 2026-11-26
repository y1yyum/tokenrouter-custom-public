//go:build integration

package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/TokenFlux/TokenRouter/ent"
	gatewayhttp "github.com/TokenFlux/TokenRouter/internal/gateway/httpapi"
	gatewaypg "github.com/TokenFlux/TokenRouter/internal/gateway/postgres"
	"github.com/TokenFlux/TokenRouter/internal/gateway/requestpolicy"
	"github.com/TokenFlux/TokenRouter/internal/gateway/responsecompat"
	"github.com/TokenFlux/TokenRouter/internal/pkg/apperror"
	"github.com/TokenFlux/TokenRouter/internal/settings"
	settingspg "github.com/TokenFlux/TokenRouter/internal/settings/postgres"
)

// gateway_parameter_policy_scenario_integration_test.go drives registered settings and
// the three production entry handlers on a migrated test database. A real
// local listener records whether validation returned before a provider send.
// An authority read in the listener checks the cohort while HTTP is in flight.

// TestGatewayParameterPolicyEntrypoints exercises identical bad schemas in
// observe and strict, with provider receipts, persisted phase and arm, and
// source attribution. Independent settings also enroll one request in both phases.
func TestGatewayParameterPolicyEntrypoints(t *testing.T) {
	db := qualityTestDatabase(t)
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	store := provideSettingsStore(client)
	parameters, responses := requestpolicy.New(store), responsecompat.New(store)
	router := gin.New()
	gatewayhttp.RegisterGatewaySettingsRoutes(router.Group("/api/v1/admin/settings"), gatewayhttp.NewRuntimeSettingsHandler(nil, responses, parameters))
	admin := func(method string, code int, body string) gjson.Result {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequestWithContext(t.Context(), method, "/api/v1/admin/settings/request-parameter-policy", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)
		require.Equal(t, code, recorder.Code, recorder.Body.String())
		return gjson.Parse(recorder.Body.String())
	}
	initial := admin(http.MethodGet, http.StatusOK, "").Get("data")
	require.Equal(t, "off", initial.Get("settings.mode").String())
	require.Equal(t, "null", initial.Get("applied_revision").Raw)
	admin(http.MethodPut, http.StatusBadRequest, `{"mode":"strict"}`)

	const model = "gpt-6-luna"
	const messagesModel = "claude-haiku-5-5"
	entries := []struct {
		path, model, fields, field string // inbound protocol, raw model and invalid schema fragment
	}{
		{requestpolicy.EndpointMessages, messagesModel, `,"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"x":{"$ref":"#/$defs/missing"}}}}]`, "tools[0].input_schema"},
		{requestpolicy.EndpointChat, model, `,"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"x":{"$ref":"#/$defs/missing"}}}}}]`, "tools[0].function.parameters"},
		{requestpolicy.EndpointResponses, model, `,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"x":{"$ref":"#/$defs/missing"}}}}]`, "tools[0].parameters"},
	}
	config := requestpolicy.Config{Mode: requestpolicy.ModeObserve}
	for _, entry := range entries {
		config.Rules = append(config.Rules, requestpolicy.Rule{
			Entry: requestpolicy.Entry{EntryGroupID: parameterFixtureGroup, Endpoint: entry.path, RequestedModel: entry.model},
			Mode:  requestpolicy.ModeStrict, StrictPercent: 100,
		})
	}
	duplicate := config
	duplicate.Rules = append(append([]requestpolicy.Rule(nil), config.Rules...), config.Rules[0])
	admin(http.MethodPut, http.StatusBadRequest, parameterPolicyConfigBody(t, duplicate))
	status := admin(http.MethodPut, http.StatusOK, parameterPolicyConfigBody(t, config)).Get("data")
	require.EqualValues(t, 1, status.Get("settings.revision").Int())
	require.EqualValues(t, 1, status.Get("applied_revision").Int())
	require.True(t, status.Get("fresh").Bool())
	admin(http.MethodPut, http.StatusConflict, parameterPolicyConfigBody(t, config))

	configure := func(input *gatewayHTTPFixtureInput) {
		input.ParameterPolicy = parameters
		input.Source.Responses.Compatibility = responses
		input.Source.Responses.Cohorts = gatewaypg.NewCompatibilityCohorts(db)
	}
	for _, mode := range []requestpolicy.Mode{requestpolicy.ModeObserve, requestpolicy.ModeStrict} {
		for _, entry := range entries {
			t.Run(string(mode)+entry.path, func(t *testing.T) {
				// The test crosses real HTTP listeners. Renew the local snapshot
				// before each case so an unrelated slow case cannot age it out.
				require.NoError(t, parameters.Refresh(t.Context()))
				reply := runParameterRequest(t, db, entry.path, entry.path, parameterPayload(entry.path, entry.model, false, entry.fields),
					func(w http.ResponseWriter, r *http.Request, _ []byte) {
						var pending int
						err := db.QueryRowContext(r.Context(), `SELECT count(*) FROM gateway_compatibility_cohorts WHERE phase=2 AND endpoint=$1 AND outcome IS NULL`, entry.path).Scan(&pending)
						if err != nil || pending != 1 {
							t.Errorf("expected one in-flight entry, found %d: %v", pending, err)
						}
						mixedUpstreamResponse(w, r, false)
					}, configure)
				if mode == requestpolicy.ModeStrict {
					require.Equal(t, http.StatusBadRequest, reply.status, string(reply.body))
					require.Contains(t, gjson.GetBytes(reply.body, "error.message").String(), entry.field)
					require.Equal(t, entry.path == requestpolicy.EndpointMessages, gjson.GetBytes(reply.body, "type").Exists())
					require.Empty(t, reply.receipts)
					assertParameterOps(t, db, reply, entry.path, entry.model, false)
					assertParameterCohort(t, db, reply.id, 2, string(mode), "parameter_400", "entry", entry.path, entry.model)
				} else {
					require.Equal(t, http.StatusOK, reply.status, string(reply.body))
					require.Len(t, reply.receipts, 1)
					assertParameterCohort(t, db, reply.id, 1, string(mode), "completed", "", entry.path, entry.model)
				}
			})
		}
		if mode == requestpolicy.ModeObserve {
			config.Mode, config.Revision = requestpolicy.ModeStrict, 1
			admin(http.MethodPut, http.StatusOK, parameterPolicyConfigBody(t, config))
		}
	}

	first := responsecompat.Rule{Tuple: responsecompat.Tuple{
		EffectiveGroupID: parameterFixtureGroup, ProviderID: parameterFixtureProvider,
		UpstreamModel: model, UpstreamEndpoint: requestpolicy.EndpointResponses,
	}, Mode: responsecompat.Observe, SyncToStream: true}
	_, err := responses.Update(t.Context(), responsecompat.Config{Mode: responsecompat.Observe, Rules: []responsecompat.Rule{first}})
	require.NoError(t, err)
	config.Mode, config.Revision = requestpolicy.ModeObserve, 2
	admin(http.MethodPut, http.StatusOK, parameterPolicyConfigBody(t, config))
	sendRejected := func() parameterReply {
		return runParameterRequest(t, db, requestpolicy.EndpointResponses, requestpolicy.EndpointResponses,
			parameterPayload(requestpolicy.EndpointResponses, model, false, ""), func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				writeParameterFixtureRejection(t, w, parameterProbe{upMessage: "only supports streaming generation requests"})
			}, configure)
	}
	both := sendRejected()
	require.Equal(t, http.StatusBadRequest, both.status, string(both.body))
	assertParameterCohort(t, db, both.id, 3, "observe", "parameter_400", "upstream", requestpolicy.EndpointResponses, model)
	var target bool
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT target_parameter_error FROM gateway_compatibility_cohorts WHERE phase=1 AND request_id=$1`, both.id).Scan(&target))
	require.True(t, target)

	config.Mode, config.Revision = requestpolicy.ModeOff, 3
	admin(http.MethodPut, http.StatusOK, parameterPolicyConfigBody(t, config))
	firstOnly := sendRejected()
	var phase, rows int
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT min(phase),count(*) FROM gateway_compatibility_cohorts WHERE request_id=$1`, firstOnly.id).Scan(&phase, &rows))
	require.Equal(t, 1, rows)
	require.Equal(t, responsecompat.ResponsesPhase, phase)

	for arm, expected := range map[string]int{"observe": 4, "strict": 3} {
		var total, finished, pending int
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*), count(outcome), count(*) FILTER(WHERE outcome IS NULL)
			FROM gateway_compatibility_cohorts WHERE phase=2 AND arm=$1`, arm).Scan(&total, &finished, &pending))
		require.Equal(t, expected, total)
		require.Equal(t, total, finished+pending)
		require.Zero(t, pending)
		t.Logf("D2 arm=%s total=%d final=%d in_flight=%d", arm, total, finished, pending)
	}
	assertParameterPolicyConcurrentCAS(t, client, config)
}

// parameterPolicyConfigBody serializes the test replacement using the production JSON shape.
func parameterPolicyConfigBody(t *testing.T, config requestpolicy.Config) string {
	t.Helper()
	body, err := json.Marshal(config)
	require.NoError(t, err)
	return string(body)
}

// assertParameterCohort checks identity, final classification and independent target metrics.
func assertParameterCohort(t *testing.T, db *sql.DB, id string, revision int64, arm, outcome, source, endpoint, model string) {
	t.Helper()
	var observedRevision, group int64
	var observedArm, observedOutcome, observedSource, observedEndpoint, observedModel string
	var provider sql.NullInt64
	var target bool
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT revision, arm, outcome, COALESCE(rejection_source,''), endpoint, model, group_id, provider_id, target_parameter_error
		FROM gateway_compatibility_cohorts WHERE phase=2 AND request_id=$1`, id).
		Scan(&observedRevision, &observedArm, &observedOutcome, &observedSource, &observedEndpoint, &observedModel, &group, &provider, &target))
	require.Equal(t, revision, observedRevision)
	require.Equal(t, arm, observedArm)
	require.Equal(t, outcome, observedOutcome)
	require.Equal(t, source, observedSource)
	require.Equal(t, endpoint, observedEndpoint)
	require.Equal(t, model, observedModel)
	require.EqualValues(t, parameterFixtureGroup, group)
	require.False(t, provider.Valid)
	require.False(t, target)
}

// assertParameterPolicyConcurrentCAS holds two independent settings sessions
// at the database boundary after reading the same current revision. Exactly
// one replacement may publish even though both local instance gates admitted it.
func assertParameterPolicyConcurrentCAS(t *testing.T, client *ent.Client, config requestpolicy.Config) {
	t.Helper()
	repo, ok := settingspg.NewSettingRepository(client).(settings.ConditionalRepository)
	require.True(t, ok)
	arrivals, release := make(chan *string, 2), make(chan struct{})
	barrier := &compatibilityCASBarrier{ConditionalRepository: repo, settingKey: requestpolicy.SettingKey, arrivals: arrivals, release: release}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	config.Revision = 4
	services := [2]*requestpolicy.Service{requestpolicy.New(settings.New(barrier)), requestpolicy.New(settings.New(barrier))}
	results := [2]chan error{make(chan error, 1), make(chan error, 1)}
	for i, service := range services {
		go func() {
			_, err := service.Update(ctx, config)
			results[i] <- err
		}()
	}
	for range services {
		select {
		case raw := <-arrivals:
			require.NotNil(t, raw)
			require.Contains(t, *raw, `"revision":4`)
		case <-ctx.Done():
			t.Fatal("both writers did not reach the settings CAS")
		}
	}
	close(release)
	var reasons []string
	for i, result := range results {
		err := <-result
		reasons = append(reasons, apperror.Reason(err))
		status, readErr := services[i].Get(t.Context())
		require.NoError(t, readErr)
		require.EqualValues(t, 5, status.Settings.Revision)
		if err == nil {
			require.NotNil(t, status.AppliedRevision)
			require.EqualValues(t, 5, *status.AppliedRevision)
		} else {
			require.Nil(t, status.AppliedRevision)
		}
	}
	require.ElementsMatch(t, []string{apperror.UnknownReason, "REQUEST_PARAMETER_POLICY_CONFLICT"}, reasons)
	t.Log("cross-instance CAS: one publisher, one conflict, saved revision=5")
}
