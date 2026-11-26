package requestpolicy

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// candidates.go extracts the requirements used for route admission. A
// requirement retains the client path for errors and a capability key for
// resolving evidence. Different cache locations and cache durations need
// separate keys because preserving one declaration cannot establish support
// for other declarations in the same request.
//
// This pass reads the frozen client document, so a converter's derived value
// cannot supply evidence for a client constraint. In particular, a missing
// tool list at this stage cannot prove that a provider-side limit is unused.
// The field policy can make that determination later against prepared tools.

// Requirement identifies one original field whose semantics the route must preserve.
type Requirement struct {
	Path   string   // client field path
	Key    string   // route capability key
	Value  Value    // original client value
	Scopes []string // additional required evidence keys
	block  string   // original enclosing cache block
	role   string   // speaker for message content
}

// Requirements returns the explicit constraints of a strict request. Cache
// paths retain wire order and full array indices. Observation requests return
// no requirements for the scheduler to enforce.
func Requirements(intent Intent) []Requirement {
	if intent.Mode != ModeStrict {
		return nil
	}
	root := gjson.Parse(intent.RawJSON())
	var required []Requirement
	add := func(path, key string, raw gjson.Result) {
		if raw.Exists() && raw.Type != gjson.Null {
			required = append(required, Requirement{Path: path, Key: key, Value: Value{Origin: OriginExplicit, Raw: raw.Raw}})
		}
	}
	add("temperature", "temperature", root.Get("temperature"))
	add("thinking.budget_tokens", "thinking.budget_tokens", root.Get("thinking.budget_tokens"))

	choice := root.Get("tool_choice")
	switch {
	case choice.Type == gjson.String && choice.String() == "required":
		add("tool_choice", "tool_choice.required", choice)
	case choice.IsObject():
		switch kind := choice.Get("type").String(); kind {
		case "auto", "none":
		case "any", "required":
			add("tool_choice", "tool_choice.required", choice)
		case "tool", "function":
			add("tool_choice", "tool_choice.named_function", choice)
		default:
			add("tool_choice", "tool_choice."+kind, choice)
		}
	}
	if limit := root.Get("max_tool_calls"); intent.Endpoint == EndpointResponses && limit.Exists() && limit.Type != gjson.Null {
		required = append(required, Requirement{
			Path: "max_tool_calls", Key: "max_tool_calls",
			Value: Value{Origin: OriginExplicit, Raw: limit.Raw}, Scopes: toolLimitScopes(root),
		})
	}
	return append(required, cacheRequirements(intent.Endpoint, root)...)
}

// toolLimitScopes collects evidence keys for built-in tool kinds found in
// the original shape. Hosted templates, inherited state and unresolved tools
// require separate evidence because the provider may supply their declarations.
func toolLimitScopes(root gjson.Result) []string {
	var scopes []string
	for _, path := range []string{"previous_response_id", "conversation", "conversation_id", "prompt", "session", "session_id"} {
		if value := root.Get(path); value.Exists() && value.Type != gjson.Null {
			scopes = append(scopes, "max_tool_calls.inherited")
			break
		}
	}
	if tools := root.Get("tools"); tools.Exists() {
		scopes = appendToolLimitScopes(scopes, tools)
	}
	for _, item := range root.Get("input").Array() {
		if strings.TrimSpace(item.Get("type").String()) == "additional_tools" {
			scopes = appendToolLimitScopes(scopes, item.Get("tools"))
		}
	}
	return scopes
}

// appendToolLimitScopes records hosted and unresolved tool kinds in a declaration array.
func appendToolLimitScopes(scopes []string, tools gjson.Result) []string {
	if !tools.IsArray() {
		return append(scopes, "max_tool_calls.unresolved_tools")
	}
	for _, tool := range tools.Array() {
		switch kind := tool.Get("type").String(); kind {
		case "function", "custom":
		case "", "namespace":
			scopes = append(scopes, "max_tool_calls.unresolved_tools")
		default:
			scopes = append(scopes, "max_tool_calls."+kind)
		}
	}
	return scopes
}

// ToolLimitScopes returns every capability needed to retain the original limit
// after preparation. An opaque session header also requires inherited support.
func ToolLimitScopes(original string, prepared []byte, inherited bool) []string {
	scopes := append(toolLimitScopes(gjson.Parse(original)), toolLimitScopes(gjson.ParseBytes(prepared))...)
	if inherited {
		scopes = append(scopes, "max_tool_calls.inherited")
	}
	return scopes
}

// cacheRequirements collects all cache declarations at supported client
// locations. The capability identity includes both location and the wire
// directive, so evidence for an ephemeral 5m user content block cannot
// authorize a system cache or a 1h block. Omitted TTL has its own identity
// since equivalence with a particular duration depends on the route.
func cacheRequirements(endpoint string, root gjson.Result) []Requirement {
	var required []Requirement
	add := func(scope, path, role string, block gjson.Result) {
		cache := block.Get("cache_control")
		if !cache.Exists() || cache.Type == gjson.Null {
			return
		}
		ttl := cache.Get("ttl")
		label := "default"
		if ttl.Exists() {
			label = ttl.String()
		}
		key := strings.Join([]string{"cache_control", scope, cache.Get("type").String(), label}, ".")
		if declared := cache.Get("scope"); declared.Exists() {
			key += "." + declared.String()
		}
		if path != "" {
			path += "."
		}
		required = append(required, Requirement{
			Path: path + "cache_control", Key: key, Value: Value{Origin: OriginExplicit, Raw: cache.Raw},
			block: block.Raw, role: role,
		})
	}
	switch endpoint {
	case EndpointResponses:
		for i, item := range root.Get("input").Array() {
			role := item.Get("role").String()
			for j, block := range item.Get("content").Array() {
				scope := "input.content." + role + "." + block.Get("type").String()
				add(scope, "input."+strconv.Itoa(i)+".content."+strconv.Itoa(j), role, block)
			}
		}
	case EndpointMessages, EndpointChat:
		add("request", "", "", root)
		for _, field := range []string{"system", "tools"} {
			for i, block := range root.Get(field).Array() {
				add(field, field+"."+strconv.Itoa(i), "", block)
			}
		}
		for i, item := range root.Get("messages").Array() {
			role := item.Get("role").String()
			for j, block := range item.Get("content").Array() {
				scope := "messages.content." + role + "." + block.Get("type").String()
				add(scope, "messages."+strconv.Itoa(i)+".content."+strconv.Itoa(j), role, block)
			}
		}
	}
	return required
}
