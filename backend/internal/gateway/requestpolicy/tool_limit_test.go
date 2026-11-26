package requestpolicy

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// tool_limit_test.go checks original, injected and inherited tool sources
// independently. Only a self-contained request with two known tool sets may
// omit the budget, and response restoration must preserve bare delta events.

// TestUnusedToolLimit distinguishes a known empty tool set from an unexpanded
// session, an injected provider tool and a request tool lost during preparation.
func TestUnusedToolLimit(t *testing.T) {
	const functions = `{"max_tool_calls":2,"tools":[{"type":"function","name":"local"}]}`
	for _, tc := range []struct {
		original, prepared string
		want               bool
	}{
		{functions, functions, true},
		{`{"max_tool_calls":2}`, `{}`, true},
		{functions, `{"tools":[{"type":"web_search"}]}`, false},
		{`{"max_tool_calls":2,"tools":[{"type":"file_search"}]}`, `{}`, false},
		{`{"max_tool_calls":2,"previous_response_id":"resp_1"}`, `{}`, false},
		{`{"max_tool_calls":2,"conversation":"conv_1"}`, `{}`, false},
		{functions, `{"prompt":{"id":"pmpt_1"}}`, false},
		{functions, `{"tools":[{"type":"unknown"}]}`, false},
		{functions, `{"input":[{"type":"additional_tools","tools":[{"type":"web_search"}]}]}`, false},
		{`{"max_tool_calls":2,"input":[{"type":"additional_tools","tools":[{"type":"web_search"}]}]}`, `{}`, false},
		{`{"max_tool_calls":"2"}`, `{}`, false},
		{`{"max_tool_calls":1.5}`, `{}`, false},
		{`{"max_tool_calls":-1}`, `{}`, false},
	} {
		value, ok := UnusedToolLimit(Intent{Mode: ModeStrict, Endpoint: "/v1/responses", raw: tc.original}, []byte(tc.prepared))
		require.Equal(t, tc.want, ok)
		if tc.want {
			require.EqualValues(t, 2, value)
		}
	}
}

// TestRestoreToolLimit covers synchronous JSON and the nested stream envelope.
func TestRestoreToolLimit(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{`{"object":"response","id":"r"}`, `{"object":"response","id":"r","max_tool_calls":2}`},
		{`{"type":"response.completed","response":{"id":"r"}}`, `{"type":"response.completed","response":{"id":"r","max_tool_calls":2}}`},
		{`{"type":"response.output_text.delta","delta":"hello"}`, `{"type":"response.output_text.delta","delta":"hello"}`},
	} {
		result, err := RestoreToolLimit([]byte(tc.input), 2)
		require.NoError(t, err)
		require.JSONEq(t, tc.want, string(result))
	}
}
