package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"

	wire "github.com/TokenFlux/TokenRouter/internal/protocol/openai"
)

// response_compatibility_output.go reconstructs raw Responses output by
// position while retaining each item's protocol identities and extension fields.
// Output and content positions are separate namespaces. Text from one part
// cannot append to another, and a function-call ID cannot move between items
// merely because a later snapshot supplies a different index.
//
// Deltas and completed snapshots describe the same output, so materialization
// checks their common content before choosing the richest supported value.
// Added events can carry empty shells and nonfinal item status. Terminal
// snapshots can supply missing objects or fields, but incompatible populated
// observations produce a protocol error before any JSON response is published.

// The position cap bounds per-attempt index and content bookkeeping without allocating sparse arrays.
const (
	responsesMaxOutputPositions = 4096
)

// responsesObject preserves JSON object fields and their decoded numeric spellings.
type responsesObject = map[string]any

// responsesOutput binds item and call IDs to ordered slots within one response.
type responsesOutput struct {
	indices map[int]*responsesOutputItem    // explicit output index bindings
	items   map[string]*responsesOutputItem // response item identity bindings
	calls   map[string]*responsesOutputItem // tool call identity bindings
	order   []*responsesOutputItem          // stable first-observed slot order
}

// responsesOutputItem holds aggregate fields for one output item and its parts.
type responsesOutputItem struct {
	index  int                                 // output position, or -1
	fields responsesObject                     // merged item-level snapshot fields
	parts  map[responsesPartKey]*responsesPart // independently positioned content
	text   map[string]*responsesText           // item-level streamed argument fields
}

// responsesPartKey distinguishes message content from reasoning summary positions.
type responsesPartKey struct {
	group string // containing array field
	index int    // position inside array
}

// responsesPart retains one position's metadata and accumulated textual value.
type responsesPart struct {
	fields responsesObject // part metadata and identity
	text   *responsesText  // optional streamed content field
}

// responsesText distinguishes an opening value, ordered fragments, and final snapshots.
type responsesText struct {
	field    string          // text, refusal, or argument field
	prefix   string          // bytes from an added snapshot
	delta    strings.Builder // subsequent fragments in wire order
	final    string          // value from a done or terminal snapshot
	hasFinal bool            // final snapshot was supplied
}

// newResponsesOutput constructs an empty output index for one attempt.
func newResponsesOutput() responsesOutput {
	return responsesOutput{
		indices: make(map[int]*responsesOutputItem),
		items:   make(map[string]*responsesOutputItem),
		calls:   make(map[string]*responsesOutputItem),
	}
}

// observe accepts an output event and selects its item and part using both
// index and identity evidence. A missing response ID is handled by the caller.
// Unknown event kinds do not create synthetic output items.
func (o *responsesOutput) observe(event string, body responsesObject) error {
	kind, path, field, done := responsesOutputEvent(event)
	if kind == "" {
		return nil
	}
	index, err := responsesPosition(body["output_index"])
	if err != nil {
		return err
	}
	itemID, err := responsesString(body, "item_id")
	if err != nil {
		return err
	}
	callID, err := responsesString(body, "call_id")
	if err != nil {
		return err
	}
	if kind == "item" {
		item, ok := body["item"].(responsesObject)
		if !ok {
			return errors.New("upstream Responses item event has no item object")
		}
		return o.observeItem(index, item, itemID, callID, done)
	}
	if path == "" && field == "" {
		if index < 0 && itemID == "" && callID == "" {
			return nil
		}
		item := responsesObject{"type": kind, "status": "completed", "result": body["result"]}
		return o.observeItem(index, item, itemID, callID, true)
	}
	slot, err := o.bind(index, itemID, callID)
	if err != nil {
		return err
	}
	if err := mergeResponsesObject(slot.fields, responsesObject{"type": kind, "name": body["name"]}); err != nil {
		return err
	}
	if path == "" {
		return slot.observeArgument(field, body, done)
	}
	target := "content_index"
	if path == "summary" {
		target = "summary_index"
	}
	partIndex, err := responsesPosition(body[target])
	if err != nil {
		return err
	}
	if partIndex < 0 {
		partIndex = 0
	}
	return slot.observePart(path, partIndex, field, body, done)
}

