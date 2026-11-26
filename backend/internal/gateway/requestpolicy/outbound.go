package requestpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// outbound.go checks the complete wire document against the frozen client
// controls. Candidate evidence establishes what a route supports. This check
// establishes what the selected encoder and its later edits actually produced.
// Cache declarations are checked together before the transport can send any
// bytes, including when the wire document is a proposed repair.
//
// Native routes compare cache directives at their original positions. The
// Messages bridge compares ordered text blocks so joining adjacent turns can
// preserve a breakpoint without using the same array indices. The caller can
// skip an incompatible route without exposing altered controls or starting a
// partial exchange.

// WireMismatch identifies a client field whose encoding did not survive preparation.
type WireMismatch struct {
	Path string // original client field path
}

// noRewriteKey marks a provider route with a byte passthrough contract.
type noRewriteKey struct{}

// Error names the client field without exposing either document.
func (e *WireMismatch) Error() string {
	return fmt.Sprintf("The upstream route cannot preserve %s", e.Path)
}

// IsWireMismatch distinguishes a local encoding refusal from a transport failure.
func IsWireMismatch(err error) bool {
	var mismatch *WireMismatch
	return errors.As(err, &mismatch)
}

// StrictIntent returns the frozen controls only for an enforcing request.
func StrictIntent(ctx context.Context) (Intent, bool) {
	intent, _ := FromContext(ctx)
	return intent, intent.Mode == ModeStrict
}

// WithoutRewrite allows validation on byte passthrough without field removal.
func WithoutRewrite(ctx context.Context) context.Context {
	return context.WithValue(ctx, noRewriteKey{}, true)
}

// RewriteDisabled reports whether the selected transport must retain its bytes.
func RewriteDisabled(ctx context.Context) bool {
	disabled, _ := ctx.Value(noRewriteKey{}).(bool)
	return disabled
}

// HasClientCache protects explicit cache declarations from legacy rewriting.
func HasClientCache(ctx context.Context) bool {
	intent, strict := StrictIntent(ctx)
	return strict && len(cacheRequirements(intent.Endpoint, gjson.Parse(intent.raw))) != 0
}

// CacheRepresentable rejects scopes the local cross-protocol encoder cannot
// carry, even if the remote endpoint accepts cache control at other locations.
func CacheRepresentable(intent Intent, req Requirement, outbound string) bool {
	if !strings.HasPrefix(req.Key, "cache_control.") || intent.Endpoint == outbound {
		return true
	}
	if intent.Endpoint != EndpointResponses || outbound != EndpointMessages {
		return false
	}
	block := gjson.Parse(req.block)
	cache := gjson.Parse(req.Value.Raw)
	typ, ttl := block.Get("type").String(), cache.Get("ttl")
	if strings.TrimSpace(block.Get("text").String()) == "" || cache.Get("type").String() != "ephemeral" {
		return false
	}
	if ttl.Exists() && ttl.String() != "5m" && ttl.String() != "1h" {
		return false
	}
	knownMembers := true
	cache.ForEach(func(key, _ gjson.Result) bool {
		knownMembers = key.Str == "type" || key.Str == "ttl"
		return knownMembers
	})
	if !knownMembers {
		return false
	}
	return req.role == "user" && (typ == "input_text" || typ == "text") ||
		req.role == "assistant" && (typ == "output_text" || typ == "text")
}

