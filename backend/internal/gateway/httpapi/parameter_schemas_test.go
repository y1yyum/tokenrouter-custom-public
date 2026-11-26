package httpapi

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/TokenFlux/TokenRouter/internal/gateway/requestpolicy"
)

// parameter_schemas_test.go checks locally provable failures and preserves legitimate
// client JSON Schema features that do not need provider-specific rewriting.

// TestParameterSchemaPaths distinguishes missing local references from remote
// references and verifies that contradictions point to the client's original
// schema field. Literal example objects remain user data even when they use
// names that also appear in the schema vocabulary.
func TestParameterSchemaPaths(t *testing.T) {
	const path = "tools[2].function.parameters"
	for _, tc := range []struct {
		name   string
		schema string
		want   string
	}{
		{"local", `{"type":"object","properties":{"x":{"$ref":"#/$defs/t"}},"$defs":{"t":{"type":"string"}}}`, ""},
		{"missing-local", `{"type":"object","properties":{"x":{"$ref":"#/$defs/gone"}}}`, path},
		{"external", `{"type":"object","properties":{"x":{"$ref":"https://example.invalid/schema"}}}`, ""},
		{"closed-required", `{"type":"object","properties":{"x":{"type":"object","required":["y"],"additionalProperties":false}}}`, path + `.properties["x"].required[0]`},
		{"open-required", `{"type":"object","required":["y"]}`, ""},
		{"pattern", `{"type":"object","patternProperties":{"^y$":{"type":"string"}},"additionalProperties":false,"required":["y"]}`, ""},
		{"invalid-type", `{"properties":{"x":{"type":null}}}`, path + `.properties["x"].type`},
		{"literal-example", `{"type":"object","examples":[{"type":null,"required":["y"]}]}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateParameterSchema(gjson.Parse(tc.schema), path)
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// TestParameterSchemaAdmission names each inbound schema shape and confirms
// that observation of the identical input cannot reject the client's request.
func TestParameterSchemaAdmission(t *testing.T) {
	for _, tc := range []struct{ endpoint, body, path string }{
		{"/v1/messages", `{"tools":[{"name":"lookup","input_schema":{"properties":{"x":{"$ref":"#/$defs/missing"}}}}]}`, "tools[0].input_schema"},
		{"/v1/chat/completions", `{"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":null}}}]}`, "tools[0].function.parameters.type"},
		{"/v1/responses", `{"tools":[{"type":"function","name":"lookup","parameters":5}]}`, "tools[0].parameters"},
		{"/v1/chat/completions", `{"response_format":{"type":"json_schema","json_schema":{"name":"answer"}}}`, "response_format.json_schema.schema"},
		{"/v1/responses", `{"text":{"format":{"type":"json_schema","schema":{"type":7}}}}`, "text.format.schema.type"},
	} {
		require.NoError(t, validateParameterSchemas(requestpolicy.ModeObserve, tc.endpoint, tc.body))
		require.ErrorContains(t, validateParameterSchemas(requestpolicy.ModeStrict, tc.endpoint, tc.body), tc.path)
	}
}