// observeArray applies a terminal output array using its zero-based positions.
// Empty arrays can be supplemented by other observations. Any populated item
// must agree with the identities and fields already observed at that position.
func (o *responsesOutput) observeArray(raw any) error {
	if raw == nil {
		return nil
	}
	items, ok := raw.([]any)
	if !ok || len(items) > responsesMaxOutputPositions {
		return errors.New("upstream Responses output is not a bounded array")
	}
	for index, value := range items {
		item, ok := value.(responsesObject)
		if !ok {
			return errors.New("upstream Responses output contains a non-object item")
		}
		if err := o.observeItem(index, item, "", "", true); err != nil {
			return err
		}
	}
	return nil
}

// observeItem merges a full observation while preserving part positions. Item
// status may advance from an opening status, and textual fields reconcile
// separately with deltas when the resolver builds the output array.
func (o *responsesOutput) observeItem(index int, item responsesObject, eventItemID, eventCallID string, done bool) error {
	itemID, err := responsesString(item, "id")
	if err != nil {
		return err
	}
	callID, err := responsesString(item, "call_id")
	if err != nil {
		return err
	}
	if (itemID != "" && eventItemID != "" && itemID != eventItemID) ||
		(callID != "" && eventCallID != "" && callID != eventCallID) {
		return errors.New("upstream Responses output event disagrees with its item identities")
	}
	slot, err := o.bind(index, firstResponsesString(itemID, eventItemID), firstResponsesString(callID, eventCallID))
	if err != nil {
		return err
	}
	kind, err := responsesString(item, "type")
	if err != nil {
		return err
	}
	for field, value := range item {
		if err := slot.observeItemField(kind, field, value, done); err != nil {
			return err
		}
	}
	return nil
}

// observeItemField routes item fields to their position-aware merge rules.
func (i *responsesOutputItem) observeItemField(kind, field string, value any, done bool) error {
	if value == nil {
		return nil
	}
	switch {
	case (field == "content" && (kind == "message" || (kind == "" && i.fields["type"] == "message"))) ||
		(field == "summary" && (kind == "reasoning" || (kind == "" && i.fields["type"] == "reasoning"))):
		parts, ok := value.([]any)
		if !ok || len(parts) > responsesMaxOutputPositions {
			return errors.New("upstream Responses item parts are not a bounded array")
		}
		if len(parts) == 0 && i.fields[field] == nil {
			i.fields[field] = parts
		}
		for index, raw := range parts {
			part, ok := raw.(responsesObject)
			if !ok {
				return errors.New("upstream Responses item contains a non-object part")
			}
			if err := i.part(field, index).observe(part, done); err != nil {
				return err
			}
		}
		return nil
	case field == "arguments" && kind == "function_call", field == "input" && kind == "custom_tool_call":
		text, ok := value.(string)
		if !ok {
			return errors.New("upstream Responses tool input is not a string")
		}
		return i.argument(field).set(text, done)
	case field == "id" || field == "call_id" || field == "type":
		if value == "" {
			return nil
		}
	case field == "status":
		next, ok := value.(string)
		if !ok {
			return errors.New("upstream Responses item status is not a string")
		}
		current, _ := i.fields[field].(string)
		if next == "" || (isResponsesOpenStatus(next) && current != "") {
			return nil
		}
		if isResponsesOpenStatus(current) {
			delete(i.fields, field)
		}
	}
	return mergeResponsesObject(i.fields, responsesObject{field: value})
}

// bind returns the slot corroborated by any supplied identity or position.
// Explicit identifiers remain attached to the first bound slot. A later
// observation can supply a missing identifier or index, but identifying two
// existing slots at once makes their accumulated positions ambiguous.
func (o *responsesOutput) bind(index int, itemID, callID string) (*responsesOutputItem, error) {
	var slot *responsesOutputItem
	for _, candidate := range []*responsesOutputItem{o.indices[index], o.items[itemID], o.calls[callID]} {
		if candidate == nil {
			continue
		}
		if slot != nil && slot != candidate {
			return nil, errors.New("upstream Responses output index and identities designate different items")
		}
		slot = candidate
	}
	if slot == nil {
		if index < 0 && itemID == "" && callID == "" {
			return nil, errors.New("upstream Responses output event has no index or item identity")
		}
		if len(o.order) >= responsesMaxOutputPositions {
			return nil, errors.New("upstream Responses output exceeds the item limit")
		}
		slot = &responsesOutputItem{
			index: -1, fields: make(responsesObject),
			parts: make(map[responsesPartKey]*responsesPart), text: make(map[string]*responsesText),
		}
		o.order = append(o.order, slot)
	}
	if index >= 0 && slot.index >= 0 && index != slot.index {
		return nil, errors.New("upstream Responses output identity moved between indexes")
	}
	identities := make(responsesObject, 2)
	if itemID != "" {
		identities["id"] = itemID
	}
	if callID != "" {
		identities["call_id"] = callID
	}
	if err := mergeResponsesObject(slot.fields, identities); err != nil {
		return nil, err
	}
	if index >= 0 {
		slot.index = index
		o.indices[index] = slot
	}
	if itemID != "" {
		o.items[itemID] = slot
	}
	if callID != "" {
		o.calls[callID] = slot
	}
	return slot, nil
}