// PrepareOutbound checks the candidate's finished document as a unit. The only
// removal here is an unused tool limit, after all outgoing tools are visible.
// The original limit is returned separately for downstream Responses output.
func PrepareOutbound(intent Intent, endpoint string, body []byte, allowOmit bool) ([]byte, int64, error) {
	if intent.Mode != ModeStrict {
		return body, 0, nil
	}
	original, outgoing := gjson.Parse(intent.raw), gjson.ParseBytes(body)
	limit, omit := UnusedToolLimit(intent, body, endpoint)
	omit = omit && allowOmit
	if !omit {
		limit = 0
	}
	for _, req := range Requirements(intent) {
		var preserved bool
		switch req.Key {
		case "temperature":
			path := "temperature"
			if endpoint == "gemini" {
				path = "generationConfig.temperature"
				if outgoing.Get("request").IsObject() {
					path = "request." + path
				}
			}
			preserved = equalNumber(original.Get("temperature"), outgoing.Get(path))
		case "thinking.budget_tokens":
			preserved = endpoint == EndpointMessages &&
				equalNumber(original.Get(req.Path), outgoing.Get(req.Path)) &&
				original.Get("thinking.type").String() == outgoing.Get("thinking.type").String()
		case "max_tool_calls":
			preserved = omit || endpoint == EndpointResponses && equalNumber(original.Get(req.Path), outgoing.Get(req.Path))
		default:
			if strings.HasPrefix(req.Key, "tool_choice.") {
				preserved = preservesToolChoice(original, outgoing, intent.Endpoint, endpoint)
			} else {
				// Cache declarations are compared below in one pass so a shifted
				// cross-protocol block cannot satisfy more than one requirement.
				continue
			}
		}
		if !preserved {
			return nil, 0, &WireMismatch{Path: strings.TrimPrefix(req.Path, ".")}
		}
	}
	if path := changedCachePath(intent.Endpoint, endpoint, original, outgoing); path != "" {
		return nil, 0, &WireMismatch{Path: path}
	}
	if !omit || !outgoing.Get("max_tool_calls").Exists() {
		return body, limit, nil
	}
	prepared, err := sjson.DeleteBytes(body, "max_tool_calls")
	return prepared, limit, err
}

// preservesToolChoice compares forced choices across the standard wire forms.
// Vendor-specific selectors require the same endpoint and complete selector.
func preservesToolChoice(original, outgoing gjson.Result, inbound, outbound string) bool {
	before, after := original.Get("tool_choice"), outgoing.Get("tool_choice")
	if inbound == outbound {
		return equalJSON(before, after)
	}
	kind, name := forcedTool(before)
	wireKind, wireName := forcedTool(after)
	if (kind != "required" && kind != "function") || kind != wireKind || name != wireName {
		return false
	}
	if namespace := before.Get("namespace"); namespace.Exists() && !equalJSON(namespace, after.Get("namespace")) {
		return false
	}
	if before.Get("disable_parallel_tool_use").Bool() && outgoing.Get("parallel_tool_calls").Type != gjson.False {
		return false
	}
	return original.Get("parallel_tool_calls").Type != gjson.False || after.Get("disable_parallel_tool_use").Bool()
}

// AllowsRepair checks the original output cap and forbids schema or cache
// deletion by the legacy rejection normalizer. prompt_cache_breakpoint has its
// own legacy rule and is outside cache_control equivalence.
func AllowsRepair(ctx context.Context, before, after []byte) bool {
	intent, strict := StrictIntent(ctx)
	if !strict {
		return true
	}
	if ctx.Err() != nil {
		return false
	}
	original := gjson.Parse(intent.raw)
	previous, proposed := gjson.ParseBytes(before), gjson.ParseBytes(after)
	for _, path := range []string{"max_output_tokens", "max_completion_tokens", "max_tokens", "tool_choice", "temperature", "max_tool_calls", "thinking", "text.format", "response_format", "tools"} {
		if original.Get(path).Exists() && !equalJSON(previous.Get(path), proposed.Get(path)) {
			return false
		}
	}
	return changedCachePath(EndpointResponses, EndpointResponses, previous, proposed) == ""
}

// AllowsMessagesRepair checks that the proposed wire document preserves the
// frozen controls. Derived budgets can change as long as the client did not
// specify an output cap that the proposed repair would alter.
func AllowsMessagesRepair(ctx context.Context, initial, proposed []byte) bool {
	if ctx.Err() != nil {
		return false
	}
	intent, _ := StrictIntent(ctx)
	_, _, err := PrepareOutbound(intent, EndpointMessages, proposed, true)
	if err != nil {
		return false
	}
	for _, path := range []string{"max_output_tokens", "max_completion_tokens", "max_tokens"} {
		if intent.Lookup(path).Origin == OriginExplicit &&
			!equalJSON(gjson.GetBytes(initial, "max_tokens"), gjson.GetBytes(proposed, "max_tokens")) {
			return false
		}
	}
	return true
}

