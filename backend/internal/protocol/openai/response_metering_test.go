package openai

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// TestResponsesMeteringCombinesSparseAccounting verifies the provenance and
// normalized counters of one observation. Later sparse fields fill without
// adding cumulative reports twice or replacing cached and image buckets.
func TestResponsesMeteringCombinesSparseAccounting(t *testing.T) {
	earlier := ParseResponsesMetering([]byte(`{"usage":{"prompt_tokens":11,"input_tokens_details":{"cached_tokens":3,"image_tokens":2},"cache_write_tokens":4}}`))
	terminal := ParseResponsesMetering([]byte(`{"usage":{"output_tokens":6},"response":{"usage":{"output_tokens_details":{"image_tokens":1}}}}`))
	selected, usedEarlier, usedTerminal := OverlayResponsesMetering(earlier, terminal)
	require.True(t, usedEarlier)
	require.True(t, usedTerminal)
	usage, seen, valid := selected.Result()
	require.True(t, seen)
	require.True(t, valid)
	require.Equal(t, 11, usage.InputTokens)
	require.Equal(t, 6, usage.OutputTokens)
	require.Equal(t, 3, usage.CacheReadInputTokens)
	require.Equal(t, 4, usage.CacheCreationInputTokens)
	require.Equal(t, 2, usage.ImageInputTokens)
	require.Equal(t, 1, usage.ImageOutputTokens)
	body, err := selected.Patch([]byte(`{"usage":{"vendor_counter":99}}`))
	require.NoError(t, err)
	require.EqualValues(t, 99, gjson.GetBytes(body, "usage.vendor_counter").Int())
	require.EqualValues(t, 11, gjson.GetBytes(body, "usage.input_tokens").Int())
}

// TestResponsesMeteringReconcilesSameFrameViews covers agreement, sparse
// counters, and zero placeholders within a frame. It verifies that differing
// positive aliases are untrusted even when a preferred spelling is zero.
func TestResponsesMeteringReconcilesSameFrameViews(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		conflicting   bool
	}{
		{"equal", `{"usage":{"prompt_tokens":7,"completion_tokens":2},"response":{"usage":{"input_tokens":7,"output_tokens":2}}}`, false},
		{"sparse", `{"usage":{"input_tokens":7},"response":{"usage":{"output_tokens":2}}}`, false},
		{"outer-zero", `{"usage":{"input_tokens":0,"output_tokens":0},"response":{"usage":{"input_tokens":7,"output_tokens":2}}}`, false},
		{"inner-zero", `{"usage":{"input_tokens":7,"output_tokens":2},"response":{"usage":{"input_tokens":0,"output_tokens":0}}}`, false},
		{"alias-zero-fallback", `{"usage":{"input_tokens":0,"prompt_tokens":7,"output_tokens":0,"completion_tokens":2}}`, false},
		{"envelope-conflict", `{"usage":{"input_tokens":7,"output_tokens":2},"response":{"usage":{"input_tokens":70,"output_tokens":2}}}`, true},
		{"partial-zero-conflict", `{"usage":{"input_tokens":0,"output_tokens":2},"response":{"usage":{"input_tokens":7,"output_tokens":2}}}`, true},
		{"input-alias-conflict", `{"usage":{"input_tokens":7,"prompt_tokens":70,"output_tokens":2}}`, true},
		{"output-alias-conflict", `{"usage":{"input_tokens":7,"output_tokens":2,"completion_tokens":20}}`, true},
		{"cache-alias-conflict-behind-zero", `{"usage":{"input_tokens":7,"output_tokens":2,"input_tokens_details":{"cached_tokens":0},"cache_read_tokens":2,"cached_tokens":3}}`, true},
		{"hosted-image-alias-conflict", `{"usage":{"input_tokens":7,"output_tokens":2,"input_tokens_details":{"image_tokens":2}},"tool_usage":{"image_gen":{"input_tokens_details":{"image_tokens":3}}}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := ParseResponsesMetering([]byte(tc.payload))
			require.Equal(t, tc.conflicting, frame.Conflicting())
			usage, seen, valid := frame.Result()
			require.True(t, seen)
			require.Equal(t, !tc.conflicting, valid)
			if valid {
				require.Equal(t, 7, usage.InputTokens)
				require.Equal(t, 2, usage.OutputTokens)
			}
		})
	}
}

// TestResponsesMeteringRejectsUnverifiedCounts distinguishes complete zero from
// missing and corrupt counters. It includes disagreement within an authoritative
// pair and a progressive decrease that cannot prove a stable billable count.
func TestResponsesMeteringRejectsUnverifiedCounts(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		seen, valid   bool
	}{
		{"missing", `{"response":{"usage":null}}`, false, false},
		{"empty", `{"usage":{}}`, true, false},
		{"sparse-zero", `{"usage":{"input_tokens":0}}`, true, false},
		{"complete-zero", `{"usage":{"input_tokens":0,"output_tokens":0}}`, true, true},
		{"negative", `{"usage":{"input_tokens":1,"output_tokens_details":{"image_tokens":-1}}}`, true, false},
		{"coerced", `{"usage":{"input_tokens":"1"}}`, true, false},
		{"decimal", `{"usage":{"output_tokens":1.5}}`, true, false},
		{"overflow", `{"usage":{"input_tokens":9999999999999999999999}}`, true, false},
		{"duplicate", `{"usage":{"input_tokens":7,"input_tokens":8}}`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, seen, valid := ParseResponsesMetering([]byte(tc.payload)).Result()
			require.Equal(t, tc.seen, seen)
			require.Equal(t, tc.valid, valid)
		})
	}
	for _, authoritative := range []bool{false, true} {
		left := ParseResponsesMetering([]byte(`{"usage":{"input_tokens":7,"output_tokens":2}}`))
		placeholder := ParseResponsesMetering([]byte(`{"usage":{"input_tokens":0,"output_tokens":0}}`))
		require.NoError(t, left.Merge(placeholder, authoritative))
		usage, _, valid := left.Result()
		require.True(t, valid)
		require.Equal(t, 7, usage.InputTokens)
		conflict := ParseResponsesMetering([]byte(`{"usage":{"input_tokens":6}}`))
		require.Error(t, left.Merge(conflict, authoritative))
		_, _, valid = left.Result()
		require.False(t, valid)
	}
}