// build materializes observed positions in index order and merges their fragments.
// Items that never report a position follow indexed items in observation order.
// The caller checks whether any returned item establishes useful output.
func (o *responsesOutput) build() ([]responsesObject, error) {
	ordered := append([]*responsesOutputItem(nil), o.order...)
	sort.SliceStable(ordered, func(a, b int) bool {
		left, right := ordered[a].index, ordered[b].index
		return left >= 0 && (right < 0 || left < right)
	})
	items := make([]responsesObject, 0, len(ordered))
	for _, item := range ordered {
		if err := item.materialize(); err != nil {
			return nil, err
		}
		items = append(items, item.fields)
	}
	return items, nil
}

// observeArgument applies a tool delta or its full input snapshot.
func (i *responsesOutputItem) observeArgument(field string, body responsesObject, done bool) error {
	if field == "arguments" && i.fields["type"] == "tool_search_call" {
		if done && body[field] != nil {
			if value, ok := body[field].(string); !ok {
				encoded, err := json.Marshal(body[field])
				if err != nil {
					return err
				}
				body = responsesObject{field: string(encoded)}
			} else if !json.Valid([]byte(value)) {
				return errors.New("upstream Responses tool search arguments are invalid")
			}
		}
	}
	return i.argument(field).observe(body, done)
}

// argument selects or creates the accumulator for one tool input field.
func (i *responsesOutputItem) argument(field string) *responsesText {
	if i.text[field] == nil {
		i.text[field] = &responsesText{field: field}
	}
	return i.text[field]
}

// observePart applies a content event to its selected part position.
func (i *responsesOutputItem) observePart(group string, index int, field string, body responsesObject, done bool) error {
	part := i.part(group, index)
	if field == "" {
		raw, ok := body["part"].(responsesObject)
		if !ok {
			return errors.New("upstream Responses content event has no part object")
		}
		return part.observe(raw, done)
	}
	kind := field
	if field == "text" {
		kind = "output_text"
		if group == "summary" {
			kind = "summary_text"
		}
	}
	if err := mergeResponsesObject(part.fields, responsesObject{"type": kind}); err != nil {
		return err
	}
	return part.textObservation(field, body, done)
}

// part selects or creates the accumulator for one content or summary position.
func (i *responsesOutputItem) part(group string, index int) *responsesPart {
	key := responsesPartKey{group: group, index: index}
	if i.parts[key] == nil {
		i.parts[key] = &responsesPart{fields: make(responsesObject)}
	}
	return i.parts[key]
}

// observe associates part metadata and its opening or final content.
func (p *responsesPart) observe(part responsesObject, done bool) error {
	kind, err := responsesString(part, "type")
	if err != nil {
		return err
	}
	for field, value := range part {
		if (field == "text" || field == "refusal" || field == "transcript") && value != nil {
			text, ok := value.(string)
			if !ok {
				return errors.New("upstream Responses content is not a string")
			}
			if err := p.setTextField(field); err != nil {
				return err
			}
			if err := p.text.set(text, done); err != nil {
				return err
			}
			continue
		}
		if field == "type" && kind == "" {
			continue
		}
		if err := mergeResponsesObject(p.fields, responsesObject{field: value}); err != nil {
			return err
		}
	}
	return nil
}

// setTextField binds one content position to its textual representation.
func (p *responsesPart) setTextField(field string) error {
	if p.text == nil {
		p.text = &responsesText{field: field}
	}
	if p.text.field != field {
		return errors.New("upstream Responses content part has conflicting text fields")
	}
	return nil
}

// textObservation applies a delta or a full value to one previously resolved part.
func (p *responsesPart) textObservation(field string, body responsesObject, done bool) error {
	if err := p.setTextField(field); err != nil {
		return err
	}
	return p.text.observe(body, done)
}

