package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/TokenFlux/TokenRouter/internal/gateway/errorpolicy"
	"github.com/TokenFlux/TokenRouter/internal/gateway/responsecompat"
	upstreamopenai "github.com/TokenFlux/TokenRouter/internal/upstream/openai"
)

// openai_failover_error.go selects final OpenAI failover error presentation
// and relays bounded retry guidance.
// Provider retry exhaustion presents classification before administrator error
// rules. Oversized requests and unsupported continuation end first, followed
// by credential, capacity, silent-refusal and cyber-warning errors; a normal
// failure can then match the original status and body against OpenAI
// presentation rules.
//
// The original upstream evidence stays distinct from client display fields, so
// a credential or capacity classification can select its own status and
// message. Retry-After relay uses a bounded header policy accepting at most
// one week into the future, while the default status mapping hides provider
// authentication details behind gateway errors. Observation hooks run only on
// the corresponding final branch, and the stream-aware writer controls whether
// that presentation becomes JSON or a terminal event.

// OpenAIFailoverError carries final failure evidence and branch-specific
// client presentation. Its original Status and Body feed administrator rules;
// display fields serve classified errors before ordinary status mapping.
type OpenAIFailoverError struct {
	// Status, ClientStatus and CredentialStatus keep source status separate from
	// classified presentation. Status supplies rule and Ops evidence; the other
	// values are read only in their selected capacity or credential branch.
	// Status: original upstream failure status.
	// ClientStatus supplies the capacity branch's display status. A nonpositive
	// value selects 503 when CapacityShed has a nonblank ClientMessage.
	// ClientStatus: capacity rejection client status.
	// CredentialStatus: credential rejection client status.
	Status, ClientStatus, CredentialStatus int
	// ClientMessage, CredentialMessage, TooLargeMessage, SilentMessage,
	// CyberMessage and UpstreamMessage belong to the ordered presentation
	// branches. Classified messages precede rules; UpstreamMessage supplies
	// ordinary matching-rule output and the final upstream observation.
	// ClientMessage: capacity or continuation message; blank continuation uses
	// its fixed requirement message, and blank capacity skips that branch.
	// CredentialMessage: classified credential rejection message.
	// TooLargeMessage: classified request-size rejection message.
	// SilentMessage: classified silent-refusal rejection message.
	// CyberMessage: classified cyber-warning rejection message.
	// UpstreamMessage: ordinary upstream diagnostic message.
	ClientMessage, CredentialMessage, TooLargeMessage, SilentMessage, CyberMessage, UpstreamMessage string
	// upstream response retry headers
	Headers http.Header
	Body    []byte // original rule-matching response bytes
	// TooLarge, ContinuationUnsupported, Credential, CapacityShed, SilentRefusal
	// and CyberWarning select final branches in that order. Earlier true flags
	// win, so their associated messages are used before administrator rules.
	// TooLarge: request exceeds upstream limit.
	// ContinuationUnsupported: HTTP continuation capability missing.
	// Credential: provider credential failure classified.
	// CapacityShed: request-scoped capacity failure classified.
	// SilentRefusal: empty usable output classified.
	// CyberWarning: upstream cyber warning classified.
	TooLarge, ContinuationUnsupported, Credential, CapacityShed, SilentRefusal, CyberWarning bool
	// ResponsesCompatibility identifies the strict reader's classified failure.
	// Its status survives exhaustion and administrator presentation overrides.
	ResponsesCompatibility bool
}

// ErrorRuleMatcher supplies final presentation rules for upstream failure
// evidence.
type ErrorRuleMatcher interface {
	// MatchRule returns the first applicable presentation rule for platform,
	// upstream status and body, or nil when none applies. Implementations keep
	// matching priority consistent with the configured rule order.
	MatchRule(string, int, []byte) *errorpolicy.ErrorPassthroughRule
}

// FailoverErrorHooks supplies synchronous upstream observation and
// monitoring-skip callbacks.
type FailoverErrorHooks struct {
	// final upstream error observation
	Upstream       func(*gin.Context, int, string)
	SkipMonitoring func(*gin.Context) // matched-rule monitoring skip marker
}

