package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"

	wire "github.com/TokenFlux/TokenRouter/internal/protocol/openai"
	"github.com/TokenFlux/TokenRouter/internal/protocol/wirejson"
)

// response_compatibility_resolution.go resolves one ordered Responses exchange.
// A semantic terminal records a candidate outcome. Later frames can corroborate
// that candidate, add sparse fields, or establish a conflict. A single bound
// Response ID and independent output positions prevent data from different
// responses from being assembled together.
//
// Replay, output, tier, and accounting rely on different evidence. The reader
// publishes the replay lock as soon as a frame demonstrates work, but holds
// client output until EOF. Metering from nonterminal frames stays separate so
// a rejected terminal cannot contribute the usage being used to corroborate it.
// Identity and accounting are published after frame validation, so a rejected
// tail cannot advance the counters or establish a previously missing identity.

// responsesStreamResolver collects ordered evidence under one response identity.
type responsesStreamResolver struct {
	attempt    *ResponsesStreamAttempt          // executor-visible attempt state
	snapshot   ResponsesStreamSnapshot          // ordered observations for publication
	normalizer wire.ResponseLifecycleNormalizer // existing lifecycle envelope repair
	output     responsesOutput                  // reconstructed output positions
	problem    error                            // first protocol violation
	mixedID    bool                             // attribution beyond explicit IDs is unsafe

	// terminal, terminalEvent, and terminalPayload store a candidate until EOF.
	// A retryable leading bare error stays provisional until a lifecycle event
	// confirms an outcome or EOF leaves the bare error as the sole terminal.
	terminal           ResponsesStreamResolution // candidate verdict before EOF
	terminalEvent      string                    // normalized semantic event name
	terminalPayload    []byte                    // evidence for failure classification
	terminalBody       responsesObject           // merged final response metadata
	terminalProof      bool                      // affirmative failure-before-execution evidence
	provisionalFailure []byte                    // retryable leading bare error

	// explicitUsage records nonterminal usage carrying the bound ID. Usage
	// without an ID can corroborate it only while attribution stays unambiguous.
	independentUsage wire.ResponsesMetering         // nonterminal cumulative usage
	explicitUsage    wire.ResponsesMetering         // ID-matched nonterminal usage
	terminalUsage    wire.ResponsesMetering         // authoritative terminal usage
	independentTier  string                         // observed execution tier outside terminal
	explicitTier     string                         // nonterminal tier explicitly bound by ID
	terminalTier     string                         // actual reported terminal tier
	tierConflict     bool                           // incompatible independent tier fields
	explicitConflict bool                           // incompatible explicitly bound tier fields
	finishedImages   *wire.OpenAIImageOutputCounter // completed images outside terminal
	explicitImages   *wire.OpenAIImageOutputCounter // completed images with an explicit response ID
	countSearch      func([]byte) int               // configured search item classifier
	finishedSearches map[int]bool                   // completed search positions with explicit-ID presence
}

// newResponsesStreamResolver initializes isolated output and accounting for one attempt.
func newResponsesStreamResolver(attempt *ResponsesStreamAttempt) *responsesStreamResolver {
	return &responsesStreamResolver{
		attempt: attempt,
		snapshot: ResponsesStreamSnapshot{
			Usage:            ResponsesUsageEvidence{State: ResponsesUsageMissing},
			IndependentUsage: ResponsesUsageEvidence{State: ResponsesUsageMissing},
		},
		output: newResponsesOutput(), finishedImages: wire.NewOpenAIImageOutputCounter(),
		explicitImages: wire.NewOpenAIImageOutputCounter(),
	}
}

