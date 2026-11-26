package requestpolicy

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// tool_limit.go handles the Responses built-in tool-call limit when a
// candidate cannot express it. Only an explicit positive integer with no
// applicable built-in tools may be omitted. Missing session information or
// a tool of unknown kind leaves the original constraint intact.

// UnusedToolLimit returns the original integer when neither client tools nor
// the fully prepared outbound tool set can consume the built-in tool budget.
// Unexpanded prompt templates and conversation references can contribute tools
// outside these two payloads, so both views must be self-contained.
func UnusedToolLimit(intent Intent, outgoing []byte, endpoint ...string) (int64, bool) {
	if intent.Mode != ModeStrict || intent.Endpoint != "/v1/responses" || !gjson.ValidBytes(outgoing) {
		return 0, false
	}
	original := gjson.Parse(intent.RawJSON())
	limit := original.Get("max_tool_calls")
	if limit.Type != gjson.Number {
		return 0, false
	}
	count, err := strconv.ParseInt(limit.Raw, 10, 64)
	if err != nil || count < 1 {
		return 0, false
	}
	prepared := gjson.ParseBytes(outgoing)
	messages := len(endpoint) > 0 && endpoint[0] == EndpointMessages
	if !containsOnlyClientTools(original, false) || !containsOnlyClientTools(prepared, messages) {
		return 0, false
	}
	return count, true
}

// containsOnlyClientTools accepts a documented local function/custom-tool
// declaration. Other kinds may run at the provider, including a namespace
// with uninspected members. A missing tool list is known empty only for a
// self-contained request without prior response, prompt or session state.
func containsOnlyClientTools(request gjson.Result, messages bool) bool {
	if !request.IsObject() {
		return false
	}
	for _, reference := range []string{"previous_response_id", "conversation", "conversation_id", "prompt", "session", "session_id"} {
		if value := request.Get(reference); value.Exists() && value.Type != gjson.Null {
			return false
		}
	}
	choice := request.Get("tool_choice")
	if choice.IsObject() {
		switch choice.Get("type").String() {
		case "function", "custom":
		case "any", "tool", "auto", "none":
			if !messages {
				return false
			}
		default:
			return false
		}
	} else if choice.Exists() && choice.Type != gjson.Null {
		switch choice.String() {
		case "none", "auto", "required":
		default:
			return false
		}
	}
	tools := request.Get("tools")
	if tools.Exists() && !clientToolArray(tools, messages) {
		return false
	}
	for _, item := range request.Get("input").Array() {
		if strings.TrimSpace(item.Get("type").String()) == "additional_tools" && !clientToolArray(item.Get("tools"), false) {
			return false
		}
	}
	return true
}

// clientToolArray checks each explicit declaration, including tool arrays
// carried inside Responses Lite input items before normalization or lifting.
func clientToolArray(tools gjson.Result, messages bool) bool {
	if !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Array() {
		switch tool.Get("type").String() {
		case "function", "custom":
		case "":
			if !messages || tool.Get("name").String() == "" || !tool.Get("input_schema").IsObject() {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// RestoreToolLimit restores a previously omitted limit on a Response object
// or an event carrying one. It leaves delta and error events untouched; the
// caller supplies the proven original value retained for this attempt.
func RestoreToolLimit(data []byte, limit int64) ([]byte, error) {
	if limit < 1 || !json.Valid(data) {
		return data, nil
	}
	root := gjson.ParseBytes(data)
	if root.Get("object").String() == "response" {
		return sjson.SetBytes(data, "max_tool_calls", limit)
	}
	if strings.HasPrefix(root.Get("type").String(), "response.") && root.Get("response").IsObject() {
		return sjson.SetBytes(data, "response.max_tool_calls", limit)
	}
	return data, nil
}
