package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	gatewayprovider "github.com/TokenFlux/TokenRouter/internal/gateway/provider"
	"github.com/TokenFlux/TokenRouter/internal/gateway/provider/openaiforward"
	"github.com/TokenFlux/TokenRouter/internal/gateway/responsecompat"
	"github.com/TokenFlux/TokenRouter/internal/infra/telemetry"
	"github.com/TokenFlux/TokenRouter/internal/protocol"
	"github.com/TokenFlux/TokenRouter/internal/upstream"
	"github.com/TokenFlux/TokenRouter/internal/upstream/openai"
)

// openai_responses_compatibility.go matches and rewrites finished native
// Responses requests for both HTTP builders. The builders resolve the URL,
// complete model and body preparation, and apply provider header overrides.
// Reading the finished request keeps the match tied to the actual exchange,
// even when routing or a local repair changes the outbound model.
//
// The first rollout decision belongs to the logical request and precedes
// aggregation and provider queue admission. After any wait, the current
// snapshot can withdraw permission for the prepared conversion. The original
// mapped HTTP request remains available until the last decision, keeping
// rollback independent of the conversion's body, replay policy, reader, and clocks.

const (
	// responsesCompatibilityAdmissionKey retains the first rollout arm and its identity across provider attempts.
	responsesCompatibilityAdmissionKey = "responses_compatibility_admission"
	// responsesRequestStartKey retains the monotonic HTTP entry timestamp across routing and retries.
	responsesRequestStartKey = "responses_compatibility_request_start"

	// These defaults keep converted exchanges bounded when the native stream clocks are disabled.
	responsesDefaultFirstOutput = 120 * time.Second
	responsesDefaultReadIdle    = 180 * time.Second
	responsesDefaultTotal       = 5 * time.Minute
	responsesSlotExpiryMargin   = 5 * time.Second
)

// responsesCompatibilityAdmission retains the initial rollout arm and prior conversion targets.
type responsesCompatibilityAdmission struct {
	tuple    responsecompat.Tuple    // first admitted tuple
	decision responsecompat.Decision // first arm and revision
	// converted tracks dispatched tuples whose later retries need synchronous
	// rollback headers even if the policy is already closed at retry preparation.
	converted map[responsecompat.Tuple]struct{}
}

// responsesCapacityKey identifies the request-local provider admission callbacks.
type responsesCapacityKey struct{}

// ResponsesProviderCapacity lets the send hook acquire the selected provider
// under its remaining budget and return a prefetched slot before queueing.
type ResponsesProviderCapacity struct {
	Acquire func(context.Context) error // acquires only if no slot is already held
	Yield   func()                      // returns an owned slot before blocking
}

// WithResponsesProviderCapacity carries the current provider's admission callbacks
// into wire preparation. The serial forwarding owner supplies both callbacks
// and releases any remaining provider slot when the selected attempt exits.
func WithResponsesProviderCapacity(ctx context.Context, capacity ResponsesProviderCapacity) context.Context {
	return context.WithValue(ctx, responsesCapacityKey{}, capacity)
}

// responsesCapacity returns the optional admission callbacks owned by the current attempt.
func responsesCapacity(ctx context.Context) ResponsesProviderCapacity {
	capacity, _ := ctx.Value(responsesCapacityKey{}).(ResponsesProviderCapacity)
	return capacity
}