// observe validates one frame or complete JSON object and publishes any new
// semantic evidence before the next physical read. Validate the response
// identity before reading the frame's evidence and bind it only when the
// frame passes validation. This prevents a rejected frame from supplying
// the ID that would make earlier anonymous observations attributable.
// Valid independent observations can still arrive after a terminal that
// will ultimately be rejected.
func (r *responsesStreamResolver) observe(label string, data []byte) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		if label != "" && !upstreamHeartbeatLabel(label) {
			r.reject(errors.New("upstream Responses event is missing its JSON payload"))
		}
		return
	}
	if upstreamHeartbeatLabel(label) && data[0] != '{' && !gjson.ValidBytes(data) {
		return
	}
	defer func() { r.attempt.publish(r.snapshot) }()
	if err := wirejson.ValidateRequestObject(data); err != nil {
		r.reject(errors.New("upstream Responses frame is not an unambiguous JSON object"))
		return
	}
	var fields responsesObject
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&fields); err != nil {
		r.reject(err)
		return
	}
	event, response, err := responsesFrameEvent(label, fields)
	if err != nil {
		r.reject(err)
		return
	}
	if upstreamHeartbeatLabel(event) {
		return
	}
	id, err := r.validateResponseID(fields, response)
	if err != nil {
		r.reject(err)
		return
	}
	// Read the original usage views before lifecycle repair so normalization
	// cannot change which counter fields are compared within the same frame.
	usage := wire.ParseResponsesMetering(data)
	data, _, _ = r.normalizer.Normalize(data, event)
	// Contradictory positive evidence cannot authorize settlement or prove
	// that a fresh attempt would be safe to send.
	if observed := responsesUsageEvidence(usage, ResponsesUsageIndependent); observed.State == ResponsesUsageReported || usage.Conflicting() {
		r.snapshot.ReplayBlocked = true
	}
	if err := r.observeFrame(event, fields, response, data, id != "", usage); err != nil {
		r.reject(err)
		return
	}
	if id != "" {
		r.snapshot.ResponseID = id
	}
	r.snapshot.IndependentUsage = responsesUsageEvidence(r.trustedIndependentUsage(), ResponsesUsageIndependent)
	r.snapshot.Usage, _ = r.selectedUsage(r.problem != nil)
	r.snapshot.ServiceTier = r.trustedTier(r.problem != nil)
}

// observeFrame separates terminal authority from progressive accounting and
// records execution evidence before processing a potentially invalid tail.
// Lifecycle preambles can echo a requested service tier, so the attempt only
// treats a tier as actual outside those echoes. Independent usage and completed
// work become eligible for settlement after the frame passes its lifecycle,
// output, and tier checks. A later valid usage-only event can supply evidence
// even when the request has already encountered a protocol violation.
func (r *responsesStreamResolver) observeFrame(event string, fields, response responsesObject, data []byte, explicit bool, usage wire.ResponsesMetering) error {
	if event == "error" {
		_, started := responsesExecutionProof(data)
		r.snapshot.ReplayBlocked = r.snapshot.ReplayBlocked || started
	}
	if event == "error" && r.terminal == "" && !r.snapshot.ReplayBlocked &&
		OpenAIStreamErrorEventShouldFailover(data, ExtractOpenAISSEErrorMessage(data)) {
		if r.provisionalFailure != nil {
			if err := r.mergeFailureEvidence(r.provisionalFailure, data); err != nil {
				return err
			}
		}
		r.provisionalFailure = data
		return nil
	}
	resolution := responsesEventResolution(event)
	if resolution != "" {
		r.snapshot.FirstOutput = true
		if resolution != ResponsesStreamFailed || responsesFrameWorked(data, event) {
			r.snapshot.ReplayBlocked = true
		}
		return r.observeTerminal(event, resolution, fields, response, data, usage)
	}
	worked := responsesFrameWorked(data, event)
	if worked {
		r.snapshot.FirstOutput = true
		r.snapshot.ReplayBlocked = true
	}
	preamble := OpenAIStreamEventIsPreamble(event)
	kind, _, _, _ := responsesOutputEvent(event)
	if r.terminal != "" && (kind != "" || worked || preamble) {
		return errors.New("upstream Responses emitted generation events after its terminal")
	}
	if err := r.output.observe(event, fields); err != nil {
		return err
	}
	image, err := responsesCompletedImage(event, data)
	if err != nil {
		return err
	}
	// Check the frame before it can add counters, images, searches, or a
	// processing tier to previously verified settlement evidence.
	if !preamble && usage.HasValues() && !usage.Invalid() {
		if err := r.observeIndependentTier(fields, response, explicit); err != nil {
			return err
		}
	}
	r.observeIndependentUsage(usage, explicit)
	r.observeCompletedSearch(event, data, explicit)
	if image.Exists() {
		r.finishedImages.AddCompletedImage(image)
		if explicit {
			r.explicitImages.AddCompletedImage(image)
		}
	}
	return nil
}