// observe reads incremental or full text using the event-specific field name.
func (t *responsesText) observe(body responsesObject, done bool) error {
	field := "delta"
	if done {
		field = t.field
	}
	text, ok := body[field].(string)
	if !ok {
		return errors.New("upstream Responses delta or final content is not a string")
	}
	if done {
		return t.set(text, true)
	}
	_, err := t.delta.WriteString(text)
	return err
}

// set records a nonempty snapshot and verifies repeated populated values agree.
// Empty opening snapshots are placeholders, while empty final fields can
// contradict nonempty final snapshots or deltas at the same position.
func (t *responsesText) set(value string, done bool) error {
	if !done {
		if value == "" {
			return nil
		}
		if t.prefix != "" && t.prefix != value {
			return errors.New("upstream Responses has conflicting opening content")
		}
		t.prefix = value
		return nil
	}
	if t.hasFinal && t.final != value {
		return errors.New("upstream Responses has conflicting final content")
	}
	t.final, t.hasFinal = value, true
	return nil
}

// value reconciles all accumulated fragments with the supplied full content.
// The snapshot can contain a suffix missing from deltas, but cannot replace
// bytes already observed at that item and content position.
func (t *responsesText) value() (string, error) {
	observed := t.prefix + t.delta.String()
	if t.hasFinal {
		if !strings.HasPrefix(t.final, observed) {
			return "", errors.New("upstream Responses final content conflicts with its deltas")
		}
		return t.final, nil
	}
	return observed, nil
}

// materialize writes content and argument values into their JSON fields.
// All repeated observations have arrived before this method runs. It sorts
// positions without allocating arrays sized by a provider-supplied index.
func (i *responsesOutputItem) materialize() error {
	for field, text := range i.text {
		value, err := text.value()
		if err != nil {
			return err
		}
		if i.fields["type"] == "tool_search_call" && field == "arguments" {
			raw := wire.ToolSearchCallArgumentsJSON(value)
			var decoded any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				return err
			}
			if err := mergeResponsesObject(i.fields, responsesObject{field: decoded}); err != nil {
				return err
			}
		} else {
			i.fields[field] = value
		}
	}
	keys := make([]responsesPartKey, 0, len(i.parts))
	for key := range i.parts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(a, b int) bool {
		if keys[a].group != keys[b].group {
			return keys[a].group < keys[b].group
		}
		return keys[a].index < keys[b].index
	})
	for _, key := range keys {
		part := i.parts[key]
		if part.text != nil {
			value, err := part.text.value()
			if err != nil {
				return err
			}
			part.fields[part.text.field] = value
		}
		parts, _ := i.fields[key.group].([]any)
		i.fields[key.group] = append(parts, part.fields)
	}
	if i.fields["type"] == "message" && i.fields["role"] == nil {
		i.fields["role"] = "assistant"
	}
	return nil
}

// responsesOutputEvent maps known output events to fields used by reconstruction.
func responsesOutputEvent(event string) (kind, group, field string, done bool) {
	if tool, ok := strings.CutSuffix(strings.TrimPrefix(event, "response."), ".completed"); ok && responsesHostedTool(tool) {
		return tool, "", "", true
	}
	done = strings.HasSuffix(event, ".done")
	switch strings.TrimSuffix(strings.TrimSuffix(event, ".done"), ".delta") {
	case "response.output_text":
		return "message", "content", "text", done
	case "response.refusal":
		return "message", "content", "refusal", done
	case "response.audio_transcript":
		return "message", "content", "transcript", done
	case "response.reasoning_summary_text":
		return "reasoning", "summary", "text", done
	case "response.function_call_arguments":
		return "function_call", "", "arguments", done
	case "response.custom_tool_call_input":
		return "custom_tool_call", "", "input", done
	case "response.tool_search_call_arguments":
		return "tool_search_call", "", "arguments", done
	}
	switch event {
	case "response.output_item.added", "response.output_item.done":
		return "item", "", "", done
	case "response.content_part.added", "response.content_part.done":
		return "message", "content", "", done
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		return "reasoning", "summary", "", done
	default:
		return "", "", "", false
	}
}

// responsesPosition validates an optional output or content index before use.
// Absent indexes return -1 so identity-based binding remains distinguishable
// from an explicitly supplied index of zero.
func responsesPosition(raw any) (int, error) {
	if raw == nil {
		return -1, nil
	}
	value, ok := raw.(json.Number)
	if ok {
		index, err := strconv.Atoi(value.String())
		if err == nil && index >= 0 && index < responsesMaxOutputPositions {
			return index, nil
		}
	}
	return -1, errors.New("upstream Responses contains an invalid output or content index")
}

