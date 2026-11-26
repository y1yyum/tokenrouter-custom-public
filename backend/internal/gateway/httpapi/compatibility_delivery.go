package httpapi

import (
	"net/http"
	"sync"

	"github.com/tidwall/gjson"

	"github.com/TokenFlux/TokenRouter/internal/gateway/requestpolicy"
	"github.com/TokenFlux/TokenRouter/internal/gateway/responsecompat"
	"github.com/TokenFlux/TokenRouter/internal/protocol/wirejson"
	"github.com/TokenFlux/TokenRouter/internal/upstream/openai"
)

// compatibility_delivery.go classifies D2 using data accepted by the client
// HTTP writer. Provider readers may observe a terminal during usage
// drain even though they can no longer deliver it. The writer's existing frame
// parser supplies this observer with complete events so the cohort never has
// to retain an entire generation or trust the provider's served flag.
//
// Messages and Responses deliver explicit terminal semantics. Chat can deliver
// refusal fragments and finish reasons on different events, so scalar evidence
// remains open across those events. Earlier delivered terminals survive later
// failures and disconnects. A disconnect before terminal delivery leaves the
// request unfinished for the client, regardless of upstream usage.

// parameterDelivery retains constant-size evidence for one client's output.
// The mutex serializes writer callbacks with request-exit snapshot reads. The
// original client endpoint is immutable and selects the wire contract after any
// upstream protocol conversion.
type parameterDelivery struct {
	endpoint string     // original client endpoint
	mu       sync.Mutex // protects delivery evidence below
	terminal string     // first definitive delivered outcome
	// The same two values carry a Chat refusal delta until finish or record a
	// Responses refusal delta alongside any delivered non-refusal text.
	refusal, text bool   // observed refusal fragment and visible Responses text
	chatFinish    string // most severe delivered Chat choice finish reason
	disconnected  bool   // downstream write short or rejected
}

// observe accepts one validated, successfully written SSE frame. It uses the
// client endpoint rather than the upstream adapter's protocol, so converted
// streams are classified from the same fields a native client can read. A
// delivered error can close a stream before Chat emits its sentinel, while an
// already finalized semantic outcome is retained when a late event arrives.
func (o *parameterDelivery) observe(data, event string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.terminal != "" || o.disconnected {
		return
	}
	if data == "[DONE]" {
		if o.endpoint == requestpolicy.EndpointChat {
			o.terminal = o.chatFinish
			if o.refusal {
				o.terminal = responsecompat.OutcomeRefused
			}
		}
		return
	}
	frame := gjson.Parse(data)
	kind := frame.Get("type").Str
	if kind == "" {
		kind = event
	}
	if kind == "error" || frame.Get("error").IsObject() {
		o.terminal = responsecompat.OutcomeOtherFailure
		return
	}
	switch o.endpoint {
	case requestpolicy.EndpointMessages:
		if kind == "message_delta" {
			o.terminal = parameterMessagesOutcome(frame.Get("delta.stop_reason").Str)
		}
	case requestpolicy.EndpointChat:
		o.observeChat(frame)
	case requestpolicy.EndpointResponses:
		o.observeResponses(kind, frame)
	}
}

