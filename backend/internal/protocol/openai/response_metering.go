package openai

import (
	"errors"
	"maps"
	"math"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/TokenFlux/TokenRouter/internal/protocol/wirejson"
)

// response_metering.go validates and reconciles usage evidence for one
// converted Responses attempt. Observations keep field presence until the
// existing OpenAI extractor computes forwarding counters. This lets a sparse
// report supplement another frame and keeps an actual zero distinguishable
// from an omitted counter. The top-level and nested views of one frame
// describe the same instant, so the parser reconciles them before exposing
// the frame to the resolver.
//
// Two terminal snapshots represent the same cumulative work. An all-zero
// placeholder does not erase positive evidence, while conflicting populated
// terminal counters invalidate terminal accounting. Progressive observations
// may grow across frames, but a decrease leaves that progressive source
// unverified. A conflict within one frame carries a distinct error so the
// resolver can reject an authoritative terminal without losing independent
// observations.

// responsesMeteringCounts maps interpreted native and alias counters to the
// canonical paths consumed by the existing Responses usage parser.
var (
	responsesMeteringCounts = map[string][]string{
		"input_tokens":                                                   {"input_tokens", "prompt_tokens"},
		"output_tokens":                                                  {"output_tokens", "completion_tokens"},
		"total_tokens":                                                   {"total_tokens"},
		"prompt_cache_miss_tokens":                                       {"prompt_cache_miss_tokens"},
		"input_tokens_details.cached_tokens":                             append(append([]string{}, cacheReadDetailPaths...), cacheReadAliasPaths...),
		"cache_creation_input_tokens":                                    append(append([]string{}, cacheWriteDetailPaths...), cacheWriteAliasPaths...),
		"input_tokens_details.image_tokens":                              {"input_tokens_details.image_tokens", "prompt_tokens_details.image_tokens"},
		"input_tokens_details.cached_tokens_details.image_tokens":        {"input_tokens_details.cached_tokens_details.image_tokens", "prompt_tokens_details.cached_tokens_details.image_tokens"},
		"output_tokens_details.image_tokens":                             {"output_tokens_details.image_tokens", "completion_tokens_details.image_tokens"},
		"output_tokens_details.reasoning_tokens":                         {"output_tokens_details.reasoning_tokens", "completion_tokens_details.reasoning_tokens"},
		"input_tokens_details.cache_write_tokens_details.ttl_30m_tokens": {cacheWriteDetailsPath + ".ttl_30m_tokens", promptWriteDetailsPath + ".ttl_30m_tokens"},
		"input_tokens_details.cache_write_tokens_details.ttl_12h_tokens": {cacheWriteDetailsPath + ".ttl_12h_tokens", promptWriteDetailsPath + ".ttl_12h_tokens"},
	}

	// These errors distinguish malformed counters from contradictory reports of the same counter in an upstream response.
	errInvalidResponsesMetering     = errors.New("invalid upstream Responses metering counters")
	errConflictingResponsesMetering = errors.New("conflicting upstream Responses metering counters")
)

// ResponsesMetering holds validated cumulative counter fields for a single evidence source.
type ResponsesMetering struct {
	counts  map[string]int // counters with explicit field presence
	problem error          // malformed or inconsistent evidence
	seen    bool           // nonnull usage object reported
}

// ParseResponsesMetering validates the counter evidence in one Responses frame.
// Top-level and nested usage can supply missing fields to each other, but
// populated overlaps must agree unless one whole view is a zero placeholder.
// Both usage objects must contain nonnegative bounded counters wherever
// reported. This comparison applies even on a progressive frame because
// its two views describe one observation, so neither view represents growth.
func ParseResponsesMetering(payload []byte) ResponsesMetering {
	root := gjson.ParseBytes(payload)
	var m ResponsesMetering
	for _, prefix := range []string{"", "response."} {
		node := root.Get(prefix + "usage")
		if !node.Exists() || node.Type == gjson.Null {
			continue
		}
		if !node.IsObject() || (!m.seen && wirejson.ValidateUsageEnvelope(payload) != nil) {
			m.seen = true
			m.problem = errInvalidResponsesMetering
			return m
		}
		counts, err := readResponsesMeteringCounts(node, root.Get(prefix+"tool_usage.image_gen"))
		if err := m.Merge(ResponsesMetering{seen: true, counts: counts, problem: err}, true); err != nil {
			return m
		}
	}
	return m
}