// prepareCompatibilitySend supplies the common policy hook for the two native
// Responses HTTP dispatch paths. The hook admits the mapped tuple before a
// conversion or provider wait and uses the live client context to prevent a
// send if that wait ends in cancellation. The gateway records the initial arm even when
// the later snapshot withdraws permission. The final check records the latest
// evaluated revision in both arms. If waiting prevents that check, the latest
// queue or preparation evaluation remains recorded. Only a fully prepared wire
// request enters the hook so rollback preserves mapping and permitted repairs.
func (s *OpenAIResponsesExecutor) prepareCompatibilitySend(ctx context.Context, c *gin.Context, provider *gatewayprovider.ExecutionProvider, startedAt time.Time, effort *string) openaiforward.PrepareResponsesSend {
	capacity := responsesCapacity(ctx)
	waitForSend := func(waitCtx context.Context) error {
		if err := context.Cause(waitCtx); err != nil {
			return err
		}
		if capacity.Acquire != nil {
			if err := capacity.Acquire(waitCtx); err != nil {
				return err
			}
		}
		return context.Cause(waitCtx)
	}
	skipPolicy := func() openaiforward.PrepareResponsesSend {
		if capacity.Acquire == nil {
			return nil
		}
		return func(*http.Request) (*openai.ResponsesStreamAttempt, error) { return nil, waitForSend(ctx) }
	}
	if s.Compatibility == nil || !ResponsesMayDrain(c, provider) {
		return skipPolicy()
	}
	if upstreamProtocol := provider.Route.Protocol(); upstreamProtocol != "" && upstreamProtocol != protocol.ProtocolOpenAIResponses {
		return skipPolicy()
	}
	requestID, _ := c.Request.Context().Value(telemetry.ClientRequestID).(string)
	groupID := OpenAIResponseGroupID(c)
	if candidate, ok := provider.Route.Candidate(); ok {
		groupID = candidate.GroupID
	}
	if requestID == "" || groupID <= 0 {
		return skipPolicy()
	}
	return func(request *http.Request) (*openai.ResponsesStreamAttempt, error) {
		value, admitted := c.Get(responsesCompatibilityAdmissionKey)
		if (!admitted && !s.Compatibility.MayMatch()) || request.Method != http.MethodPost ||
			request.URL == nil || request.GetBody == nil || request.ContentLength <= 0 {
			return nil, waitForSend(ctx)
		}
		tuple := responsecompat.Tuple{
			EffectiveGroupID: groupID,
			ProviderID:       provider.Record.ID,
			UpstreamEndpoint: request.URL.EscapedPath(),
		}
		prepared, err := readPreparedResponsesBody(request)
		if err != nil {
			return nil, err
		}
		model := gjson.GetBytes(prepared, "model")
		if model.Type != gjson.String {
			return nil, waitForSend(ctx)
		}
		tuple.UpstreamModel = model.String()
		decision := s.Compatibility.Match(tuple, requestID)
		admission, _ := value.(*responsesCompatibilityAdmission)
		// An initial observation decision remains binding across candidate changes
		// even if a later rule or configuration would permit enforcement.
		if !admitted && decision.Mode != responsecompat.Off {
			admission = &responsesCompatibilityAdmission{tuple: tuple, decision: decision}
			c.Set(responsesCompatibilityAdmissionKey, admission)
			beginCompatibilityCohort(c, responsesCohortKey, s.Cohorts, responsecompat.Cohort{
				Phase: responsecompat.ResponsesPhase, RequestID: requestID, Revision: decision.Revision,
				Arm: decision.Mode, Tuple: tuple,
			})
		}
		if admission == nil {
			return nil, waitForSend(ctx)
		}
		cohort := compatibilityCohortFrom(c, responsesCohortKey)
		recordRevision := func(observed responsecompat.Decision) {
			if cohort != nil && observed.Mode != "" {
				cohort.finalRevision = observed.Revision
			}
		}
		recordRevision(decision)
		eligible := admission.decision.Mode == responsecompat.Enforce && decision.Mode == responsecompat.Enforce
		waitCtx := ctx
		var transport openai.ResponsesTransportOptions
		var release func()
		if eligible {
			transport = s.compatibilityTransport(c, startedAt, effort)
			// Use the same absolute deadline for both queues and transport. Snapshot
			// updates can wake the local wait but never renew its request budget.
			bounded, cancel := context.WithDeadlineCause(ctx, transport.Deadline, openai.ErrResponsesDeadline)
			defer cancel()
			waitCtx = bounded
			var observed responsecompat.Decision
			release, observed, err = s.Compatibility.AcquireConversion(waitCtx, tuple, requestID, capacity.Yield)
			recordRevision(observed)
			if err != nil {
				return nil, context.Cause(waitCtx)
			}
		}
		defer func() {
			// A revoked or rejected conversion cannot hand its local slot to a
			// reader. Return it here unless ownership passed to the HTTP exchange.
			if release != nil {
				release()
			}
		}()
		if release != nil {
			current := s.Compatibility.Match(tuple, requestID)
			recordRevision(current)
			if current.Mode != responsecompat.Enforce {
				// Revocation can race a slot grant. Leave the slot available while
				// the now-synchronous request waits for ordinary provider capacity.
				release()
				release = nil
			}
		}
		if err := waitForSend(waitCtx); err != nil {
			return nil, err
		}
		var streamBody []byte
		if release != nil {
			// Build conversion bytes separately so a revoked attempt retains the
			// candidate's original mapped body and its synchronous replay representation.
			streamBody, err = sjson.SetBytes(prepared, "stream", true)
			if err != nil {
				return nil, fmt.Errorf("set stream on prepared Responses body: %w", err)
			}
		}
		// Capacity grants can race cancellation and policy replacement. Keep
		// the last evaluation here, after both waits and body preparation.
		finalDecision := s.Compatibility.Match(tuple, requestID)
		recordRevision(finalDecision)
		if err := context.Cause(waitCtx); err != nil {
			return nil, err
		}
		if len(admission.converted) != 0 && c.Writer.Written() {
			// Local repair or outer failover already established replay eligibility.
			// Refuse the new send if downstream output committed during admission.
			return nil, fmt.Errorf("Responses output was committed before replay")
		}
		if admission.decision.Mode != responsecompat.Enforce {
			return nil, nil
		}
		if release != nil && finalDecision.Mode == responsecompat.Enforce {
			transport.ReleaseConversion = release
			release = nil
			observeDispatch := transport.OnDispatch
			transport.OnDispatch = func() {
				// Record the tuple only at handoff. A canceled or expired attempt
				// has no converted transport to associate with its later retries.
				if admission.converted == nil {
					admission.converted = make(map[responsecompat.Tuple]struct{})
				}
				admission.converted[tuple] = struct{}{}
				if cohort != nil {
					cohort.applied = true
				}
				if observeDispatch != nil {
					observeDispatch()
				}
			}
			convertResponsesSend(request, streamBody)
			return &openai.ResponsesStreamAttempt{Transport: transport}, nil
		}
		// A revoked conversion keeps the builder's mapped body and synchronous
		// reader. Earlier conversion targets also need JSON headers if the rule
		// was already off when the replacement was built.
		_, wasConverted := admission.converted[tuple]
		if eligible || admission.tuple == tuple || wasConverted {
			setResponsesAccept(request.Header, "application/json")
		}
		return nil, nil
	}
}