// observeCompletedSearch records only completed search items at validated
// output positions. Repeated frames fill the same position, and a mixed-ID
// exchange can retain only evidence explicitly bound to the response.
func (r *responsesStreamResolver) observeCompletedSearch(event string, data []byte, explicit bool) {
	if r.countSearch == nil || event != "response.output_item.done" {
		return
	}
	item := gjson.GetBytes(data, "item")
	if item.Get("status").String() != "completed" || r.countSearch([]byte(`{"output":[`+item.Raw+`]}`)) == 0 {
		return
	}
	if r.finishedSearches == nil {
		r.finishedSearches = make(map[int]bool)
	}
	index := int(gjson.GetBytes(data, "output_index").Int())
	r.finishedSearches[index] = r.finishedSearches[index] || explicit
}

// responsesCompletedImage returns a completed image candidate after validating
// its output format. Other events return an empty candidate, including image
// previews that can demonstrate work without providing billable image output.
func responsesCompletedImage(event string, data []byte) (gjson.Result, error) {
	if event != "response.output_item.done" && event != "response.image_generation_call.completed" {
		return gjson.Result{}, nil
	}
	item := gjson.GetBytes(data, "item")
	if event == "response.output_item.done" && item.Get("type").String() != "image_generation_call" {
		return gjson.Result{}, nil
	}
	if !item.Exists() {
		item = gjson.ParseBytes(data)
	}
	if err := wire.ValidateGIFImageOutputs(data); err != nil {
		return gjson.Result{}, err
	}
	return item, nil
}

// observeTerminal accumulates compatible terminal views without committing
// success. A matching pair can fill omissions in response metadata and output.
// Independent state remains separate even when contradictions invalidate this
// terminal and the reader continues to later frames.
func (r *responsesStreamResolver) observeTerminal(event string, resolution ResponsesStreamResolution, fields, response responsesObject, data []byte, usage wire.ResponsesMetering) error {
	previous := r.terminal
	if previous != "" && previous != resolution {
		return errors.New("upstream Responses has contradictory terminal outcomes")
	}
	if previous == ResponsesStreamFailed {
		if event != r.terminalEvent && event != "error" && r.terminalEvent != "error" {
			return errors.New("upstream Responses has conflicting failure events")
		}
		if err := r.mergeFailureEvidence(r.terminalPayload, data); err != nil {
			return err
		}
	}
	if previous == "" || (r.terminalEvent == "error" && event != "error") {
		r.terminal, r.terminalEvent, r.terminalPayload = resolution, event, data
	}
	r.provisionalFailure = nil
	// Later terminals may omit the proof, but an explicit contradictory
	// execution marker in a later response must close any replay window.
	proof, started := responsesExecutionProof(data)
	r.terminalProof = r.terminalProof || proof
	r.snapshot.ReplayBlocked = r.snapshot.ReplayBlocked || started
	if err := r.terminalUsage.Merge(usage, true); err != nil {
		return err
	}
	tier, err := responsesFrameTier(fields, response)
	if err != nil {
		return err
	}
	if err := mergeResponsesTier(&r.terminalTier, tier); err != nil {
		return err
	}
	if r.independentTier != "" && tier != "" && r.independentTier != tier {
		return errors.New("upstream Responses terminal tier conflicts with an independent report")
	}
	if response == nil {
		return nil
	}
	if err := r.output.observeArray(response["output"]); err != nil {
		return err
	}
	// Output, metering, and tier have dedicated comparison rules. Preserve
	// the other final fields after validating equality wherever they overlap.
	delete(response, "output")
	delete(response, "service_tier")
	delete(response, "status")
	if topUsage := fields["usage"]; response["usage"] == nil && topUsage != nil {
		response["usage"] = topUsage
	}
	if r.terminalBody == nil {
		r.terminalBody = response
		return nil
	}
	for _, field := range []string{"usage", "tool_usage"} {
		if r.terminalBody[field] == nil && response[field] != nil {
			r.terminalBody[field] = response[field]
		}
		delete(response, field)
	}
	return mergeResponsesObject(r.terminalBody, response)
}

