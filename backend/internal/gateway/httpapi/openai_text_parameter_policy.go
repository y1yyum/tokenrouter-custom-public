package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/TokenFlux/TokenRouter/internal/apikey"
	"github.com/TokenFlux/TokenRouter/internal/gateway/execution"
	gatewayprovider "github.com/TokenFlux/TokenRouter/internal/gateway/provider"
	"github.com/TokenFlux/TokenRouter/internal/gateway/requestpolicy"
	"github.com/TokenFlux/TokenRouter/internal/gateway/responsecompat"
	"github.com/TokenFlux/TokenRouter/internal/infra/telemetry"
	"github.com/TokenFlux/TokenRouter/internal/protocol/wirejson"
)

// openai_text_parameter_policy.go enrolls authenticated text requests in D2
// and handles strict entry validation. These operations use only the original
// entry group, protocol endpoint, model, and validated JSON. The HTTP handler
// keeps the resulting context for all subsequent preparation and execution.
//
// A request rejected here cannot reach provider selection. Enrollment occurs
// first so the denominator includes those early failures, while the phase's
// final result records entry rejection without relying on upstream error text.
// Generic execution evidence provides a completion fallback when the protocol
// reader has not recorded a more specific delivered terminal.

const (
	// parameterEntryGroupKey retains the group bound by authentication before effective routing changes it.
	parameterEntryGroupKey = "gateway_parameter_entry_group"
)

// parameterOriginalBodyKey identifies bytes held before an authentication rewrite.
type parameterOriginalBodyKey struct{}

// bindParameterEntryGroup saves the authenticated policy scope before any group
// fallback middleware runs. Missing group is stored as zero so a later
// effective group cannot qualify an originally ungrouped request.
func bindParameterEntryGroup(c *gin.Context, key *apikey.APIKey) {
	var groupID int64
	if key.GroupID != nil {
		groupID = *key.GroupID
	}
	c.Set(parameterEntryGroupKey, groupID)
}

// rejectByParameterPolicy binds the initial matched intent and registers its
// cohort. Call after basic original-object validation and before rewriting the
// document. The authentication group remains the match key even when route
// guards have already resolved fallback. A true return means the native
// error envelope was sent and the entry handler must stop preparation.
func (h *OpenAITextHandler) rejectByParameterPolicy(c *gin.Context, key *apikey.APIKey, endpoint string, body []byte) bool {
	if h.options.ParameterPolicy == nil {
		return false
	}
	groupID := c.GetInt64(parameterEntryGroupKey)
	if _, boundByAuth := c.Get(parameterEntryGroupKey); !boundByAuth && key != nil && key.GroupID != nil {
		groupID = *key.GroupID
	}
	if original, ok := c.Request.Context().Value(parameterOriginalBodyKey{}).([]byte); ok {
		if endpoint != EndpointMessages {
			var err error
			original, err = NormalizeLenientJSONRequestBody(original, h.options.MaxBodyBytes)
			if err != nil {
				return false
			}
		}
		if wirejson.ValidateRequestObject(original) != nil {
			return false
		}
		body = original
	}
	requestID, _ := c.Request.Context().Value(telemetry.ClientRequestID).(string)
	ctx, matched := h.options.ParameterPolicy.Admit(c.Request.Context(), groupID, endpoint, requestID, body)
	if !matched {
		return false
	}
	ctx = gatewayprovider.WithParameterAttempts(ctx)
	c.Request = c.Request.WithContext(ctx)
	intent, _ := requestpolicy.FromContext(ctx)
	beginCompatibilityCohort(c, parametersCohortKey, h.options.CompatibilityCohorts, responsecompat.Cohort{
		Phase: responsecompat.ParameterPhase, RequestID: requestID, Revision: intent.Revision,
		Arm: responsecompat.Mode(intent.Mode), ClientStream: gjson.GetBytes(body, "stream").Bool(),
		Tuple: responsecompat.Tuple{EffectiveGroupID: intent.EntryGroupID, UpstreamModel: intent.RequestedModel, UpstreamEndpoint: intent.Endpoint},
	})
	err := validateParameterSchemas(intent.Mode, intent.Endpoint, intent.RawJSON())
	if err == nil {
		return false
	}
	RecordParameterRejection(c, responsecompat.RejectionEntry)
	if endpoint == EndpointMessages {
		h.anthropicErrorResponse(c, http.StatusBadRequest, "invalid_request_error", err.Error())
	} else {
		h.errorResponse(c, http.StatusBadRequest, "invalid_request_error", err.Error())
	}
	return true
}

// retainParameterOriginalBody holds the body's original bytes when a standard
// generation request is rewritten in authentication. Rewrite operations leave
// their input untouched, so the deferred capture can borrow it. Later rewrites
// keep the first byte slice and admission copies it only on a matched request.
func retainParameterOriginalBody(request *http.Request, body []byte) {
	if request == nil || request.URL == nil || request.Method != http.MethodPost {
		return
	}
	switch strings.TrimRight(request.URL.Path, "/") {
	case EndpointMessages, "/antigravity" + EndpointMessages, EndpointChatCompletions, EndpointResponses, "/chat/completions", "/responses", "/openai/v1/responses":
		ctx := request.Context()
		if ctx.Value(parameterOriginalBodyKey{}) == nil {
			*request = *request.WithContext(context.WithValue(ctx, parameterOriginalBodyKey{}, body))
		}
	}
}

// RecordParameterRejection records the source of a final parameter rejection.
// Call at a terminal error decision, after applicable recovery, so provisional
// candidate or upstream errors cannot become the source for a later success.
func RecordParameterRejection(c *gin.Context, source string) {
	if cohort := compatibilityCohortFrom(c, parametersCohortKey); cohort != nil && cohort.rejectionSource == "" {
		cohort.rejectionSource = source
	}
}

// recordParameterExecution records a codec-observed disconnect or the existing
// unary completion fallback. SSE completion remains owned by the accepted
// HTTP frames, including a local terminal written after execution returns.
func recordParameterExecution(c *gin.Context, result execution.ExecutionResult, err error) {
	cohort := compatibilityCohortFrom(c, parametersCohortKey)
	if cohort == nil || cohort.delivered != "" {
		return
	}
	switch {
	case result.Attempt.ClientDisconnect:
		cohort.clientDisconnect = true
	case err == nil && result.Attempt.Served && !cohort.entry.ClientStream && !CommittedSSE(c):
		cohort.unaryServed = true
	}
}