// readPreparedResponsesBody reads a separate copy of the mapped body for
// matching and conversion. The original reader remains available for a
// synchronous send, including after the provider queue revokes conversion.
func readPreparedResponsesBody(request *http.Request) ([]byte, error) {
	body, err := request.GetBody()
	if err != nil {
		return nil, fmt.Errorf("open prepared Responses body: %w", err)
	}
	defer func() { _ = body.Close() }()
	prepared := make([]byte, request.ContentLength)
	if _, err := io.ReadFull(body, prepared); err != nil {
		return nil, fmt.Errorf("read prepared Responses body: %w", err)
	}
	return prepared, nil
}

// ResponsesMayDrain reports whether a selected candidate can use the converted
// transport. Slot acquisition uses this before the final mapped model and
// URL exist. The final send hook still makes the exact rollout decision.
func ResponsesMayDrain(c *gin.Context, provider *gatewayprovider.ExecutionProvider) bool {
	return c != nil && c.GetBool(responsesForegroundJSONKey) && provider != nil && provider.Record.AllowsResponsesRequestRewrite()
}

// compatibilityTransport binds attempt and logical request clocks. Attempt
// time starts at provider preparation, while the total clock includes routing
// and remains fixed across provider changes. A short configured slot lifetime
// caps that total from the same entry time, which predates all slot acquisition
// paths. Headroom leaves time to close before Redis expires the slot, including
// the use of whole-second acquisition scores.
func (s *OpenAIResponsesExecutor) compatibilityTransport(c *gin.Context, startedAt time.Time, effort *string) openai.ResponsesTransportOptions {
	value, _ := c.Get(responsesRequestStartKey)
	requestStart, _ := value.(time.Time)
	if requestStart.IsZero() {
		requestStart = startedAt
	}
	policy := s.Output.Options.CompatibilityTransport
	policy.StartedAt = startedAt
	if policy.FirstOutputTimeout <= 0 {
		effortValue := ""
		if effort != nil {
			effortValue = *effort
		}
		policy.FirstOutputTimeout = s.Output.FirstOutputTimeout(effortValue)
		if policy.FirstOutputTimeout <= 0 {
			policy.FirstOutputTimeout = responsesDefaultFirstOutput
		}
	}
	if policy.StreamInterval <= 0 {
		policy.StreamInterval = s.Output.TextStreamInterval()
		if policy.StreamInterval <= 0 {
			policy.StreamInterval = responsesDefaultReadIdle
		}
	}
	total := s.Output.Options.CompatibilityTotalTimeout
	if total <= 0 {
		total = responsesDefaultTotal
	}
	if ttl := s.Output.Options.CompatibilityProviderSlotTTL; ttl > 0 {
		total = min(total, ttl-responsesSlotExpiryMargin)
	}
	policy.Deadline = requestStart.Add(total)
	return policy
}

// convertResponsesSend replaces the wire representation of a single request.
// Call after provider overrides and before transport so callers and providers
// cannot override the selected response media type. The rebuilt body and
// GetBody own the patched bytes, independent of any original candidate payload.
func convertResponsesSend(request *http.Request, streamBody []byte) {
	// Rebuild every HTTP body representation together so the transport advertises
	// the exact length of the converted bytes, including if the body is reopened.
	_ = request.Body.Close()
	request.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(streamBody)), nil
	}
	request.Body = io.NopCloser(bytes.NewReader(streamBody))
	request.ContentLength = int64(len(streamBody))
	// Remove even noncanonical header keys before setting the one representation
	// required for the converted POST. Redirects would escape the matched URL.
	setResponsesAccept(request.Header, "text/event-stream")
	for key := range request.Header {
		if strings.EqualFold(key, "Content-Type") || strings.EqualFold(key, "Content-Length") {
			delete(request.Header, key)
		}
	}
	request.Header.Set("Content-Type", "application/json")
	*request = *request.WithContext(upstream.WithHTTPUpstreamRedirectsDisabled(request.Context()))
}

// setResponsesAccept removes alternate header spellings so the selected wire media type is unique.
func setResponsesAccept(header http.Header, value string) {
	for key := range header {
		if strings.EqualFold(key, "Accept") {
			delete(header, key)
		}
	}
	header.Set("Accept", value)
}