// validateResponseID checks the response-scoped IDs before the frame can
// contribute output or usage. It returns the candidate ID for the caller to
// bind after validation. On disagreement only explicitly bound observations
// remain attributable. The output recorder handles item and call IDs.
func (r *responsesStreamResolver) validateResponseID(fields, response responsesObject) (string, error) {
	id, err := responsesString(response, "id")
	if err != nil {
		r.mixedID = true
		return "", err
	}
	outer, err := responsesString(fields, "response_id")
	if err != nil {
		r.mixedID = true
		return "", err
	}
	if id != "" && outer != "" && id != outer {
		r.mixedID = true
		return "", errors.New("upstream Responses frame has conflicting response identities")
	}
	value := firstResponsesString(id, outer)
	if value != "" && (strings.TrimSpace(value) != value || (r.snapshot.ResponseID != "" && r.snapshot.ResponseID != value)) {
		r.mixedID = true
		return "", errors.New("upstream Responses stream has inconsistent response identities")
	}
	return value, nil
}

// observeIndependentUsage retains progressive counters and separately
// corroborates usage with an explicit matching Response ID. A contradictory
// observation makes its progressive source unverified. A valid terminal can
// still establish accounting with its own counters.
func (r *responsesStreamResolver) observeIndependentUsage(usage wire.ResponsesMetering, explicit bool) {
	// Merge retains disagreement as unverified evidence for the source. A
	// progressive accounting disagreement alone does not reject the response.
	if explicit {
		_ = r.explicitUsage.Merge(usage, false)
	}
	_ = r.independentUsage.Merge(usage, false)
}

// observeIndependentTier checks only execution-time nonterminal tier evidence.
// When an explicit ID corroborates the tier, retain that source separately
// from usage frames that are attributed by their position in the attempt.
// A malformed report or a mismatch against a still viable terminal leaves
// the already recorded independent tier intact. Disagreement among the
// independent tier reports makes that source unverified.
func (r *responsesStreamResolver) observeIndependentTier(fields, response responsesObject, explicit bool) error {
	tier, err := responsesFrameTier(fields, response)
	if err != nil {
		return err
	}
	if r.problem == nil && tier != "" && r.terminalTier != "" && tier != r.terminalTier {
		return errors.New("upstream Responses terminal tier conflicts with an independent report")
	}
	if err := mergeResponsesTier(&r.independentTier, tier); err != nil {
		r.tierConflict = true
		r.explicitConflict = r.explicitConflict || (explicit && r.explicitTier != "" && r.explicitTier != tier)
		return err
	}
	if explicit && tier != "" {
		r.explicitTier = tier
	}
	return nil
}