// mergeResponsesObject supplements missing fields without overwriting conflicting values.
// Nested objects merge by field, while an empty array can be supplemented by a
// populated one. Position-sensitive arrays use their dedicated accumulators.
func mergeResponsesObject(target, source responsesObject) error {
	for field, next := range source {
		current := target[field]
		switch {
		case next == nil:
		case current == nil:
			target[field] = next
		case reflect.DeepEqual(current, next):
		default:
			if left, ok := current.(responsesObject); ok {
				if right, ok := next.(responsesObject); ok {
					if err := mergeResponsesObject(left, right); err != nil {
						return err
					}
					continue
				}
			}
			left, leftArray := current.([]any)
			right, rightArray := next.([]any)
			if leftArray && rightArray {
				if len(left) == 0 {
					target[field] = next
					continue
				}
				if len(right) == 0 {
					continue
				}
			}
			return fmt.Errorf("upstream Responses has conflicting %.64q fields", field)
		}
	}
	return nil
}

// responsesString reads a string field without coercing numbers or objects.
func responsesString(object responsesObject, field string) (string, error) {
	if object[field] == nil {
		return "", nil
	}
	if value, ok := object[field].(string); ok {
		return value, nil
	}
	return "", fmt.Errorf("upstream Responses has a non-string %.64q field", field)
}

// firstResponsesString returns the first nonempty identity in the supplied pair.
func firstResponsesString(left, right string) string {
	if left != "" {
		return left
	}
	return right
}

// isResponsesOpenStatus recognizes item states that can advance to a terminal status.
func isResponsesOpenStatus(status string) bool {
	return status == "in_progress" || status == "queued" || status == "incomplete"
}

// ResponsesUsefulOutput checks reconstructed items for client-visible output.
// A refusal has its own classification when no other usable output is present.
// Tool declarations and reasoning metadata alone cannot establish completion.
func ResponsesUsefulOutput(body []byte) (visible, refused bool) {
	for _, item := range gjson.GetBytes(body, "output").Array() {
		switch item.Get("type").String() {
		case "message":
			for _, part := range item.Get("content").Array() {
				switch part.Get("type").String() {
				case "refusal":
					refused = refused || responsesNonemptyString(part.Get("refusal"))
				case "output_text":
					visible = visible || responsesNonemptyString(part.Get("text"))
				case "transcript", "output_audio":
					visible = visible || responsesNonemptyString(part.Get("transcript")) || responsesNonemptyString(part.Get("data"))
				case "output_image":
					visible = visible || responsesNonemptyString(part.Get("image_url"))
				}
			}
		case "refusal":
			refused = refused || responsesNonemptyString(item.Get("refusal"))
		case "function_call":
			args := item.Get("arguments").String()
			visible = visible || (responsesNamedTool(item) && json.Valid([]byte(args)) && gjson.Parse(args).IsObject())
		case "custom_tool_call":
			visible = visible || (responsesNamedTool(item) && responsesNonemptyString(item.Get("input")))
		case "tool_search_call":
			visible = visible || (responsesNonemptyString(item.Get("call_id")) && item.Get("arguments").IsObject()) ||
				responsesHostedItemCompleted(item)
		case "reasoning":
		default:
			kind := item.Get("type").String()
			visible = visible || (kind != "" && (wire.StreamItemHasVisibleOutput(item) || responsesHostedItemCompleted(item)))
		}
	}
	return visible, refused
}

// responsesHostedItemCompleted identifies a finished hosted call as usable output.
func responsesHostedItemCompleted(item gjson.Result) bool {
	return responsesHostedItemWorked(item) && item.Get("status").String() == "completed" &&
		(item.Get("type").String() != "image_generation_call" || responsesNonemptyString(item.Get("result")) || responsesNonemptyString(item.Get("url")))
}

// responsesNamedTool checks the minimum identity needed to dispatch a tool call.
func responsesNamedTool(item gjson.Result) bool {
	return responsesNonemptyString(item.Get("call_id")) && responsesNonemptyString(item.Get("name"))
}

// responsesNonemptyString recognizes content while preserving whitespace-only fragments.
func responsesNonemptyString(value gjson.Result) bool {
	return value.Type == gjson.String && value.Str != ""
}
