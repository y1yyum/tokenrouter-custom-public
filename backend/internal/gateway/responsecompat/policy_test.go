package responsecompat

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// policy_test.go checks exact candidate identity, conservative switches,
// validation, and stable assignment as the rollout percentage changes.

// TestCompatibilityPolicyMatchesOneTuple varies each key independently and
// checks the least permissive switch combination for a single Luna rule.
func TestCompatibilityPolicyMatchesOneTuple(t *testing.T) {
	tuple := Tuple{EffectiveGroupID: 28, ProviderID: 57, UpstreamModel: "gpt-6-luna", UpstreamEndpoint: "/v1/responses"}
	for _, tc := range []struct {
		name    string
		global  Mode
		rule    Mode
		optIn   bool
		percent int
		want    Mode
	}{
		{name: "default global off", rule: Enforce, optIn: true, percent: 100, want: Off},
		{name: "default rule off", global: Enforce, optIn: true, percent: 100, want: Off},
		{name: "global off", global: Off, rule: Enforce, optIn: true, percent: 100, want: Off},
		{name: "rule off", global: Enforce, rule: Off, optIn: true, percent: 100, want: Off},
		{name: "rule opt in absent", global: Enforce, rule: Enforce, percent: 100, want: Off},
		{name: "global observe", global: Observe, rule: Enforce, optIn: true, percent: 100, want: Observe},
		{name: "rule observe", global: Enforce, rule: Observe, optIn: true, percent: 100, want: Observe},
		{name: "zero rollout", global: Enforce, rule: Enforce, optIn: true, want: Observe},
		{name: "full rollout", global: Enforce, rule: Enforce, optIn: true, percent: 100, want: Enforce},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, snapshot, err := compile(Config{Revision: 3, Mode: tc.global, Rules: []Rule{
				{Tuple: tuple, Mode: tc.rule, SyncToStream: tc.optIn, EnforcePercent: tc.percent},
			}})
			require.NoError(t, err)
			require.Equal(t, Decision{Mode: tc.want, Revision: 3}, snapshot.decide(tuple, "logical-0"))
			require.Equal(t, Decision{Mode: Off, Revision: 3}, snapshot.decide(tuple, ""))
			for _, other := range []Tuple{
				{EffectiveGroupID: 29, ProviderID: 57, UpstreamModel: "gpt-6-luna", UpstreamEndpoint: "/v1/responses"},
				{EffectiveGroupID: 28, ProviderID: 58, UpstreamModel: "gpt-6-luna", UpstreamEndpoint: "/v1/responses"},
				{EffectiveGroupID: 28, ProviderID: 57, UpstreamModel: "GPT-6-LUNA", UpstreamEndpoint: "/v1/responses"},
				{EffectiveGroupID: 28, ProviderID: 57, UpstreamModel: "gpt-6-luna", UpstreamEndpoint: "/other/v1/responses"},
			} {
				require.Equal(t, Decision{Mode: Off, Revision: 3}, snapshot.decide(other, "logical-0"))
			}
		})
	}
}

// TestCompatibilityPolicyStableBucket fixes representative request placements
// so unrelated changes to the hash cannot silently move the rollout cohort.
func TestCompatibilityPolicyStableBucket(t *testing.T) {
	tuple := Tuple{EffectiveGroupID: 28, ProviderID: 57, UpstreamModel: "gpt-6-luna", UpstreamEndpoint: "/v1/responses"}
	for i, percent := range []int{10, 10, 50, 100} {
		_, snapshot, err := compile(Config{Revision: int64(i + 1), Mode: Enforce, Rules: []Rule{
			{Tuple: tuple, Mode: Enforce, SyncToStream: true, EnforcePercent: percent},
		}})
		require.NoError(t, err)
		for _, tc := range []struct {
			id     string
			bucket int
		}{
			{id: "logical-0", bucket: 29},
			{id: "logical-2", bucket: 7},
			{id: "logical-3", bucket: 96},
		} {
			require.Equal(t, tc.bucket, rolloutBucket(tc.id))
			want := Observe
			if tc.bucket < percent {
				want = Enforce
			}
			require.Equal(t, Decision{Mode: want, Revision: int64(i + 1)}, snapshot.decide(tuple, tc.id))
		}
	}
}

// TestCompatibilityPolicyRejectsInvalidRules checks disabled policies as well
// as enabled ones so an administrator cannot save a malformed rule for later use.
func TestCompatibilityPolicyRejectsInvalidRules(t *testing.T) {
	tuple := Tuple{EffectiveGroupID: 28, ProviderID: 57, UpstreamModel: "gpt-6-luna", UpstreamEndpoint: "/v1/responses"}
	for _, tc := range []struct {
		field  string
		change func(*Rule)
	}{
		{field: "effective_group_id", change: func(rule *Rule) { rule.EffectiveGroupID = 0 }},
		{field: "provider_id", change: func(rule *Rule) { rule.ProviderID = -1 }},
		{field: "upstream_model", change: func(rule *Rule) { rule.UpstreamModel = "gpt-*" }},
		{field: "upstream_model", change: func(rule *Rule) { rule.UpstreamModel = " gpt-6-luna" }},
		{field: "upstream_endpoint", change: func(rule *Rule) { rule.UpstreamEndpoint = "https://example.invalid/v1/responses" }},
		{field: "upstream_endpoint", change: func(rule *Rule) { rule.UpstreamEndpoint = "/v1/responses?x=y" }},
		{field: "upstream_endpoint", change: func(rule *Rule) { rule.UpstreamEndpoint = "/v1//responses" }},
		{field: "mode", change: func(rule *Rule) { rule.Mode = "on" }},
		{field: "enforce_percent", change: func(rule *Rule) { rule.EnforcePercent = -1 }},
		{field: "enforce_percent", change: func(rule *Rule) { rule.EnforcePercent = 101 }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			rule := Rule{Tuple: tuple}
			tc.change(&rule)
			_, _, err := compile(Config{Mode: Off, Rules: []Rule{rule}})
			require.ErrorContains(t, err, tc.field)
		})
	}
	_, _, err := compile(Config{Mode: "on"})
	require.ErrorContains(t, err, "global mode")
	_, _, err = compile(Config{Mode: Off, Rules: []Rule{{Tuple: tuple}, {Tuple: tuple, Mode: Enforce}}})
	require.ErrorContains(t, err, "duplicate route tuple")
}