// selectedUsage chooses the usable counters and their source for this outcome.
// A rejected terminal cannot supply fields to the independent snapshot.
func (r *responsesStreamResolver) selectedUsage(rejectTerminal bool) (ResponsesUsageEvidence, wire.ResponsesMetering) {
	independent := r.trustedIndependentUsage()
	verified := responsesUsageEvidence(independent, ResponsesUsageIndependent)
	if rejectTerminal || r.terminalUsage.Invalid() {
		// Independent counters cannot establish the charge if the same source
		// reported inconsistent rates and the terminal cannot resolve them.
		if verified.State == ResponsesUsageReported && ((!r.mixedID && r.tierConflict) || (r.mixedID && r.explicitConflict)) {
			return ResponsesUsageEvidence{State: ResponsesUsageUnverified, Source: ResponsesUsageIndependent}, wire.ResponsesMetering{}
		}
		if (verified.State == ResponsesUsageReported || verified.State == ResponsesUsageZero) ||
			(!r.terminalUsage.HasValues() && !r.terminalUsage.Invalid()) {
			return verified, independent
		}
		return ResponsesUsageEvidence{State: ResponsesUsageUnverified, Source: ResponsesUsageTerminal}, wire.ResponsesMetering{}
	}
	selected, usedIndependent, usedTerminal := wire.OverlayResponsesMetering(independent, r.terminalUsage)
	source := ResponsesUsageTerminal
	switch {
	case !usedTerminal:
		source = ResponsesUsageIndependent
	case usedIndependent:
		source = ResponsesUsageCombined
	}
	return responsesUsageEvidence(selected, source), selected
}

// trustedIndependentUsage selects observations attributable to the bound Response ID.
func (r *responsesStreamResolver) trustedIndependentUsage() wire.ResponsesMetering {
	if r.snapshot.ResponseID == "" {
		return wire.ResponsesMetering{}
	}
	if r.mixedID {
		return r.explicitUsage
	}
	return r.independentUsage
}

// reject retains the first protocol violation and immediately prevents replay.
// Frame parsing may continue, allowing matching independent usage to arrive.
func (r *responsesStreamResolver) reject(err error) {
	if r.problem == nil {
		r.problem = err
	}
	r.snapshot.ReplayBlocked = true
	r.snapshot.IndependentUsage = responsesUsageEvidence(r.trustedIndependentUsage(), ResponsesUsageIndependent)
	r.snapshot.Usage, _ = r.selectedUsage(true)
	r.snapshot.ServiceTier = r.trustedTier(true)
	r.attempt.publish(r.snapshot)
}

// resolve publishes the only final decision after EOF or a read failure.
// Output reconstruction and usable-response checks run before any result is
// returned to the delivery adapter. Partial observations remain available
// when the resolver returns a classified failure.
func (r *responsesStreamResolver) resolve(cleanEOF bool, readErr error, httpStatus int) ([]byte, *NonStreamingResult, error) {
	r.snapshot.CleanEOF = cleanEOF
	if readErr == nil && r.terminal == "" && r.provisionalFailure != nil {
		payload := r.provisionalFailure
		proof, started := responsesExecutionProof(payload)
		r.terminal, r.terminalEvent, r.terminalPayload = ResponsesStreamFailed, "error", payload
		r.terminalProof = proof
		r.snapshot.FirstOutput = true
		r.snapshot.ReplayBlocked = r.snapshot.ReplayBlocked || started
		_ = r.terminalUsage.Merge(wire.ParseResponsesMetering(payload), true)
	}
	switch {
	case r.problem != nil:
		return r.finish(nil, responsesReadError(r.problem), true)
	case readErr != nil:
		failure := responsesReadError(readErr)
		if failure.StatusCode == http.StatusGatewayTimeout && r.terminal == ResponsesStreamFailed {
			if overload := r.failure(httpStatus); overload.StatusCode == http.StatusServiceUnavailable {
				overload.cause = readErr
				return r.finish(nil, overload, true)
			}
		}
		return r.finish(nil, failure, true)
	}
	body, validationErr := r.buildResponse()
	switch {
	case validationErr != nil:
		return r.finish(nil, responsesReadError(validationErr), true)
	case r.terminal == ResponsesStreamFailed:
		return r.finish(body, r.failure(httpStatus), false)
	case httpStatus < 200 || httpStatus >= 300:
		return r.finish(nil, responsesReadError(errors.New("upstream Responses HTTP status disagrees with its result")), true)
	default:
		return r.finish(body, nil, false)
	}
}

