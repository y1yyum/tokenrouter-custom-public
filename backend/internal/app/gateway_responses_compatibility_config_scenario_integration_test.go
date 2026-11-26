//go:build integration

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/TokenFlux/TokenRouter/ent"
	gatewayhttp "github.com/TokenFlux/TokenRouter/internal/gateway/httpapi"
	"github.com/TokenFlux/TokenRouter/internal/gateway/responsecompat"
	"github.com/TokenFlux/TokenRouter/internal/infra/telemetry"
	"github.com/TokenFlux/TokenRouter/internal/settings"
	settingspg "github.com/TokenFlux/TokenRouter/internal/settings/postgres"
)

// gateway_responses_compatibility_config_scenario_integration_test.go exercises the
// rollout setting through production HTTP registration, PostgreSQL CAS, and
// two independent gateway upstream receivers. The request fixture evaluates
// the setting before ordinary forwarding so it can count observation traffic.

const (
	// compatibilityAdminPath is the complete mounted settings endpoint.
	compatibilityAdminPath = "/api/v1/admin/settings/responses-request-compatibility"
)

// compatibilityCASBarrier lets two independent stores reach the SQL CAS with
// the same old value before allowing either transaction to proceed.
type compatibilityCASBarrier struct {
	settings.ConditionalRepository                 // real settings repository
	settingKey                     string          // policy whose expectation is observed
	arrivals                       chan<- *string  // raw versions prepared by each reader
	release                        <-chan struct{} // permission to enter SQL CAS
}

// compatibilityAdminResponse captures the settings data and API error reason.
type compatibilityAdminResponse struct {
	status int                   // HTTP status
	reason string                // API error reason
	data   responsecompat.Status // saved policy and instance readback
}

