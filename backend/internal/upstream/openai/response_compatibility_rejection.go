package openai

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/TokenFlux/TokenRouter/internal/protocol/wirejson"
)

// response_compatibility_rejection.go distinguishes a standalone HTTP refusal
// from a Responses body that may contain work and must be resolved as such.
// Error status and overload codes classify the failure. Explicit execution
// evidence separately determines whether a capacity refusal can be replayed.

// directResponsesHTTPRejection recognizes a complete standalone error object
// after the HTTP body reaches EOF. Objects with other fields that could carry
// execution evidence go through the ordinary Responses resolver.
func directResponsesHTTPRejection(status int, payload []byte) *ResponsesStreamError {
	if status < http.StatusBadRequest || wirejson.ValidateRequestObject(payload) != nil {
		return nil
	}
	root := gjson.ParseBytes(payload)
	var envelope gjson.Result
	valid := true
	root.ForEach(func(key, value gjson.Result) bool {
		switch key.String() {
		case "error":
			valid = value.IsObject() && !envelope.Exists()
			envelope = value
		case "type":
			valid = value.Type == gjson.String && value.String() == "error"
		case "request_id":
			valid = value.Type == gjson.String
		default:
			valid = false
		}
		return valid
	})
	if !valid || !envelope.Exists() {
		return nil
	}
	if status >= http.StatusInternalServerError && IsOpenAIUpstreamCapacityShedEvent(payload) {
		status = http.StatusServiceUnavailable
	}
	message := strings.TrimSpace(envelope.Get("message").String())
	if message == "" {
		message = http.StatusText(status)
	}
	return &ResponsesStreamError{
		StatusCode: status, EventType: "error", Payload: payload,
		Message: message, DirectHTTPRejection: true,
	}
}

// responsesReplayableCapacityCode identifies capacity errors that are eligible
// for replay when a validated response also carries affirmative pre-execution
// evidence. HTTP status and message text alone leave the outcome unknown.
func responsesReplayableCapacityCode(payload []byte) bool {
	switch OpenAIStreamFailedEventErrorCode(payload) {
	case "server_is_overloaded", "slow_down", "rate_limit_exceeded":
		return true
	default:
		return false
	}
}

// responsesPreExecutionCapacity checks the execution flag beside the error code.
func responsesPreExecutionCapacity(payload []byte) bool {
	return responsesErrorObject(payload).Get("execution_started").Type == gjson.False &&
		responsesReplayableCapacityCode(payload)
}