// Merge observes another view of cumulative work and reports incompatible
// populated values. Terminal views must agree, while progressive views may
// increase. An explicit all-zero placeholder leaves earlier positive fields
// intact, and an all-zero earlier view yields to a positive one. Malformed
// evidence makes the source unverified, while conflicting counters also return
// an error so the resolver can reject a terminal. Evidence from other sources
// stays available because the receiver owns only one source.
func (m *ResponsesMetering) Merge(next ResponsesMetering, terminal bool) error {
	m.seen = m.seen || next.seen
	if next.Invalid() {
		m.problem = next.problem
		if next.Conflicting() {
			return next.problem
		}
		return nil
	}
	if len(next.counts) == 0 {
		return nil
	}
	if next.zeroPlaceholder() && m.hasPositive() {
		return nil
	}
	if m.zeroPlaceholder() && next.hasPositive() {
		m.counts = nil
	}
	if m.counts == nil {
		m.counts = maps.Clone(next.counts)
		return nil
	}
	for field, current := range m.counts {
		value, exists := next.counts[field]
		if exists && value != current && (terminal || value < current) {
			m.problem = errConflictingResponsesMetering
			return m.problem
		}
	}
	maps.Copy(m.counts, next.counts)
	return nil
}

// OverlayResponsesMetering selects authoritative terminal fields over earlier
// progressive fields. The caller chooses independently verified evidence when
// terminal reports are contradictory. Both inputs remain available unchanged.
func OverlayResponsesMetering(earlier, terminal ResponsesMetering) (selected ResponsesMetering, usedEarlier, usedTerminal bool) {
	if !terminal.seen || (terminal.zeroPlaceholder() && earlier.hasPositive()) {
		return earlier, earlier.seen, false
	}
	if terminal.Invalid() || earlier.Invalid() || earlier.zeroPlaceholder() {
		return terminal, false, true
	}
	if len(terminal.counts) == 0 && earlier.HasValues() {
		return earlier, true, false
	}
	selected = ResponsesMetering{seen: true, counts: maps.Clone(terminal.counts)}
	if selected.counts == nil {
		selected.counts = make(map[string]int)
	}
	// A cumulative total reported before a later input or output change need
	// not include that change. Recompute only when the terminal lacks its own total.
	for field, value := range earlier.counts {
		if _, reported := terminal.counts[field]; !reported && field != "total_tokens" {
			selected.counts[field] = value
			usedEarlier = true
		}
	}
	return selected, usedEarlier, true
}

// Result returns normalized usage and distinguishes missing, invalid, and
// complete-zero observations. An incomplete positive report remains useful
// when it establishes at least one chargeable bucket without estimation.
func (m ResponsesMetering) Result() (usage ForwardUsage, seen, valid bool) {
	if !m.seen || m.Invalid() || len(m.counts) == 0 {
		return ForwardUsage{}, m.seen, false
	}
	body, err := m.Patch([]byte("{}"))
	if err != nil {
		return ForwardUsage{}, true, false
	}
	usage, ok := ExtractOpenAIUsageFromJSONBytes(body)
	return usage, true, ok && (OpenAIUsageHasTokens(&usage) || m.zeroPlaceholder())
}

// Patch fills canonical fields in a successful response while preserving
// provider usage extensions. Missing counters are never synthesized from
// output, and an invalid observation does not rewrite the wire counts.
func (m ResponsesMetering) Patch(body []byte) ([]byte, error) {
	if m.Invalid() {
		return body, nil
	}
	var err error
	for field, value := range m.counts {
		body, err = sjson.SetBytes(body, "usage."+field, value)
		if err != nil {
			return nil, err
		}
		for _, alias := range responsesMeteringCounts[field] {
			if alias == field || !gjson.GetBytes(body, "usage."+alias).Exists() {
				continue
			}
			body, err = sjson.SetBytes(body, "usage."+alias, value)
			if err != nil {
				return nil, err
			}
		}
	}
	if _, hasTotal := m.counts["total_tokens"]; !hasTotal && gjson.GetBytes(body, "usage.total_tokens").Exists() {
		input, hasInput := m.counts["input_tokens"]
		output, hasOutput := m.counts["output_tokens"]
		if hasInput && hasOutput {
			return sjson.SetBytes(body, "usage.total_tokens", input+output)
		}
	}
	return body, nil
}

