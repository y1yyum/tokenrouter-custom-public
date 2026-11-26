package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/TokenFlux/TokenRouter/internal/apikey"
	gatewayprovider "github.com/TokenFlux/TokenRouter/internal/gateway/provider"
	"github.com/TokenFlux/TokenRouter/internal/gateway/responsecompat"
	"github.com/TokenFlux/TokenRouter/internal/infra/telemetry"
	"github.com/TokenFlux/TokenRouter/internal/provider"
)

// TestPrepareCompatibilitySend_ColdSnapshotSkipsBody verifies that an unenrolled
// request with compatibility disabled keeps its configured header and does not
// open another copy of the mapped request body.
func TestPrepareCompatibilitySend_ColdSnapshotSkipsBody(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.WithValue(t.Context(), telemetry.ClientRequestID, "cold-compatibility")
	c.Request = httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/responses", nil)
	c.Set(responsesForegroundJSONKey, true)
	c.Set("api_key", &apikey.APIKey{GroupID: new(int64(28))})
	executor := &OpenAIResponsesExecutor{Compatibility: responsecompat.New(nil)}
	selected := gatewayprovider.NewExecutionProvider(&provider.Record{ID: 57, Platform: "openai", Type: "apikey"})
	outbound := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-6-luna"}`))
	outbound.Header.Set("Accept", "text/plain")
	outbound.GetBody = func() (io.ReadCloser, error) {
		t.Fatal("disabled policy reopened the prepared body")
		return nil, nil
	}

	prepare := executor.prepareCompatibilitySend(ctx, c, selected, time.Now(), nil)
	require.NotNil(t, prepare)
	attempt, err := prepare(outbound)
	require.NoError(t, err)
	require.Nil(t, attempt)
	require.Equal(t, []string{"text/plain"}, outbound.Header.Values("Accept"))
	_, admitted := c.Get(responsesCompatibilityAdmissionKey)
	require.False(t, admitted)
}
