package openai

import (
	"errors"
	"slices"
	"strings"

	"github.com/tidwall/gjson"

	wire "github.com/TokenFlux/TokenRouter/internal/protocol/openai"
)

// response_compatibility_event.go interprets frame labels, execution evidence,
// and service tiers before the resolver can use them. The SSE event line and
// JSON body are independent observations. Comparing both before applying the
// existing lifecycle normalization preserves contradictions that ordinary
// streaming readers can resolve by taking the payload label.
//
// Execution evidence answers a different question from terminal success.
// Partial tool activity and visible content prevent replay immediately, while
// an ordinary error status leaves execution unknown. A replay candidate must
// carry an affirmative execution marker on a recognized error object.

// responsesFrameEvent resolves and cross-checks event name and response status.
// A legacy done needs an explicit terminal status even when another wire label
// looks usable. Bare Response objects and nested envelopes share this check.
func responsesFrameEvent(label string, fields responsesObject) (string, responsesObject, error) {
	payloadType, err := responsesString(fields, "type")
	if err != nil {
		return "", nil, err
	}
	response, hasResponse := fields["response"].(responsesObject)
	if fields["response"] != nil && !hasResponse {
		return "", nil, errors.New("upstream Responses event has a non-object response")
	}
	if !hasResponse && (fields["object"] == "response" || (fields["id"] != nil && fields["status"] != nil && payloadType == "")) {
		response = fields
	}
	status, err := responsesString(response, "status")
	if err != nil {
		return "", nil, err
	}
	if response != nil {
		object, err := responsesString(response, "object")
		if err != nil || (object != "" && object != "response") {
			return "", nil, errors.New("upstream Responses has an invalid response object marker")
		}
	}
	normalizedLabel, err := responsesCanonicalEvent(label, status)
	if err != nil {
		return "", nil, err
	}
	normalizedType, err := responsesCanonicalEvent(payloadType, status)
	if err != nil {
		return "", nil, err
	}
	if normalizedLabel != "" && normalizedType != "" && normalizedLabel != normalizedType {
		return "", nil, errors.New("upstream Responses SSE label disagrees with payload type")
	}
	event := firstResponsesString(normalizedType, normalizedLabel)
	if event == "" && response != nil {
		event = responsesEventFromStatus(status)
	}
	if event == "" && fields["error"] != nil {
		event = "error"
	}
	if event == "" {
		if fields["usage"] != nil || (response != nil && response["usage"] != nil) {
			event = "response.usage"
		} else {
			return "", nil, errors.New("upstream Responses payload has no recognizable event or status")
		}
	}
	if event == "error" && status != "" && responsesEventResolution(responsesEventFromStatus(status)) != ResponsesStreamFailed {
		return "", nil, errors.New("upstream Responses error event disagrees with response status")
	}
	resolution := responsesEventResolution(event)
	if resolution != "" && resolution != ResponsesStreamFailed && response["error"] != nil {
		return "", nil, errors.New("upstream Responses successful terminal carries an error object")
	}
	if resolution == ResponsesStreamCompleted && response["incomplete_details"] != nil {
		return "", nil, errors.New("upstream Responses completed terminal carries incomplete details")
	}
	return event, response, nil
}

// responsesCanonicalEvent compares a lifecycle label with the explicit status.
// The older cancellation spelling shares a terminal outcome. Non-lifecycle
// event names pass through for output or extension handling.
func responsesCanonicalEvent(event, status string) (string, error) {
	event = strings.TrimSpace(event)
	statusEvent := responsesEventFromStatus(status)
	switch event {
	case "response.done":
		if responsesEventResolution(statusEvent) == "" {
			return "", errors.New("upstream Responses done event has no explicit terminal status")
		}
		return statusEvent, nil
	case "response.canceled":
		event = "response.cancelled"
	}
	if event == "response.created" && status == "in_progress" {
		return event, nil
	}
	if status != "" && (responsesEventResolution(event) != "" || OpenAIStreamEventIsPreamble(event)) && statusEvent != event {
		return "", errors.New("upstream Responses event disagrees with response status")
	}
	return event, nil
}

// responsesEventFromStatus supplies a lifecycle label for a bare response status.
func responsesEventFromStatus(status string) string {
	switch status {
	case "completed", "incomplete", "failed", "refused":
		return "response." + status
	case "cancelled", "canceled":
		return "response.cancelled"
	case "queued":
		return "response.created"
	case "in_progress":
		return "response.in_progress"
	default:
		return ""
	}
}

// responsesEventResolution maps authoritative lifecycle events to terminal outcomes.
func responsesEventResolution(event string) ResponsesStreamResolution {
	switch event {
	case "response.completed":
		return ResponsesStreamCompleted
	case "response.incomplete":
		return ResponsesStreamIncomplete
	case "response.refused":
		return ResponsesStreamRefused
	case "response.failed", "response.cancelled", "error":
		return ResponsesStreamFailed
	default:
		return ""
	}
}

// responsesFrameTier validates the supplied actual tier fields and their agreement.
func responsesFrameTier(fields, response responsesObject) (string, error) {
	var combined string
	for _, source := range []responsesObject{fields, response} {
		value, err := responsesString(source, "service_tier")
		if err != nil {
			return "", err
		}
		if value == "" {
			continue
		}
		tier := wire.ServiceTierValue(value)
		if tier == "" {
			return "", errors.New("upstream Responses reported an unknown processing tier")
		}
		if err := mergeResponsesTier(&combined, tier); err != nil {
			return "", err
		}
	}
	return combined, nil
}