// WriteOpenAIFailoverExhausted selects a final client presentation after
// provider retry ends. Callers supply observation hooks and a stream-aware
// writer. A nil failure uses the generic 502 mapping, and classified failure
// branches take precedence over administrator rules.
//
// TooLarge and ContinuationUnsupported return before retry-header relay;
// credential, capacity, silent-refusal and cyber-warning presentation follow
// it. Rules match platform openai against the original Status and Body and can
// replace display status, message or monitoring behavior. An unmatched
// ordinary failure records its upstream message before using the default
// status mapping. A converted policy refusal retains its existing 400
// presentation and records the explicit refusal after successful JSON delivery.
func WriteOpenAIFailoverExhausted(c *gin.Context, failure *OpenAIFailoverError, started bool, rules ErrorRuleMatcher, hooks FailoverErrorHooks, write func(*gin.Context, int, string, string, bool)) {
	if failure == nil {
		status, kind, message := MapOpenAIUpstreamError(http.StatusBadGateway)
		write(c, status, kind, message, started)
		return
	}
	if failure.ResponsesCompatibility {
		outcome := responsesCompatibilityErrorOutcome(failure.Status)
		kind := "upstream_error"
		message := strings.TrimSpace(failure.ClientMessage)
		if failure.Status == http.StatusBadRequest && failure.CyberWarning {
			outcome = responsecompat.OutcomeRefused
			kind = "invalid_request_error"
			message = strings.TrimSpace(failure.CyberMessage)
		}
		if message == "" {
			message = http.StatusText(failure.Status)
		}
		if outcome != "" {
			hooks.Upstream(c, failure.Status, message)
		}
		// Converted output is always unary. An existing response, including an
		// unexpected SSE commit, cannot accept a replacement JSON document.
		gone := FailoverClientGone(c)
		if c.Writer.Written() || TerminalSSEDelivered(c) {
			RequestLogger(c, "gateway.responses_compatibility").Debug("openai.responses_error_output_already_committed")
			return
		}
		if gone {
			recordResponsesCohortDelivery(c, responsecompat.OutcomeClientGone)
			return
		}
		// A previous candidate may have left retry guidance on this writer.
		// Replace it only with a single validated value from the final attempt.
		c.Writer.Header().Del("Retry-After")
		if values := failure.Headers.Values("Retry-After"); len(values) == 1 && !strings.ContainsAny(values[0], "\r\n") {
			CopyFailoverRetryAfter(c, failure.Headers)
		}
		if outcome != "" {
			// These statuses bypass presentation rules and generic stream framing.
			// Gin records render/write errors on the active context synchronously.
			c.Writer.Header().Del("Content-Length")
			c.Header("Content-Type", "application/json")
			priorErrors := len(c.Errors)
			c.JSON(failure.Status, gin.H{"error": gin.H{"type": kind, "message": message}})
			if len(c.Errors) != priorErrors {
				outcome = responsecompat.OutcomeClientGone
			}
			recordResponsesCohortDelivery(c, outcome)
			return
		}
		started = false
	}
	if !failure.ResponsesCompatibility && compatibilityCohortFrom(c, responsesCohortKey) != nil {
		original := write
		write = func(c *gin.Context, status int, kind, message string, started bool) {
			before := len(c.Errors)
			original(c, status, kind, message, started)
			if started || !c.Writer.Written() || c.Writer.Status() != status || c.Writer.Size() <= 0 {
				return
			}
			if len(c.Errors) != before {
				recordResponsesCohortDelivery(c, responsecompat.OutcomeClientGone)
				return
			}
			// Presentation rules can preserve or change the upstream status.
			// Classify the delivered result using both that status and evidence
			// from this final attempt, never from an earlier recovered candidate.
			if status == http.StatusGatewayTimeout && failure.Status == http.StatusGatewayTimeout {
				recordResponsesCohortDelivery(c, responsecompat.OutcomeTimeout504)
			}
			if status == http.StatusServiceUnavailable && (failure.CapacityShed ||
				upstreamopenai.IsOpenAIRequestScopedCapacityShed(failure.UpstreamMessage, failure.Body) ||
				gjson.GetBytes(failure.Body, "error.type").String() == "overloaded_error") {
				recordResponsesCohortDelivery(c, responsecompat.OutcomeOverload503)
			}
		}
	}
	if failure.TooLarge {
		hooks.Upstream(c, http.StatusRequestEntityTooLarge, failure.TooLargeMessage)
		write(c, http.StatusRequestEntityTooLarge, "invalid_request_error", failure.TooLargeMessage, started)
		return
	}
	if failure.ContinuationUnsupported {
		message := strings.TrimSpace(failure.ClientMessage)
		if message == "" {
			message = "previous_response_id requires an OpenAI API-key provider for HTTP requests"
		}
		write(c, http.StatusBadRequest, "invalid_request_error", message, started)
		return
	}
	if !failure.ResponsesCompatibility {
		CopyFailoverRetryAfter(c, failure.Headers)
	}
	if failure.Credential {
		write(c, failure.CredentialStatus, "upstream_error", failure.CredentialMessage, started)
		return
	}
	if failure.CapacityShed && strings.TrimSpace(failure.ClientMessage) != "" {
		status := failure.ClientStatus
		if status <= 0 {
			status = http.StatusServiceUnavailable
		}
		write(c, status, "server_error", failure.ClientMessage, started)
		return
	}
	if failure.SilentRefusal {
		hooks.Upstream(c, failure.Status, failure.SilentMessage)
		write(c, http.StatusBadGateway, "upstream_error", failure.SilentMessage, started)
		return
	}
	if failure.CyberWarning {
		hooks.Upstream(c, failure.Status, failure.CyberMessage)
		status := failure.Status
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		write(c, status, "invalid_request_error", failure.CyberMessage, started)
		return
	}
	if rules != nil && len(failure.Body) > 0 {
		if rule := rules.MatchRule("openai", failure.Status, failure.Body); rule != nil {
			status := failure.Status
			if !rule.PassthroughCode && rule.ResponseCode != nil {
				status = *rule.ResponseCode
			}
			message := failure.UpstreamMessage
			if !rule.PassthroughBody && rule.CustomMessage != nil {
				message = *rule.CustomMessage
			}
			if rule.SkipMonitoring {
				hooks.SkipMonitoring(c)
			}
			write(c, status, "upstream_error", message, started)
			return
		}
	}
	hooks.Upstream(c, failure.Status, failure.UpstreamMessage)
	status, kind, message := MapOpenAIUpstreamError(failure.Status)
	write(c, status, kind, message, started)
}