// buildResponse assembles the response object and checks the proposed result.
// Empty completions require real refusal evidence or effective output, and an
// incomplete response also needs an explicit reason from the upstream.
func (r *responsesStreamResolver) buildResponse() ([]byte, error) {
	if r.terminal == "" {
		return nil, errors.New("upstream Responses stream has no trustworthy terminal")
	}
	items, err := r.output.build()
	if err != nil {
		return nil, err
	}
	if r.terminalBody == nil && r.terminal == ResponsesStreamFailed {
		return nil, nil
	}
	if r.terminalBody == nil || (r.snapshot.ResponseID == "" && r.terminal != ResponsesStreamFailed) {
		return nil, errors.New("upstream Responses terminal has no bound response object")
	}
	r.terminalBody["id"] = r.snapshot.ResponseID
	r.terminalBody["status"] = string(r.terminal)
	r.terminalBody["output"] = items
	if tier := r.trustedTier(false); tier != "" {
		r.terminalBody["service_tier"] = tier
	}
	body, err := json.Marshal(r.terminalBody)
	if err != nil {
		return nil, err
	}
	if err := wire.ValidateGIFImageOutputs(body); err != nil {
		return nil, err
	}
	if r.terminal == ResponsesStreamFailed {
		return body, nil
	}
	if r.terminal == ResponsesStreamIncomplete && strings.TrimSpace(gjson.GetBytes(body, "incomplete_details.reason").Str) == "" {
		return nil, errors.New("upstream Responses incomplete terminal has no reason")
	}
	visible, refused := ResponsesUsefulOutput(body)
	if (!visible && !refused) || (r.terminal == ResponsesStreamRefused && !refused) {
		return nil, errors.New("upstream Responses terminal has no effective output")
	}
	if refused && !visible {
		r.terminal = ResponsesStreamRefused
	}
	_, selected := r.selectedUsage(false)
	return selected.Patch(body)
}

// finish publishes immutable evidence and builds the partial result used by
// forwarding and settlement. A protocol failure selects independent usage and
// image evidence, while a validated final object can supply missing images.
func (r *responsesStreamResolver) finish(body []byte, streamErr *ResponsesStreamError, rejectTerminal bool) ([]byte, *NonStreamingResult, error) {
	r.snapshot.Resolution = r.terminal
	if rejectTerminal {
		r.snapshot.Resolution = ResponsesStreamProtocolError
	} else if streamErr != nil && r.terminal == ResponsesStreamFailed {
		if hit, _, _ := DetectOpenAICyberPolicy(streamErr.Payload); hit {
			r.snapshot.Resolution = ResponsesStreamRefused
		}
	}
	r.snapshot.ReplayAllowed = r.snapshot.CleanEOF && !rejectTerminal &&
		r.terminal == ResponsesStreamFailed && r.terminalProof && !r.snapshot.ReplayBlocked
	r.snapshot.ReplayBlocked = r.snapshot.ReplayBlocked || !r.snapshot.ReplayAllowed
	r.snapshot.Usage, _ = r.selectedUsage(rejectTerminal)
	r.snapshot.IndependentUsage = responsesUsageEvidence(r.trustedIndependentUsage(), ResponsesUsageIndependent)
	r.snapshot.ServiceTier = r.trustedTier(rejectTerminal)
	r.attempt.publish(r.snapshot)

	images := r.finishedImages
	if r.mixedID {
		images = r.explicitImages
	}
	// A validated reconstruction already contains the completed items. Counting
	// that representation alone avoids double counting items whose IDs arrived late.
	if len(body) != 0 || r.snapshot.ResponseID == "" {
		images = wire.NewOpenAIImageOutputCounter()
	}
	images.AddJSONResponse(body)
	result := &NonStreamingResult{
		Served: streamErr == nil && len(body) != 0, ResponseID: r.snapshot.ResponseID,
		ImageCount: images.Count(), ImageFrameCount: images.FrameCount(), ImageOutputSizes: images.Sizes(),
	}
	if r.snapshot.ResponseID != "" {
		for _, explicit := range r.finishedSearches {
			if explicit || !r.mixedID {
				result.SearchCount++
			}
		}
	}
	if evidence := r.attempt.Snapshot().Usage; evidence.State == ResponsesUsageReported || evidence.State == ResponsesUsageZero {
		result.Usage = &evidence.Usage
	}
	if streamErr != nil {
		return nil, result, streamErr
	}
	return body, result, nil
}