// mergeResponsesTier fills a missing tier or rejects conflicting populated labels.
func mergeResponsesTier(current *string, observed string) error {
	if observed == "" {
		return nil
	}
	if *current != "" && *current != observed {
		return errors.New("upstream Responses has conflicting processing tiers")
	}
	*current = observed
	return nil
}

// responsesFrameWorked recognizes visible content and tool execution before EOF.
// Lifecycle shells, empty parts and tool declarations do not start execution.
// Real argument fragments, refusal text and hosted tool progress do.
func responsesFrameWorked(data []byte, event string) bool {
	root := gjson.ParseBytes(data)
	if responsesHostedEventWorked(event) {
		return true
	}
	switch event {
	case "response.refusal.delta", "response.output_text.delta", "response.audio_transcript.delta",
		"response.function_call_arguments.delta", "response.custom_tool_call_input.delta", "response.tool_search_call_arguments.delta":
		return responsesNonemptyString(root.Get("delta"))
	case "response.refusal.done":
		return responsesNonemptyString(root.Get("refusal"))
	case "response.output_text.done":
		return responsesNonemptyString(root.Get("text"))
	case "response.audio_transcript.done":
		return responsesNonemptyString(root.Get("transcript"))
	case "response.function_call_arguments.done", "response.tool_search_call_arguments.done":
		return root.Get("arguments").IsObject() || responsesNonemptyString(root.Get("arguments"))
	case "response.custom_tool_call_input.done":
		return responsesNonemptyString(root.Get("input"))
	case "response.content_part.added", "response.content_part.done":
		return responsesPartWorked(root.Get("part"))
	case "response.output_item.added", "response.output_item.done":
		return responsesItemWorked(root.Get("item"))
	}
	for _, path := range []string{"response.output", "output"} {
		if slices.ContainsFunc(root.Get(path).Array(), responsesItemWorked) {
			return true
		}
	}
	return false
}

// responsesItemWorked checks positive evidence carried by one output item.
func responsesItemWorked(item gjson.Result) bool {
	if responsesHostedItemWorked(item) {
		return true
	}
	switch item.Get("type").String() {
	case "reasoning", "compaction":
		return false
	case "refusal":
		return responsesNonemptyString(item.Get("refusal"))
	case "function_call", "custom_tool_call", "tool_search_call":
		args := item.Get("arguments")
		return responsesNonemptyString(args) || args.IsObject() || responsesNonemptyString(item.Get("input"))
	case "message":
		return slices.ContainsFunc(item.Get("content").Array(), responsesPartWorked)
	default:
		return wire.StreamItemHasVisibleOutput(item) || item.Get("output").IsObject() || len(item.Get("output").Array()) > 0
	}
}

// responsesPartWorked recognizes visible data in one message part.
func responsesPartWorked(part gjson.Result) bool {
	for _, field := range []string{"text", "refusal", "transcript", "image_url", "data"} {
		if responsesNonemptyString(part.Get(field)) {
			return true
		}
	}
	return false
}

// responsesHostedEventWorked recognizes event names that report hosted tool activity.
func responsesHostedEventWorked(event string) bool {
	kind, phase, ok := strings.Cut(strings.TrimPrefix(event, "response."), ".")
	kind = strings.TrimSuffix(strings.TrimSuffix(kind, "_code"), "_arguments")
	if !ok || !responsesHostedTool(kind) {
		return false
	}
	switch phase {
	case "started", "in_progress", "searching", "interpreting", "executing", "generating", "completed", "failed", "partial_image", "delta", "done", "arguments.delta", "arguments.done", "code.delta", "code.done":
		return true
	default:
		return false
	}
}

// responsesHostedItemWorked recognizes actual progress on a hosted call item.
func responsesHostedItemWorked(item gjson.Result) bool {
	if !responsesHostedTool(item.Get("type").String()) || item.Get("execution").String() == "client" {
		return false
	}
	switch item.Get("status").String() {
	case "started", "in_progress", "searching", "interpreting", "executing", "generating", "completed", "failed":
		return true
	default:
		return false
	}
}

// responsesHostedTool identifies the built-in call families with execution phases.
func responsesHostedTool(kind string) bool {
	switch kind {
	case "web_search_call", "file_search_call", "code_interpreter_call", "image_generation_call",
		"mcp_call", "mcp_list_tools", "computer_call", "shell_call", "local_shell_call", "apply_patch_call", "tool_search_call":
		return true
	default:
		return false
	}
}

// responsesExecutionProof looks for an explicit execution_started boolean in
// structured error evidence. Any true marker closes replay even when another
// field says false. Missing and non-boolean markers do not establish rejection
// before execution.
func responsesExecutionProof(payload []byte) (rejected, started bool) {
	root := gjson.ParseBytes(payload)
	for _, path := range []string{"execution_started", "error.execution_started", "response.error.execution_started"} {
		if root.Get(path).Type == gjson.True {
			started = true
		}
	}
	return responsesPreExecutionCapacity(payload) && !started, started
}

// responsesErrorObject selects the structured error within a recognized envelope.
func responsesErrorObject(payload []byte) gjson.Result {
	root := gjson.ParseBytes(payload)
	for _, path := range []string{"response.error", "error"} {
		if value := root.Get(path); value.IsObject() {
			return value
		}
	}
	if root.Get("type").String() == "error" {
		return root
	}
	return gjson.Result{}
}
