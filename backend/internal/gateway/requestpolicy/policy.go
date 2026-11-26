package requestpolicy

import (
	"fmt"
	"hash/fnv"
	"strings"
	"unicode"
)

// policy.go compiles independently configured entry rules and assigns rollout
// arms by logical request ID. Matching uses the API key's original group, the
// normalized protocol endpoint, and the client's exact model text. A candidate
// can change any of those values for forwarding without changing the policy
// that the original request enters.
//
// The global mode caps the individual rule. A strict rule outside its assigned
// percentage enters observe. Hashing the same local ID across revisions keeps
// earlier strict buckets when a later policy increases the percentage.

const (
	// Modes cap parameter enforcement globally and on individual matching rules.
	ModeOff     Mode = "off"
	ModeObserve Mode = "observe"
	ModeStrict  Mode = "strict"

	// Endpoints are the canonical paths used for the three admitted text protocols.
	EndpointMessages  = "/v1/messages"
	EndpointChat      = "/v1/chat/completions"
	EndpointResponses = "/v1/responses"

	// Each bucket represents one whole rollout percentage point.
	rolloutBuckets = 100
)

// Mode is the configured limit or the effective entry rollout arm.
type Mode string

// Entry identifies a request before routing, fallback, or protocol conversion.
type Entry struct {
	EntryGroupID   int64  `json:"entry_group_id"`   // authenticated entry group
	Endpoint       string `json:"inbound_endpoint"` // canonical entry path
	RequestedModel string `json:"requested_model"`  // original model name
}

// Rule caps and apportions strict execution for one exact entry identity.
type Rule struct {
	Entry
	Mode          Mode `json:"mode"`           // rule-specific rollout cap
	StrictPercent int  `json:"strict_percent"` // percent eligible for strict
}

// Config is the complete cross-protocol parameter policy stored at one revision.
type Config struct {
	Revision int64  `json:"revision"` // zero when absent
	Mode     Mode   `json:"mode"`     // global rollout cap
	Rules    []Rule `json:"rules"`    // independently matched entry rules
}

// compile builds a private exact-match index and normalizes omitted modes to
// off. All rules are validated before publication, including dormant rules,
// so toggling a switch cannot activate malformed or ambiguous entries.
func compile(config Config) (Config, map[Entry]Rule, error) {
	if !validMode(config.Mode) {
		return Config{}, nil, fmt.Errorf("invalid global mode %q", config.Mode)
	}
	if config.Mode == "" {
		config.Mode = ModeOff
	}
	rules := make([]Rule, len(config.Rules))
	index := make(map[Entry]Rule, len(rules))
	for i, rule := range config.Rules {
		if err := validateRule(rule, index); err != nil {
			return Config{}, nil, fmt.Errorf("rules[%d]: %w", i, err)
		}
		if rule.Mode == "" {
			rule.Mode = ModeOff
		}
		rules[i] = rule
		index[rule.Entry] = rule
	}
	config.Rules = rules
	return config, index, nil
}

// validateRule checks exact entry identity and the rule's rollout controls.
func validateRule(rule Rule, index map[Entry]Rule) error {
	switch rule.Endpoint {
	case EndpointMessages, EndpointChat, EndpointResponses:
	default:
		return fmt.Errorf("inbound_endpoint must be a standard text endpoint")
	}
	model := rule.RequestedModel
	switch {
	case rule.EntryGroupID <= 0:
		return fmt.Errorf("entry_group_id must be positive")
	case model == "" || strings.ContainsAny(model, "*?") || strings.IndexFunc(model, unicode.IsControl) >= 0:
		return fmt.Errorf("requested_model must be a literal model name")
	case !validMode(rule.Mode):
		return fmt.Errorf("invalid mode %q", rule.Mode)
	case rule.StrictPercent < 0 || rule.StrictPercent > rolloutBuckets:
		return fmt.Errorf("strict_percent must be between 0 and %d", rolloutBuckets)
	}
	if _, exists := index[rule.Entry]; exists {
		return fmt.Errorf("duplicate entry tuple")
	}
	return nil
}

// validMode accepts every configured mode including its omitted default.
func validMode(mode Mode) bool {
	return mode == "" || mode == ModeOff || mode == ModeObserve || mode == ModeStrict
}

// rolloutBucket assigns the site ID to an invariant percentage bucket. The
// phase-specific prefix allows parameter rollout alongside independent Responses
// conversion rollout without forcing the two cohorts onto the same buckets.
func rolloutBucket(requestID string) int {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte("request-parameters:"))
	_, _ = hash.Write([]byte(requestID))
	return int(hash.Sum64() % rolloutBuckets)
}