// observeUnary borrows the complete accepted document from the unary writer.
// Its existing body limit applies; the SSE frame limit is only for fragments
// accumulated by the event decoder. No second response buffer is retained.
func (o *parameterDelivery) observeUnary(body []byte) {
	if !gjson.ValidBytes(body) {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.terminal != "" || o.disconnected {
		return
	}
	root := wirejson.ParseView(body)
	switch o.endpoint {
	case requestpolicy.EndpointResponses:
		o.terminal = responseJSONOutcome(body)
	case requestpolicy.EndpointMessages:
		if root.Get("type").Str == "message" {
			o.terminal = parameterMessagesOutcome(root.Get("stop_reason").Str)
		}
	case requestpolicy.EndpointChat:
		o.observeChat(root)
		o.terminal = o.chatFinish
		if o.refusal {
			o.terminal = responsecompat.OutcomeRefused
		}
	}
}

// parameterMessagesOutcome classifies an explicit Anthropic generation stop reason.
func parameterMessagesOutcome(reason string) string {
	switch reason {
	case "max_tokens":
		return responsecompat.OutcomeIncomplete
	case "refusal":
		return responsecompat.OutcomeRefused
	case "end_turn", "stop_sequence", "tool_use":
		return responsecompat.OutcomeCompleted
	default:
		return ""
	}
}

// observeChat joins refusal fragments with later choice terminals under the
// delivery lock. A request with multiple choices retains refusal ahead of
// truncation and truncation ahead of a normal finish until DONE or request exit.
func (o *parameterDelivery) observeChat(frame gjson.Result) {
	frame.Get("choices").ForEach(func(_, choice gjson.Result) bool {
		o.refusal = o.refusal || choice.Get("delta.refusal").Str != "" || choice.Get("message.refusal").Str != ""
		switch choice.Get("finish_reason").Str {
		case "content_filter":
			o.chatFinish = responsecompat.OutcomeRefused
		case "length":
			if o.chatFinish != responsecompat.OutcomeRefused {
				o.chatFinish = responsecompat.OutcomeIncomplete
			}
		case "stop", "tool_calls", "function_call":
			if o.chatFinish == "" {
				o.chatFinish = responsecompat.OutcomeCompleted
			}
		}
		return true
	})
}

// observeResponses classifies a Responses lifecycle frame under the delivery
// lock. The event and Response status cover current and legacy terminals.
// Delivered refusal deltas supply evidence when a completed object omits its
// output, and usable terminal output takes precedence over refusal fragments.
func (o *parameterDelivery) observeResponses(kind string, frame gjson.Result) {
	switch kind {
	case "response.refusal.delta", "response.refusal.done":
		o.refusal = o.refusal || frame.Get("delta").Str != "" || frame.Get("refusal").Str != ""
	case "response.output_text.delta", "response.output_text.done":
		o.text = o.text || frame.Get("delta").Str != "" || frame.Get("text").Str != ""
	case "response.failed", "response.cancelled", "response.canceled":
		o.terminal = responsecompat.OutcomeOtherFailure
	case "response.incomplete":
		o.terminal = responsecompat.OutcomeIncomplete
	case "response.completed", "response.done", "response.refused":
		response := frame.Get("response")
		if !response.IsObject() {
			response = frame
		}
		switch response.Get("status").Str {
		case "failed", "cancelled", "canceled":
			o.terminal = responsecompat.OutcomeOtherFailure
		case "incomplete":
			o.terminal = responsecompat.OutcomeIncomplete
		case "refused":
			o.terminal = responsecompat.OutcomeRefused
		case "completed", "":
			if kind == "response.refused" {
				o.terminal = responsecompat.OutcomeRefused
				return
			}
			if kind == "response.done" && response.Get("status").Str == "" {
				return
			}
			visible, refused := openai.ResponsesUsefulOutput([]byte(response.Raw))
			o.terminal = responsecompat.OutcomeCompleted
			if !visible && (refused || (o.refusal && !o.text)) {
				o.terminal = responsecompat.OutcomeRefused
			}
		}
	}
}

// failedWrite prevents later bytes from completing an undelivered SSE frame.
// Previously accepted semantic terminal evidence remains available at exit.
func (o *parameterDelivery) failedWrite() {
	o.mu.Lock()
	o.disconnected = true
	o.mu.Unlock()
}

// snapshot returns the accepted outcome and whether a write failed. An explicit
// Chat choice finish also counts when a provider omits its final DONE sentinel.
// A refusal fragment by itself is never promoted on EOF or disconnection.
func (o *parameterDelivery) snapshot() (outcome string, disconnected bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.terminal != "" {
		return o.terminal, o.disconnected
	}
	if o.chatFinish != "" && o.refusal {
		return responsecompat.OutcomeRefused, o.disconnected
	}
	return o.chatFinish, o.disconnected
}

// parameterFailureOutcome maps an accepted error's status to the D2 vocabulary.
// Codes without a dedicated category remain ordinary failures.
func parameterFailureOutcome(status int) string {
	switch status {
	case http.StatusBadRequest:
		return responsecompat.OutcomeParameter400
	case http.StatusServiceUnavailable:
		return responsecompat.OutcomeOverload503
	case http.StatusBadGateway:
		return responsecompat.OutcomeProtocol502
	case http.StatusGatewayTimeout:
		return responsecompat.OutcomeTimeout504
	default:
		return responsecompat.OutcomeOtherFailure
	}
}
