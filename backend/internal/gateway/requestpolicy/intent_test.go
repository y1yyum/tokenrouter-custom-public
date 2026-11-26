package requestpolicy

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// intent_test.go checks exact scope, stable rollout, and original context
// ownership independently of the HTTP adapter or authority refresh cadence.

// TestIntentKeepsOriginalInputAfterShutdown starts a strict logical request and
// then disables the policy. Its original intent and revision survive a later
// attempt, while new requests follow the off setting.
func TestIntentKeepsOriginalInputAfterShutdown(t *testing.T) {
	service := New(nil)
	entry := Entry{EntryGroupID: 12, Endpoint: EndpointResponses, RequestedModel: "Original-X"}
	config, rules, err := compile(Config{Revision: 1, Mode: ModeStrict, Rules: []Rule{{Entry: entry, Mode: ModeStrict, StrictPercent: 100}}})
	require.NoError(t, err)
	require.NoError(t, service.publish(time.Now(), config, rules, true))
	raw := []byte(`{"model":"Original-X","temperature":0,"stream":false,"thinking":{"budget_tokens":null},"input":[{"a.b":true}]}`)
	for _, tc := range []struct {
		group int64  // entry group input
		path  string // entry path input
		id    string // site request ID
		body  []byte // client input body
	}{
		{0, EndpointResponses, "logical", raw},
		{13, EndpointResponses, "logical", raw},
		{12, EndpointChat, "logical", raw},
		{12, EndpointResponses, "", raw},
		{12, EndpointResponses, "logical", []byte(`{"model":"original-x"}`)},
	} {
		ctx, matched := service.Admit(t.Context(), tc.group, tc.path, tc.id, tc.body)
		require.False(t, matched)
		_, found := FromContext(ctx)
		require.False(t, found)
	}
	ctx, matched := service.Admit(t.Context(), entry.EntryGroupID, entry.Endpoint, "logical", raw)
	require.True(t, matched)
	require.NoError(t, service.publish(time.Now(), Config{Revision: 2, Mode: ModeOff}, nil, true))
	original := string(raw)
	clear(raw)
	ctx, matched = service.Admit(ctx, 13, EndpointMessages, "logical", []byte(`{"model":"mapped"}`))
	require.True(t, matched)
	intent, found := FromContext(ctx)
	require.True(t, found)
	require.Equal(t, ModeStrict, intent.Mode)
	require.EqualValues(t, 1, intent.Revision)
	require.Equal(t, entry.EntryGroupID, intent.EntryGroupID)
	require.Equal(t, entry.RequestedModel, intent.RequestedModel)
	require.Equal(t, original, intent.RawJSON())
	require.Equal(t, Value{OriginExplicit, "0"}, intent.Lookup("temperature"))
	require.Equal(t, Value{OriginExplicit, "false"}, intent.Lookup("stream"))
	require.Equal(t, Value{OriginExplicit, "null"}, intent.Lookup("thinking", "budget_tokens"))
	require.Equal(t, Value{OriginExplicit, "true"}, intent.Lookup("input", "0", "a.b"))
	require.Equal(t, Value{Origin: OriginAbsent}, intent.Lookup("top_p"))
	_, matched = service.Admit(t.Context(), entry.EntryGroupID, entry.Endpoint, "new", []byte(original))
	require.False(t, matched)
}

// TestParameterRolloutBucketSurvivesRevision compares two instances with
// different rollout percentages. It verifies a wider deployment adds strict
// requests without moving existing strict requests into the observe arm.
func TestParameterRolloutBucketSurvivesRevision(t *testing.T) {
	entry := Entry{EntryGroupID: 12, Endpoint: EndpointChat, RequestedModel: "original"}
	first, second := New(nil), New(nil)
	for index, service := range []*Service{first, second} {
		config, rules, err := compile(Config{
			Revision: int64(index + 1), Mode: ModeStrict,
			Rules: []Rule{{Entry: entry, Mode: ModeStrict, StrictPercent: 10 + 40*index}},
		})
		require.NoError(t, err)
		require.NoError(t, service.publish(time.Now(), config, rules, true))
	}
	var counts [2]int
	for index := range 1000 {
		id := fmt.Sprintf("logical-%d", index)
		wasStrict := false
		for arm, service := range []*Service{first, second} {
			ctx, ok := service.Admit(t.Context(), entry.EntryGroupID, entry.Endpoint, id, []byte(`{"model":"original"}`))
			require.True(t, ok)
			intent, _ := FromContext(ctx)
			strict := intent.Mode == ModeStrict
			if strict {
				counts[arm]++
			}
			if wasStrict {
				require.True(t, strict)
			}
			wasStrict = strict
		}
	}
	require.Positive(t, counts[0])
	require.Greater(t, counts[1], counts[0])
	require.Less(t, counts[1], 1000)
}
