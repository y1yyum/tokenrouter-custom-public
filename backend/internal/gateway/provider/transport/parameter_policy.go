package transport

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/TokenFlux/TokenRouter/internal/gateway/forward"
	gatewayprovider "github.com/TokenFlux/TokenRouter/internal/gateway/provider"
	"github.com/TokenFlux/TokenRouter/internal/gateway/requestpolicy"
	"github.com/TokenFlux/TokenRouter/internal/infra/httpclient"
)

// parameter_policy.go enforces strict text controls at the final HTTP send.
// Every builder has already applied its model, beta, tool and body edits here.
// When a provably unused limit is omitted, the request's body, length and
// replay reader all describe the same payload.
//
// Restrict the check to text endpoints so auxiliary calls on the same request
// context keep their own body contracts. Local wire mismatches reach the
// provider loop as selection failures. Since they produce no upstream send,
// failure handling must skip transport health updates and may try another
// authorized candidate.

// exchangeText records the actual upstream status for an enrolled logical
// request. Provider adapters can handle a rejection without appending an Ops
// attempt, so its provenance is captured at the shared network boundary.
func (s *Client) exchangeText(req *http.Request, opts httpclient.UpstreamRequestOptions) (*http.Response, error) {
	if req == nil || req.URL == nil || req.Method != http.MethodPost {
		return s.pool.Do(req, opts)
	}
	intent, enrolled := requestpolicy.FromContext(req.Context())
	if !enrolled || parameterEndpoint(req.URL.Path) == "" {
		return s.pool.Do(req, opts)
	}
	intent.RecordUpstreamStatus(0)
	resp, err := s.pool.Do(req, opts)
	if err == nil && resp != nil {
		intent.RecordUpstreamStatus(resp.StatusCode)
	}
	return resp, err
}

// prepareParameterBody checks only text requests with frozen strict controls.
// A successful send may use a read-once body, so checking never enables a
// replay reader that an earlier transport policy deliberately disabled.
func prepareParameterBody(req *http.Request, providerID int64) error {
	if req == nil || req.URL == nil || req.Body == nil || req.Method != http.MethodPost {
		return nil
	}
	intent, strict := requestpolicy.StrictIntent(req.Context())
	if !strict || len(requestpolicy.Requirements(intent)) == 0 {
		return nil
	}
	endpoint := parameterEndpoint(req.URL.Path)
	if endpoint == "" {
		return nil
	}
	reader := req.Body
	if req.GetBody != nil {
		var err error
		reader, err = req.GetBody()
		if err != nil {
			return err
		}
	}
	body, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		return err
	}
	inherited := hasOpaqueToolSession(req.Header)
	prepared, omitted, err := requestpolicy.PrepareOutbound(intent, endpoint, body, !inherited && !requestpolicy.RewriteDisabled(req.Context()))
	if err == nil && omitted == 0 && intent.Endpoint == requestpolicy.EndpointResponses {
		limit := intent.Lookup("max_tool_calls")
		model := gjson.GetBytes(body, "model").String()
		if limit.Origin == requestpolicy.OriginExplicit && limit.Raw != "null" &&
			!gatewayprovider.ParameterToolLimitSupported(req.Context(), providerID, model, intent.RawJSON(), body, inherited) {
			err = &requestpolicy.WireMismatch{Path: "max_tool_calls"}
		}
	}
	if requestpolicy.IsWireMismatch(err) {
		_ = req.Body.Close()
		return fmt.Errorf("%w: %w", &forward.UpstreamFailoverError{
			Stage: forward.GatewayFailureStageSelection, Scope: forward.GatewayFailureScopeRequest,
			StatusCode: http.StatusServiceUnavailable, ClientStatusCode: http.StatusServiceUnavailable,
			ClientMessage: err.Error(), NextProviderAction: forward.NextProviderRetry,
		}, err)
	}
	if err != nil {
		return err
	}
	if req.GetBody == nil || !bytes.Equal(body, prepared) {
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(prepared))
		req.ContentLength = int64(len(prepared))
		if req.GetBody != nil {
			req.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(prepared)), nil
			}
		}
	}
	gatewayprovider.RecordParameterToolLimit(req.Context(), omitted)
	return nil
}

// hasOpaqueToolSession detects final wire headers that can select state beyond
// the two inspected tool arrays. Those requests cannot discard the active cap.
func hasOpaqueToolSession(headers http.Header) bool {
	for _, name := range []string{"X-Codex-Turn-State", "conversation_id", "OpenAI-Conversation-ID", "session_id"} {
		if strings.TrimSpace(headers.Get(name)) != "" {
			return true
		}
	}
	return false
}

// parameterEndpoint identifies text encoders from the final endpoint path.
// Vertex and Bedrock Messages use provider-specific path suffixes.
func parameterEndpoint(path string) string {
	switch {
	case strings.HasSuffix(path, "/responses"):
		return requestpolicy.EndpointResponses
	case strings.HasSuffix(path, "/chat/completions"):
		return requestpolicy.EndpointChat
	case strings.HasSuffix(path, "/messages"), strings.HasSuffix(path, "/invoke"), strings.HasSuffix(path, "/invoke-with-response-stream"),
		strings.Contains(path, "/publishers/anthropic/"):
		return requestpolicy.EndpointMessages
	case strings.HasSuffix(path, ":generateContent"), strings.HasSuffix(path, ":streamGenerateContent"):
		return "gemini"
	default:
		return ""
	}
}
