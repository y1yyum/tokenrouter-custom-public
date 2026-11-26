package openaiforward

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	forwardcore "github.com/TokenFlux/TokenRouter/internal/gateway/forward"
	gatewayprovider "github.com/TokenFlux/TokenRouter/internal/gateway/provider"
	"github.com/TokenFlux/TokenRouter/internal/gateway/requestpolicy"
	"github.com/TokenFlux/TokenRouter/internal/upstream"
	"github.com/TokenFlux/TokenRouter/internal/upstream/openai"
)

// responses_replay.go decides recovery for converted Responses exchanges in
// both HTTP runners. Reader evidence alone cannot authorize a second send:
// the first body must be closed, and the original client connection and writer
// must still permit recovery. Legacy recovery code handles unconverted
// exchanges separately.
//
// A complete standalone field rejection can authorize a local body repair,
// while execution evidence accompanying a capacity refusal can authorize
// outer selection. Unknown transport outcomes and reader failures without
// sufficient evidence end selection through the existing typed failover
// path, which retains the original read error for downstream classification.

// classifyResponsesSendPreparation preserves the original request deadline
// when aggregation or provider admission consumes the remaining time. It
// stops replay without attributing a body or retry guidance to this exchange.
func classifyResponsesSendPreparation(err error, platform string) error {
	if !errors.Is(err, openai.ErrResponsesDeadline) {
		return err
	}
	// An expired local admission budget says nothing about the provider.
	// Retain the compatibility timeout presentation without inference feedback.
	failure := &forwardcore.UpstreamFailoverError{
		Platform: platform, StatusCode: http.StatusGatewayTimeout,
		Stage: forwardcore.GatewayFailureStageSelection, Scope: forwardcore.GatewayFailureScopeRequest,
		Reason: forwardcore.OpenAIResponsesCompatibilityReason, NextProviderAction: forwardcore.NextProviderStop,
		ClientMessage: responsesTimeoutClientMessage(openai.ErrResponsesDeadline),
	}
	return fmt.Errorf("%w: %w", failure, err)
}

// recoverResponsesExchange decides whether a closed converted response permits
// a local field repair or an outer retry. Callers invoke it after body closure
// with the live client context and the final reader error. A returned body
// reserves one repair ticket and requires rebuilding the next exchange. A
// returned error classifies failure before group and provider routing can act.
//
// A direct HTTP field rejection only authorizes the specific supported repair.
// Capacity recovery uses the reader's finalized affirmative evidence instead.
func recoverResponsesExchange(ctx context.Context, sink upstream.OutputSink, attempt *openai.ResponsesStreamAttempt, resp *http.Response, err error, body []byte, rejected *openai.ResponsesRejectedFieldRetryState, platform string) ([]byte, error) {
	snapshot := attempt.Snapshot()
	clientReady := ctx.Err() == nil && !upstream.NewDeferredOutputContext(sink).Writer.Written()
	var failure *openai.ResponsesStreamError
	errors.As(err, &failure)
	if clientReady && snapshot.CleanEOF && !snapshot.ReplayBlocked && failure != nil && failure.DirectHTTPRejection {
		// HTTP validation identifies the field before execution. Reserve a
		// shared ticket only for a novel replacement while the client can still use it.
		repaired, _, changed, repairErr := openai.NormalizeOpenAIResponsesRejectedFieldRetryBody(failure.StatusCode, body, failure.Payload)
		if repairErr != nil {
			err = errors.Join(err, repairErr)
		} else if changed && requestpolicy.AllowsRepair(ctx, body, repaired) && rejected.Allow(repaired) {
			return repaired, nil
		}
	}
	if clientReady && snapshot.CleanEOF && !snapshot.ReplayBlocked && snapshot.ReplayAllowed && failure != nil &&
		(failure.StatusCode == http.StatusServiceUnavailable || failure.StatusCode == http.StatusTooManyRequests) {
		return nil, responsesCompatibilityFailure(resp, err, platform, forwardcore.NextProviderRetry)
	}
	attempt.BlockReplay()
	return nil, responsesCompatibilityFailure(resp, err, platform, forwardcore.NextProviderStop)
}

// responsesCompatibilityFailure retains the semantic status and raw reader
// error while exposing a typed routing decision. A nil response identifies a
// transport failure, so no upstream body or response headers were observed.
func responsesCompatibilityFailure(resp *http.Response, cause error, platform string, action forwardcore.NextProviderAction) error {
	status := http.StatusBadGateway
	var body []byte
	var headers http.Header
	if resp != nil {
		headers = resp.Header
		if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout {
			status = resp.StatusCode
		}
	}
	var readErr *openai.ResponsesStreamError
	var timeout net.Error
	switch {
	case errors.As(cause, &readErr):
		if status != http.StatusServiceUnavailable || readErr.StatusCode != http.StatusGatewayTimeout {
			status = readErr.StatusCode
		}
		body = readErr.Payload
	case errors.Is(cause, context.DeadlineExceeded), errors.As(cause, &timeout) && timeout.Timeout():
		status = http.StatusGatewayTimeout
	}
	failure := gatewayprovider.NewOpenAIUpstreamFailure(status, headers, body, "", false)
	failure.Platform = platform
	failure.Reason = forwardcore.OpenAIResponsesCompatibilityReason
	failure.NextProviderAction = action
	if status == http.StatusGatewayTimeout {
		failure.ClientMessage = responsesTimeoutClientMessage(cause)
	}
	if action == forwardcore.NextProviderStop {
		failure.RetryableOnSameProvider = false
	}
	return fmt.Errorf("%w: %w", failure, cause)
}

// responsesTimeoutClientMessage maps recognized clock causes to their existing
// client-facing text. Other errors leave the public message unset because
// transport diagnostics can include the upstream URL.
func responsesTimeoutClientMessage(err error) string {
	switch {
	case errors.Is(err, openai.ErrResponsesHeaderTimeout):
		return "Upstream Responses timed out waiting for response headers"
	case errors.Is(err, openai.ErrResponsesFirstOutputTimeout):
		return "Upstream Responses timed out waiting for first output"
	case errors.Is(err, openai.ErrResponsesReadIdleTimeout):
		return "Upstream Responses timed out between upstream reads"
	case errors.Is(err, openai.ErrResponsesDeadline):
		return "Upstream Responses request deadline exceeded"
	default:
		return ""
	}
}
