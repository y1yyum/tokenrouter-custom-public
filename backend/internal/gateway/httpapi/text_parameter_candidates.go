package httpapi

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	gatewayprovider "github.com/TokenFlux/TokenRouter/internal/gateway/provider"
	"github.com/TokenFlux/TokenRouter/internal/gateway/requestpolicy"
	"github.com/TokenFlux/TokenRouter/internal/gateway/requeststate"
	"github.com/TokenFlux/TokenRouter/internal/gateway/responsecompat"
	"github.com/TokenFlux/TokenRouter/internal/pkg/apperror"
	"github.com/TokenFlux/TokenRouter/internal/protocol"
)

// text_parameter_candidates.go presents strict selection and final-route
// failures in the originating client protocol. Candidate rejection is a
// local outcome, and capacity failure retains the gateway availability
// classification. The final-route check runs after selection because effort
// policy and hydration can alter the model or protocol of a ranked candidate.
//
// Selection can prove a field conflict by exhausting the entire authorized
// pool. Dispatch only sees one target, so a mismatch there yields temporary
// unavailability. It cannot retroactively assert that every route was
// incompatible or emit an upstream failure observation.

// RecheckTextParameterCandidate verifies the resolved route before dispatch.
// A previously selected route that cannot still be verified yields temporary
// unavailability because dispatch has no proof about the remaining pool.
func RecheckTextParameterCandidate(ctx context.Context, c *gin.Context, target *gatewayprovider.ExecutionProvider, model string, source protocol.ProtocolID, started bool) error {
	intent, _ := requestpolicy.FromContext(ctx)
	requirements := requestpolicy.Requirements(intent)
	if gatewayprovider.CheckParameterCandidate(ctx, target, model, intent, requirements).Support == gatewayprovider.ParameterSupported {
		return nil
	}
	err := requestpolicy.CandidateUnavailable()
	WriteTextParameterCandidateError(c, source, err, started)
	return err
}

// WriteTextParameterCandidateError handles a terminal candidate policy failure
// in the source protocol. Call after caller cancellation or replay decisions.
// The handled marker prevents later forwarding cleanup from writing a second
// terminal. The cohort records the candidate as the rejection source only
// when the final result is a proved field conflict.
func WriteTextParameterCandidateError(c *gin.Context, source protocol.ProtocolID, err error, started bool) bool {
	if !requestpolicy.IsCandidateError(err) {
		return false
	}
	if c == nil || IsResponseCommitted(c) || FailoverClientGone(c) {
		return true
	}
	failure := apperror.FromError(err)
	kind := "api_error"
	if failure.Code == http.StatusBadRequest {
		kind = "invalid_request_error"
		MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalPolicyDenied)
		RecordParameterRejection(c, responsecompat.RejectionCandidate)
	} else {
		MarkOpsRoutingCapacityLimited(c)
	}
	if source == protocol.ProtocolAnthropicMessages {
		DefaultOpenAIErrorOutput().WriteAnthropicStreamingError(c, int(failure.Code), kind, failure.Message, started)
	} else {
		DefaultOpenAIErrorOutput().WriteStreamingErrorWithCode(c, int(failure.Code), kind, failure.Reason, failure.Message, started, false)
	}
	MarkResponseCommitted(c)
	return true
}

// WriteStrictParameterCapacityError reports exhausted provider waiting as
// route unavailability for constrained strict requests. User concurrency and
// ordinary requests continue through their existing admission error handlers.
func WriteStrictParameterCapacityError(c *gin.Context, started bool) bool {
	ctx := c.Request.Context()
	intent, _ := requestpolicy.FromContext(ctx)
	if len(requestpolicy.Requirements(intent)) == 0 {
		return false
	}
	source, _ := requeststate.ClientProtocolFromContext(ctx)
	return WriteTextParameterCandidateError(c, source, requestpolicy.CandidateUnavailable(), started)
}