// responsesCompatibilityErrorOutcome projects the reader's classified failures
// into client-visible rollout outcomes. Other statuses follow the existing
// presentation rules and stay classified by the final HTTP response.
func responsesCompatibilityErrorOutcome(status int) string {
	switch status {
	case http.StatusServiceUnavailable:
		return responsecompat.OutcomeOverload503
	case http.StatusGatewayTimeout:
		return responsecompat.OutcomeTimeout504
	case http.StatusBadGateway:
		return responsecompat.OutcomeProtocol502
	default:
		return ""
	}
}

// CopyFailoverRetryAfter relays a trimmed, bounded upstream Retry-After value
// into the live HTTP response. Missing context or headers skip relay; overlong
// values, CR/LF and values rejected by IsSafeRetryAfter are ignored, leaving
// any current destination value unchanged.
func CopyFailoverRetryAfter(c *gin.Context, headers http.Header) {
	if c == nil || headers == nil {
		return
	}
	retryAfter := strings.TrimSpace(headers.Get("Retry-After"))
	// Bound header bytes and reject line breaks before parsing; only admitted
	// values are installed so retry metadata cannot inject another HTTP header line.
	if retryAfter == "" || len(retryAfter) > 128 || strings.ContainsAny(retryAfter, "\r\n") || !IsSafeRetryAfter(retryAfter) {
		return
	}
	c.Header("Retry-After", retryAfter)
}

// IsSafeRetryAfter accepts decimal delays up to seven days or HTTP dates no
// later than seven days from now. Zero seconds and past dates are accepted;
// numeric input uses uint32 parsing, and callers trim and reject header line
// breaks separately.
func IsSafeRetryAfter(value string) bool {
	digitsOnly := true
	for _, char := range value {
		if char < '0' || char > '9' {
			digitsOnly = false
			break
		}
	}
	if digitsOnly {
		seconds, err := strconv.ParseUint(value, 10, 32)
		// Bound decimal retry delays to the one-week relay window.
		return err == nil && seconds <= uint64((7*24*time.Hour)/time.Second)
	}
	retryAt, err := http.ParseTime(value)
	if err != nil {
		return false
	}
	// Date values use the same one-week upper relay bound.
	return !retryAt.After(time.Now().Add(7 * 24 * time.Hour))
}

// MapOpenAIUpstreamError maps upstream status to the default final client
// failure. Authentication and access failures become gateway errors, 429
// retains rate-limit status, 529 becomes unavailable, and unspecified statuses
// use generic 502 presentation.
func MapOpenAIUpstreamError(statusCode int) (int, string, string) {
	switch statusCode {
	case 401:
		return http.StatusBadGateway, "upstream_error", "Upstream authentication failed, please contact administrator"
	case 403:
		return http.StatusBadGateway, "upstream_error", "Upstream access forbidden, please contact administrator"
	case 429:
		return http.StatusTooManyRequests, "rate_limit_error", "Upstream rate limit exceeded, please retry later"
	case 529:
		return http.StatusServiceUnavailable, "upstream_error", "Upstream service overloaded, please retry later"
	case 500, 502, 503, 504:
		return http.StatusBadGateway, "upstream_error", "Upstream service temporarily unavailable"
	default:
		return http.StatusBadGateway, "upstream_error", "Upstream request failed"
	}
}
