package responsecompat

import (
	"fmt"
	"hash/fnv"
	"path"
	"strings"
	"time"
	"unicode"
)

// policy.go validates Responses compatibility rules and decides the rollout arm
// for an exact outbound candidate. Validation builds an immutable tuple index
// before publication. A lookup needs only this index and the site's logical
// request ID, so changes to routing or storage cannot add work to the lookup.
//
// The global switch caps each rule. When both permit enforcement, the same hash
// places a logical request in the same bucket across instances and revisions.
// Increasing the percentage can therefore add buckets without reshuffling the
// cohort. The gateway owns the request's first decision and its forwarding.

const (
	Off     Mode = "off"     // compatibility inactive
	Observe Mode = "observe" // record a match and forward normally
	Enforce Mode = "enforce" // candidate eligible for conversion

	// rolloutBuckets assigns one bucket to each whole percentage point.
	rolloutBuckets = 100
)

// Mode is a configured switch or the effective rollout arm of a request.
type Mode string

// Tuple identifies one eligible outbound candidate after routing and model mapping.
type Tuple struct {
	EffectiveGroupID int64  `json:"effective_group_id"` // route's effective group
	ProviderID       int64  `json:"provider_id"`        // local provider identifier
	UpstreamModel    string `json:"upstream_model"`     // final outbound model
	UpstreamEndpoint string `json:"upstream_endpoint"`  // final outbound URL path
}

// Rule adds conversion permission and rollout controls to one exact tuple.
type Rule struct {
	Tuple               // exact outbound candidate identity
	Mode           Mode `json:"mode"`            // per-rule cap on the rollout arm
	SyncToStream   bool `json:"sync_to_stream"`  // explicit opt-in for the rule
	EnforcePercent int  `json:"enforce_percent"` // whole percent in the enforcement arm
}

// Config is the complete stored policy, with a revision assigned by the writer.
type Config struct {
	Revision int64  `json:"revision"` // zero denotes a missing setting
	Mode     Mode   `json:"mode"`     // global cap on every rollout arm
	Rules    []Rule `json:"rules"`    // exact rules with no duplicate tuples
}

// Decision reports the effective arm and the revision of the snapshot used to
// decide it. Enforce grants eligibility for that candidate, and Observe leaves
// its original forwarding intact. Off does not enter a rollout arm.
type Decision struct {
	Mode     Mode  // rollout arm for this candidate and logical request
	Revision int64 // evaluated snapshot revision, zero when no fresh version is available
}

// snapshot holds the immutable rule index for one validated configuration.
type snapshot struct {
	// Read times carry a monotonic component within the process. The generation
	// distinguishes overlapping authoritative operations at the same revision.
	readStarted time.Time
	generation  uint64
	revision    int64          // persisted version represented by this snapshot
	mode        Mode           // global rollout cap
	rules       map[Tuple]Rule // rules indexed by exact outbound identity
	changed     chan struct{}  // closed when this snapshot is superseded
}

// compile validates all rules even when a switch is off, so toggling a policy
// cannot expose a previously unchecked tuple. It returns normalized settings
// and an independent index for publishing without retaining the caller's slice.
func compile(config Config) (Config, *snapshot, error) {
	if !validMode(config.Mode) {
		return Config{}, nil, fmt.Errorf("invalid global mode %q", config.Mode)
	}
	if config.Mode == "" {
		config.Mode = Off
	}
	rules := make([]Rule, len(config.Rules))
	index := make(map[Tuple]Rule, len(rules))
	for i, rule := range config.Rules {
		if err := validateRule(rule, index); err != nil {
			return Config{}, nil, fmt.Errorf("rules[%d]: %w", i, err)
		}
		if rule.Mode == "" {
			rule.Mode = Off
		}
		rules[i] = rule
		index[rule.Tuple] = rule
	}
	config.Rules = rules
	return config, &snapshot{revision: config.Revision, mode: config.Mode, rules: index}, nil
}

// validateRule checks the values used for exact lookup and rejects duplicate
// identities, including duplicates that specify different rollout controls.
func validateRule(rule Rule, index map[Tuple]Rule) error {
	switch {
	case rule.EffectiveGroupID <= 0 || rule.ProviderID <= 0:
		return fmt.Errorf("effective_group_id and provider_id must be positive")
	case !literal(rule.UpstreamModel):
		return fmt.Errorf("upstream_model must be a literal model name")
	case !literal(rule.UpstreamEndpoint) || !strings.HasPrefix(rule.UpstreamEndpoint, "/") ||
		rule.UpstreamEndpoint == "/" || path.Clean(rule.UpstreamEndpoint) != rule.UpstreamEndpoint ||
		strings.ContainsAny(rule.UpstreamEndpoint, "#%\\"):
		return fmt.Errorf("upstream_endpoint must be a canonical absolute path")
	case !validMode(rule.Mode):
		return fmt.Errorf("invalid mode %q", rule.Mode)
	case rule.EnforcePercent < 0 || rule.EnforcePercent > rolloutBuckets:
		return fmt.Errorf("enforce_percent must be between 0 and %d", rolloutBuckets)
	}
	if _, exists := index[rule.Tuple]; exists {
		return fmt.Errorf("duplicate route tuple")
	}
	return nil
}

// literal accepts names without whitespace, control characters, or wildcards.
func literal(value string) bool {
	return value != "" && !strings.ContainsAny(value, "*?") &&
		strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

// validMode allows an omitted switch to take the off default.
func validMode(mode Mode) bool {
	return mode == "" || mode == Off || mode == Observe || mode == Enforce
}

// decide admits an exact enabled tuple into a rollout arm. The request ID is
// required for stable assignment and for the caller's logical-request accounting.
func (s *snapshot) decide(tuple Tuple, logicalRequestID string) Decision {
	if s == nil {
		return Decision{Mode: Off}
	}
	decision := Decision{Mode: Off, Revision: s.revision}
	rule, matched := s.rules[tuple]
	if !matched || !rule.SyncToStream || logicalRequestID == "" || s.mode == Off || rule.Mode == Off {
		return decision
	}
	decision.Mode = Observe
	if s.mode == Enforce && rule.Mode == Enforce && rolloutBucket(logicalRequestID) < rule.EnforcePercent {
		decision.Mode = Enforce
	}
	return decision
}

// rolloutBucket maps the same logical request ID to a stable bucket across
// processes, policy versions, and candidate changes. It hashes only the site ID
// so changing a rule or retrying a candidate cannot change that ID's bucket.
func rolloutBucket(logicalRequestID string) int {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(logicalRequestID))
	return int(hash.Sum64() % rolloutBuckets)
}
