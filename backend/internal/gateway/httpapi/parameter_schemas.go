package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/tidwall/gjson"

	"github.com/TokenFlux/TokenRouter/internal/gateway/requestpolicy"
)

// parameter_schemas.go checks client-declared response and function schemas before a
// strict request can reach a provider. The field path comes from the original
// protocol. Local references resolve within their own schema and remote
// references never trigger network requests from the gateway.

var errExternalSchema = errors.New("external schema is not available locally")

// validateParameterSchemas returns the first demonstrable schema error in original
// request order. Schemas absent from optional function declarations retain
// their provider defaults. Observation mode does not change the client result.
func validateParameterSchemas(mode requestpolicy.Mode, endpoint string, raw string) error {
	if mode != requestpolicy.ModeStrict {
		return nil
	}
	body := gjson.Parse(raw)
	switch endpoint {
	case "/v1/responses":
		if body.Get("text.format.type").String() == "json_schema" {
			if err := validateParameterSchema(body.Get("text.format.schema"), "text.format.schema"); err != nil {
				return err
			}
		}
	case "/v1/chat/completions":
		if body.Get("response_format.type").String() == "json_schema" {
			if err := validateParameterSchema(body.Get("response_format.json_schema.schema"), "response_format.json_schema.schema"); err != nil {
				return err
			}
		}
	}
	for index, tool := range body.Get("tools").Array() {
		var field string
		switch endpoint {
		case "/v1/messages":
			field = "input_schema"
		case "/v1/chat/completions":
			if tool.Get("type").String() == "function" {
				field = "function.parameters"
			}
		case "/v1/responses":
			if tool.Get("type").String() == "function" {
				field = "parameters"
			}
		}
		if field == "" {
			continue
		}
		if schema := tool.Get(field); schema.Exists() {
			if err := validateParameterSchema(schema, fmt.Sprintf("tools[%d].%s", index, field)); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateParameterSchema checks syntax, local reference resolution and closed
// object contradictions. An unresolved external document has no local verdict;
// callers preserve the original declaration for the upstream's own validation.
func validateParameterSchema(raw gjson.Result, path string) error {
	if !raw.IsObject() {
		return fmt.Errorf("%s: expected a schema object", path)
	}
	if err := checkClosedSchemaObjects(raw, path); err != nil {
		return err
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal([]byte(raw.Raw), &schema); err != nil {
		return fmt.Errorf("%s: invalid schema: %w", path, err)
	}
	_, err := schema.Resolve(&jsonschema.ResolveOptions{
		Loader: func(*url.URL) (*jsonschema.Schema, error) { return nil, errExternalSchema },
	})
	if err != nil && !errors.Is(err, errExternalSchema) {
		return fmt.Errorf("%s: invalid schema: %w", path, err)
	}
	return nil
}

// checkClosedSchemaObjects finds required names forbidden by the same object's
// property rules. Only schema-bearing keywords are traversed; example data,
// default values and literal objects in an enum are never treated as schemas.
func checkClosedSchemaObjects(node gjson.Result, path string) error {
	kind := node.Get("type")
	if kind.Exists() && (kind.Type != gjson.String && !kind.IsArray() || kind.IsArray() && len(kind.Array()) == 0) {
		return fmt.Errorf("%s.type: expected a type name or nonempty list of types", path)
	}
	additional := node.Get("additionalProperties")
	if kind.String() == "object" && additional.Type == gjson.False && len(node.Get("patternProperties").Map()) == 0 {
		properties := node.Get("properties").Map()
		for index, required := range node.Get("required").Array() {
			if _, ok := properties[required.String()]; !ok && required.Type == gjson.String {
				return fmt.Errorf("%s.required[%d]: property %q is forbidden by this closed object", path, index, required.String())
			}
		}
	}
	var first error
	node.ForEach(func(key, child gjson.Result) bool {
		name := key.String()
		childPath := path + "." + name
		switch name {
		case "$defs", "definitions", "properties", "patternProperties", "dependentSchemas":
			child.ForEach(func(property, nested gjson.Result) bool {
				first = checkClosedSchemaObjects(nested, childPath+"["+strconv.Quote(property.String())+"]")
				return first == nil
			})
		case "allOf", "anyOf", "oneOf", "prefixItems":
			for i, nested := range child.Array() {
				if first = checkClosedSchemaObjects(nested, fmt.Sprintf("%s[%d]", childPath, i)); first != nil {
					break
				}
			}
		case "items", "contains", "if", "then", "else", "not", "additionalProperties", "propertyNames":
			if child.IsObject() {
				first = checkClosedSchemaObjects(child, childPath)
			}
		}
		return first == nil
	})
	return first
}