// trustedTier selects a corroborated processing tier for the completed attempt.
func (r *responsesStreamResolver) trustedTier(rejectTerminal bool) string {
	if !rejectTerminal && r.terminalTier != "" {
		return r.terminalTier
	}
	if r.snapshot.ResponseID == "" {
		return ""
	}
	if r.mixedID {
		if !r.explicitConflict {
			return r.explicitTier
		}
		return ""
	}
	if !r.tierConflict {
		return r.independentTier
	}
	return ""
}

// failure translates the selected upstream failure using existing OpenAI status rules.
func (r *responsesStreamResolver) failure(httpStatus int) *ResponsesStreamError {
	payload := r.terminalPayload
	message := firstResponsesString(ExtractOpenAISSEErrorMessage(payload), "Upstream response failed")
	if hit, _, _ := DetectOpenAICyberPolicy(payload); hit {
		return &ResponsesStreamError{StatusCode: http.StatusBadRequest, EventType: r.terminalEvent, Payload: payload, Message: message}
	}
	status := OpenAIStreamFailureStatus(payload, message)
	if status == http.StatusBadGateway {
		status = OpenAIStreamFailedEventSemanticStatus(payload, message)
	}
	if status == http.StatusBadGateway && httpStatus >= 400 && httpStatus <= 599 {
		status = httpStatus
	}
	return &ResponsesStreamError{StatusCode: status, EventType: r.terminalEvent, Payload: payload, Message: message}
}

// mergeFailureEvidence checks whether a bare error and lifecycle failure agree.
func (r *responsesStreamResolver) mergeFailureEvidence(previous, next []byte) error {
	var oldInfo, newInfo responsesObject
	if err := json.Unmarshal([]byte(responsesErrorObject(previous).Raw), &oldInfo); err != nil {
		return nil
	}
	if err := json.Unmarshal([]byte(responsesErrorObject(next).Raw), &newInfo); err != nil {
		return nil
	}
	comparable := responsesObject{}
	for _, field := range []string{"code", "type", "message", "status_code", "execution_started"} {
		if field == "type" && (oldInfo[field] == "error" || newInfo[field] == "error") {
			continue
		}
		comparable[field] = oldInfo[field]
		if err := mergeResponsesObject(comparable, responsesObject{field: newInfo[field]}); err != nil {
			return err
		}
	}
	return nil
}

// responsesUsageEvidence maps protocol observations to the public accounting decision.
func responsesUsageEvidence(data wire.ResponsesMetering, source ResponsesUsageSource) ResponsesUsageEvidence {
	usage, seen, valid := data.Result()
	switch {
	case !seen:
		return ResponsesUsageEvidence{State: ResponsesUsageMissing}
	case !valid:
		return ResponsesUsageEvidence{State: ResponsesUsageUnverified, Source: source}
	case wire.OpenAIUsageHasTokens(&usage):
		return ResponsesUsageEvidence{State: ResponsesUsageReported, Source: source, Usage: usage}
	default:
		return ResponsesUsageEvidence{State: ResponsesUsageZero, Source: source, Usage: usage}
	}
}
