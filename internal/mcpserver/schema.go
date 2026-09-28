package mcpserver

import (
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
)

// This file gives a tool's Go input struct an explicit InputSchema when the
// struct tags alone cannot express a constraint, following the same
// approach freecad-mcp/internal/mcpserver/schema.go uses. Every other tool
// in this package leaves InputSchema nil and lets mcp.AddTool infer it from
// the struct's own json/jsonschema tags (registry.go); only set_fan_speed
// (tools_control.go) needs the extra enum on its fan property, so this file
// stays deliberately small rather than importing freecad-mcp's fuller helper
// set wholesale.

// inputSchema infers T's JSON schema by reflection, the same schema
// mcp.AddTool would build automatically for a nil InputSchema.
func inputSchema[T any]() *jsonschema.Schema {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		panic(err)
	}
	return s
}

// withEnum restricts the named string property of s to values, dropping the
// inferred "null" alternative an optional (pointer) property gets, since the
// property is only ever one of values, never JSON null, once its enum is
// set.
func withEnum(s *jsonschema.Schema, name string, values ...string) *jsonschema.Schema {
	prop, ok := s.Properties[name]
	if !ok {
		panic(fmt.Sprintf("withEnum: no property %q", name))
	}
	enum := make([]any, len(values))
	for i, v := range values {
		enum[i] = v
	}
	dropNullAlt(prop)
	prop.Enum = enum
	return s
}

// dropNullAlt removes prop's inferred "null" alternative, a no-op when it
// has none: an optional string property built from a Go pointer type infers
// ["null", "string"], but once an enum is set the property is only ever
// present with a real value, never JSON null.
func dropNullAlt(prop *jsonschema.Schema) {
	if len(prop.Types) == 2 && prop.Types[0] == "null" {
		prop.Type, prop.Types = prop.Types[1], nil
	}
}