// TestGatewayResponsesCompatibilityConfig tests the real settings route with
// one Luna rule and counts upstream traffic with an observe decision in each
// dispatch. It then races two administrator saves using separate update gates.
func TestGatewayResponsesCompatibilityConfig(t *testing.T) {
	db := newResponsesCompatibilityDatabase(t)
	repo, ok := settingspg.NewSettingRepository(ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))).(settings.ConditionalRepository)
	require.True(t, ok)
	store := settings.New(repo)
	service := responsecompat.New(store)
	httpSettings := newCompatibilityAdminRouter(service)
	tuple := responsecompat.Tuple{
		EffectiveGroupID: responsesCompatibilityGroup, ProviderID: responsesCompatibilityA,
		UpstreamModel: responsesCompatibilityLuna, UpstreamEndpoint: responsesCompatibilityPath,
	}

	initial := callCompatibilityAdmin(t, t.Context(), httpSettings, http.MethodGet, "")
	require.Equal(t, http.StatusOK, initial.status)
	require.Equal(t, responsecompat.Config{Mode: responsecompat.Off, Rules: []responsecompat.Rule{}}, initial.data.Settings)
	require.Nil(t, initial.data.AppliedRevision)
	require.Equal(t, responsecompat.Decision{Mode: responsecompat.Off}, service.Match(tuple, "logical-test"))
	for _, body := range []string{`{}`, `{"revision":null}`, `{"revision":0,"unknown":true}`, `{"revision":0}{}`} {
		require.Equal(t, http.StatusBadRequest, callCompatibilityAdmin(t, t.Context(), httpSettings, http.MethodPut, body).status)
	}

	rule := responsecompat.Rule{Tuple: tuple, Mode: responsecompat.Observe, SyncToStream: true}
	duplicate := compatibilityConfigBody(t, responsecompat.Config{Mode: responsecompat.Off, Rules: []responsecompat.Rule{rule, rule}})
	rejected := callCompatibilityAdmin(t, t.Context(), httpSettings, http.MethodPut, duplicate)
	require.Equal(t, http.StatusBadRequest, rejected.status)
	require.Equal(t, "INVALID_RESPONSES_COMPATIBILITY", rejected.reason)
	_, err := repo.GetValue(t.Context(), responsecompat.SettingKey)
	require.ErrorIs(t, err, settings.ErrSettingNotFound)

	var callbackCount int
	unsubscribe := store.Subscribe(func() {
		callbackCount++
		// Read through the repository from the notification to demonstrate that
		// the row is committed and the request-side snapshot has already advanced.
		raw, err := repo.GetValue(t.Context(), responsecompat.SettingKey)
		require.NoError(t, err)
		require.Contains(t, raw, `"revision":1`)
		require.Equal(t, responsecompat.Decision{Mode: responsecompat.Observe, Revision: 1}, service.Match(tuple, "logical-test"))
	})
	t.Cleanup(unsubscribe)
	config := responsecompat.Config{Mode: responsecompat.Observe, Rules: []responsecompat.Rule{rule}}
	observeBody := compatibilityConfigBody(t, config)
	saved := callCompatibilityAdmin(t, t.Context(), httpSettings, http.MethodPut, observeBody)
	require.Equal(t, http.StatusOK, saved.status)
	require.Equal(t, 1, callbackCount)
	config.Revision = 1
	require.Equal(t, config, saved.data.Settings)
	require.NotNil(t, saved.data.AppliedRevision)
	require.EqualValues(t, 1, *saved.data.AppliedRevision)
	stale := callCompatibilityAdmin(t, t.Context(), httpSettings, http.MethodPut, observeBody)
	require.Equal(t, http.StatusConflict, stale.status)
	require.Equal(t, "RESPONSES_COMPATIBILITY_CONFLICT", stale.reason)
	require.Equal(t, 1, callbackCount)

	cold := responsecompat.New(settings.New(repo))
	readback := callCompatibilityAdmin(t, t.Context(), newCompatibilityAdminRouter(cold), http.MethodGet, "")
	require.Equal(t, http.StatusOK, readback.status)
	require.Equal(t, config, readback.data.Settings)
	require.Nil(t, readback.data.AppliedRevision)
	require.Equal(t, responsecompat.Decision{Mode: responsecompat.Off}, cold.Match(tuple, "logical-test"))

	for _, route := range []struct {
		name        string
		passthrough bool
	}{{name: "ordinary"}, {name: "apikey_passthrough", passthrough: true}} {
		t.Run(route.name, func(t *testing.T) {
			id := "resp_observe_" + route.name
			var decision responsecompat.Decision
			var logicalID string
			fixture := newResponsesCompatibilityFixture(t, db, route.passthrough,
				responsesCompatibilitySuccess(t, id, responsesCompatibilityLuna, "false"),
				responsesCompatibilitySuccess(t, id+"_b", responsesCompatibilityFallbackModel, "false"),
				func(c *gin.Context) {
					logicalID, _ = c.Request.Context().Value(telemetry.ClientRequestID).(string)
					decision = service.Match(tuple, logicalID)
				},
			)
			payload := fmt.Sprintf(`{"model":%q,"input":"return ok","max_tokens":%d,"stream":false}`, responsesCompatibilityModel, responsesCompatibilityLimit)
			reply := fixture.post(t, payload)
			fixture.finish(t)
			require.NotEmpty(t, logicalID)
			require.Equal(t, reply.header.Get("X-TokenRouter-Request-ID"), logicalID)
			require.Equal(t, responsecompat.Decision{Mode: responsecompat.Observe, Revision: 1}, decision)
			assertResponsesCompatibilityRequest(t, fixture.a.snapshot(), responsesCompatibilityA, responsesCompatibilityLuna, "false", route.passthrough)
			require.Empty(t, fixture.b.snapshot())
			assertResponsesCompatibilityReply(t, reply, id, false)
			t.Log("observe revision=1, A=1, B=0, outstream=false, client=JSON")
		})
	}

	assertCompatibilityConcurrentSaves(t, repo, tuple)
}

// newCompatibilityAdminRouter mounts the production settings registrar on the
// same route prefix used by the application's authenticated administration.
// The route inventory test separately verifies that application's middleware.
func newCompatibilityAdminRouter(service *responsecompat.Service) *gin.Engine {
	router := gin.New()
	gatewayhttp.RegisterGatewaySettingsRoutes(router.Group("/api/v1/admin/settings"), gatewayhttp.NewRuntimeSettingsHandler(nil, service, nil))
	return router
}

