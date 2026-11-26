package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOpenAIRecoverResponsesPanic_WritesFallbackResponse panics inside a
// deferred Responses recovery call before output. It checks recovery without
// another panic and a JSON 502 upstream-error fallback.
func TestOpenAIRecoverResponsesPanic_WritesFallbackResponse(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	h := &OpenAITextHandler{backend: openAITextHTTPBackend{}}
	streamStarted := false
	require.NotPanics(t, func() {
		func() {
			defer h.recoverResponsesPanic(c, &streamStarted)
			panic("test panic")
		}()
	})

	require.Equal(t, http.StatusBadGateway, w.Code)

	var parsed map[string]any
	err := json.Unmarshal(w.Body.Bytes(), &parsed)
	require.NoError(t, err)

	errorObj, ok := parsed["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "upstream_error", errorObj["type"])
	assert.Equal(t, "Upstream request failed", errorObj["message"])
}

// TestOpenAIRecoverResponsesPanic_NoPanicNoWrite runs deferred Responses
// recovery on a normally returning call. It checks absence of a panic and that
// no response bytes or status are committed.
func TestOpenAIRecoverResponsesPanic_NoPanicNoWrite(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	h := &OpenAITextHandler{backend: openAITextHTTPBackend{}}
	streamStarted := false
	require.NotPanics(t, func() {
		func() {
			defer h.recoverResponsesPanic(c, &streamStarted)
		}()
	})

	require.False(t, c.Writer.Written())
	assert.Equal(t, "", w.Body.String())
}

// TestOpenAIRecoverResponsesPanicUsesCommittedProtocol exercises panic recovery
// after unary JSON and native SSE have both committed. The JSON remains intact,
// while the genuine stream receives one terminal despite its unset marker.
func TestOpenAIRecoverResponsesPanicUsesCommittedProtocol(t *testing.T) {
	for _, tc := range []struct {
		name, media, body string
		status            int
	}{
		{name: "json", media: "application/json", body: `{"original":true}`, status: http.StatusTeapot},
		{name: "native_sse", media: "text/event-stream", body: ": ready\n\n", status: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Data(tc.status, tc.media, []byte(tc.body))
			h := &OpenAITextHandler{backend: openAITextHTTPBackend{}}
			streamStarted := false
			require.NotPanics(t, func() {
				func() {
					defer h.recoverResponsesPanic(c, &streamStarted)
					panic("test panic")
				}()
			})
			require.Equal(t, tc.status, w.Code)
			if tc.media == "application/json" {
				require.Equal(t, tc.body, w.Body.String())
				return
			}
			require.Contains(t, w.Body.String(), tc.body+"event: response.failed\n")
		})
	}
}