// changedCachePath checks native block paths directly. A Responses-to-Messages
// text bridge may fold adjacent turns, so it compares cached text by speaker,
// exact text and order. An unsupported role or content kind fails closed.
func changedCachePath(inbound, outbound string, original, outgoing gjson.Result) string {
	required := cacheRequirements(inbound, original)
	if len(required) == 0 {
		return ""
	}
	wired := cacheRequirements(outbound, outgoing)
	if inbound == outbound {
		byPath := make(map[string]Requirement, len(wired))
		for _, req := range wired {
			byPath[req.Path] = req
		}
		for _, req := range required {
			actual := byPath[req.Path]
			if actual.Key != req.Key || !equalJSON(gjson.Parse(req.Value.Raw), gjson.Parse(actual.Value.Raw)) ||
				req.Path != "cache_control" && !equalJSON(gjson.Parse(req.block), gjson.Parse(actual.block)) {
				return req.Path
			}
		}
		return ""
	}
	if inbound != EndpointResponses || outbound != EndpointMessages {
		return strings.TrimPrefix(required[0].Path, ".")
	}
	at := 0
	for _, actual := range wired {
		if !strings.HasPrefix(actual.Path, "messages.") {
			continue
		}
		if at == len(required) {
			return required[0].Path
		}
		req := required[at]
		at++
		source := gjson.Parse(req.block).Get("text")
		content := gjson.Parse(actual.block)
		if req.role != actual.role || content.Get("type").String() != "text" ||
			source.Type != gjson.String || source.Str != content.Get("text").Str ||
			!equalJSON(gjson.Parse(req.Value.Raw), gjson.Parse(actual.Value.Raw)) {
			return req.Path
		}
	}
	if at < len(required) {
		return required[at].Path
	}
	return ""
}

// forcedTool normalizes native, wrapped Chat, and Messages function selectors.
func forcedTool(choice gjson.Result) (kind, name string) {
	if choice.Type == gjson.String {
		return choice.Str, ""
	}
	switch kind = choice.Get("type").String(); kind {
	case "any", "required":
		return "required", ""
	case "tool", "function":
		if name = choice.Get("name").String(); name == "" {
			name = choice.Get("function.name").String()
		}
		return "function", name
	default:
		return kind, choice.Get("name").String()
	}
}

// equalNumber compares decimal literals without rounding large integers.
func equalNumber(a, b gjson.Result) bool {
	if a.Type != gjson.Number || b.Type != gjson.Number {
		return false
	}
	if a.Raw == b.Raw {
		return true
	}
	for _, raw := range []string{a.Raw, b.Raw} {
		if len(raw) > 1024 {
			return false
		}
		if at := strings.IndexAny(raw, "eE"); at >= 0 {
			exponent, err := strconv.Atoi(raw[at+1:])
			if err != nil || exponent < -1024 || exponent > 1024 {
				return false
			}
		}
	}
	left, leftOK := new(big.Rat).SetString(a.Raw)
	right, rightOK := new(big.Rat).SetString(b.Raw)
	return leftOK && rightOK && left.Cmp(right) == 0
}

// equalJSON ignores object key order and whitespace while retaining numeric
// lexemes and the distinction between an absent key and explicit null.
func equalJSON(a, b gjson.Result) bool {
	if a.Raw == b.Raw {
		return true
	}
	if a.Type != b.Type || !a.Exists() || !b.Exists() {
		return false
	}
	var left, right any
	decode := func(raw string, target *any) error {
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.UseNumber()
		return decoder.Decode(target)
	}
	if decode(a.Raw, &left) != nil || decode(b.Raw, &right) != nil {
		return false
	}
	l, _ := json.Marshal(left)
	r, _ := json.Marshal(right)
	return string(l) == string(r)
}