// callCompatibilityAdmin runs one registered HTTP request and keeps its status
// and body. In concurrent cases each caller owns its own request and recorder.
func callCompatibilityAdmin(t *testing.T, ctx context.Context, handler http.Handler, method, body string) compatibilityAdminResponse {
	t.Helper()
	request := httptest.NewRequestWithContext(ctx, method, compatibilityAdminPath, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	result := compatibilityAdminResponse{status: recorder.Code}
	var wire struct {
		Reason string                `json:"reason"` // response reason when an operation fails
		Data   responsecompat.Status `json:"data"`   // policy observed by the serving instance
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &wire); err != nil {
		t.Errorf("decode administrator response: %v (%s)", err, recorder.Body.String())
	}
	result.reason, result.data = wire.Reason, wire.Data
	return result
}

// compatibilityConfigBody serializes the full policy using its published HTTP
// field names, including revision zero on the initial replacement.
func compatibilityConfigBody(t *testing.T, config responsecompat.Config) string {
	t.Helper()
	body, err := json.Marshal(config)
	require.NoError(t, err)
	return string(body)
}

// assertCompatibilityConcurrentSaves attempts global shutdown from two
// instances at the same revision and proves only the winner publishes it.
func assertCompatibilityConcurrentSaves(t *testing.T, repo settings.ConditionalRepository, tuple responsecompat.Tuple) {
	t.Helper()
	arrivals := make(chan *string, 2)
	release := make(chan struct{})
	barrier := &compatibilityCASBarrier{ConditionalRepository: repo, settingKey: responsecompat.SettingKey, arrivals: arrivals, release: release}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	rule := responsecompat.Rule{Tuple: tuple, Mode: responsecompat.Enforce, SyncToStream: true, EnforcePercent: 100}
	body := compatibilityConfigBody(t, responsecompat.Config{Revision: 1, Mode: responsecompat.Off, Rules: []responsecompat.Rule{rule}})
	services := [2]*responsecompat.Service{responsecompat.New(settings.New(barrier)), responsecompat.New(settings.New(barrier))}
	results := [2]chan compatibilityAdminResponse{make(chan compatibilityAdminResponse, 1), make(chan compatibilityAdminResponse, 1)}
	// Both goroutines hold independent instance gates. A buffered result lets
	// either exit after context cancellation if the test fails before releasing the barrier.
	for i, service := range services {
		router := newCompatibilityAdminRouter(service)
		go func() { results[i] <- callCompatibilityAdmin(t, ctx, router, http.MethodPut, body) }()
	}
	for range services {
		select {
		case expected := <-arrivals:
			require.NotNil(t, expected)
			require.Contains(t, *expected, `"revision":1`)
		case <-ctx.Done():
			t.Fatal("both writers did not reach SQL CAS")
		}
	}
	// Release after both services have already prepared their raw expectation;
	// whichever transaction locks the row second must observe a changed value.
	close(release)
	var codes []int
	for i, result := range results {
		reply := <-result
		codes = append(codes, reply.status)
		status, err := services[i].Get(t.Context())
		require.NoError(t, err)
		require.EqualValues(t, 2, status.Settings.Revision)
		require.Equal(t, responsecompat.Off, status.Settings.Mode)
		if reply.status == http.StatusOK {
			require.NotNil(t, status.AppliedRevision)
			require.EqualValues(t, 2, *status.AppliedRevision)
			require.Equal(t, responsecompat.Decision{Mode: responsecompat.Off, Revision: 2}, services[i].Match(tuple, "logical-test"))
		} else {
			require.Equal(t, "RESPONSES_COMPATIBILITY_CONFLICT", reply.reason)
			require.Nil(t, status.AppliedRevision)
		}
	}
	require.ElementsMatch(t, []int{http.StatusOK, http.StatusConflict}, codes)
	t.Log("two instances: one HTTP 200, one HTTP 409, saved revision=2")
}

// CompareAndSetMultiple pauses both writers before delegating to the real SQL
// transaction. Cancellation leaves the underlying repository untouched.
func (r *compatibilityCASBarrier) CompareAndSetMultiple(ctx context.Context, values map[string]string, expected map[string]*string) error {
	r.arrivals <- expected[r.settingKey]
	select {
	case <-r.release:
		return r.ConditionalRepository.CompareAndSetMultiple(ctx, values, expected)
	case <-ctx.Done():
		return ctx.Err()
	}
}