// Invalid reports whether this source supplied malformed or contradictory counters.
func (m ResponsesMetering) Invalid() bool { return m.problem != nil }

// Conflicting reports whether the metering source contains contradictory counters.
func (m ResponsesMetering) Conflicting() bool { return m.problem == errConflictingResponsesMetering }

// HasValues reports whether the source supplied any recognized accounting fields.
func (m ResponsesMetering) HasValues() bool { return len(m.counts) != 0 }

// hasPositive identifies a positive value anywhere in the reported fields.
func (m ResponsesMetering) hasPositive() bool {
	for _, value := range m.counts {
		if value > 0 {
			return true
		}
	}
	return false
}

// zeroPlaceholder recognizes whole-observation zero before comparing terminal fields.
func (m ResponsesMetering) zeroPlaceholder() bool {
	if m.Invalid() || m.hasPositive() {
		return false
	}
	_, input := m.counts["input_tokens"]
	_, output := m.counts["output_tokens"]
	return input && output
}

// readResponsesMeteringCounts reconciles spellings within one usage object.
// Every supplied alias must be a bounded counter, and positive aliases for the
// same bucket must agree. A zero alias retains its existing precedence on
// cache and detail buckets. Input and output totals and hosted-image details
// retain their existing zero fallback, so a provider using both wire formats
// can still supply a single usable count for those buckets.
func readResponsesMeteringCounts(usage, image gjson.Result) (map[string]int, error) {
	if image.Exists() && image.Type != gjson.Null &&
		(!image.IsObject() || wirejson.ValidateUsageObject([]byte(image.Raw)) != nil) {
		return nil, errInvalidResponsesMetering
	}
	counts := make(map[string]int)
	conflicting := false
	for field, paths := range responsesMeteringCounts {
		var positive int
		for _, path := range paths {
			raw := usage.Get(path)
			if !raw.Exists() {
				continue
			}
			count, ok := BoundedJSONNonNegativeInt(raw)
			if !ok || count > math.MaxInt/4 {
				return nil, errInvalidResponsesMetering
			}
			if count > 0 {
				conflicting = conflicting || (positive > 0 && positive != count)
				positive = count
			}
			// Input and output totals retain the compatibility fallback to a
			// positive prompt or completion alias when the native field is zero.
			if current, present := counts[field]; !present || ((field == "input_tokens" || field == "output_tokens") && current == 0) {
				counts[field] = count
			}
		}
		if field != "input_tokens_details.image_tokens" && field != "output_tokens_details.image_tokens" {
			continue
		}
		raw := image.Get(field)
		if !raw.Exists() {
			continue
		}
		count, ok := BoundedJSONNonNegativeInt(raw)
		if !ok || count > math.MaxInt/4 {
			return nil, errInvalidResponsesMetering
		}
		conflicting = conflicting || (positive > 0 && count > 0 && positive != count)
		// Hosted-image details fill a zero main usage bucket when an upstream
		// reports image tokens in its separate tool-level usage object.
		if counts[field] == 0 {
			counts[field] = count
		}
	}
	if conflicting {
		return nil, errConflictingResponsesMetering
	}
	if !validateResponsesMeteringShape(usage, counts) {
		return nil, errInvalidResponsesMetering
	}
	return counts, nil
}

// validateResponsesMeteringShape rejects scalar detail containers and impossible
// cache or image subsets when their inclusive counters are present. Sparse
// reports that omit the inclusive counter retain their reported details.
func validateResponsesMeteringShape(usage gjson.Result, counts map[string]int) bool {
	for _, path := range []string{"input_tokens_details", "prompt_tokens_details", "output_tokens_details", "completion_tokens_details", cacheWriteDetailsPath, promptWriteDetailsPath, "input_tokens_details.cached_tokens_details", "prompt_tokens_details.cached_tokens_details"} {
		value := usage.Get(path)
		if value.Exists() && value.Type != gjson.Null && !value.IsObject() {
			return false
		}
	}
	for detail, value := range counts {
		total, _, isDetail := strings.Cut(detail, "_details.")
		if !isDetail || !strings.HasSuffix(detail, "image_tokens") && !strings.HasSuffix(detail, "cached_tokens") {
			continue
		}
		if inclusive, present := counts[total]; present && value > inclusive {
			return false
		}
	}
	return true
}
