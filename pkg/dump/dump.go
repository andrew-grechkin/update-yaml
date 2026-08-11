// Reflect-based helpers that turn arbitrary Go values - including the
// polymorphic goccy AST - into a JSON/YAML-serializable representation.
// Kept in its own package (rather than co-located with pkg/inspect's
// Trace* helpers) so consumers can pull in Inspect/DumpJson/DumpYaml
// without also pulling in the debug-tagged trace path. Release builds
// of the main tool link only pkg/inspect and see the trace stubs, so
// this package stays out of their link set; the cmd/probe-yaml binary
// depends on it directly and is compiled unconditionally.

package dump

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/goccy/go-yaml"
)

// Returns a pretty-printed JSON encoding of data.
//
// Marshal errors yield an empty string.
func DumpJson(data any) string {
	jsonBytes, _ := json.MarshalIndent(data, "", "  ")
	return string(jsonBytes)
}

// Returns a YAML encoding of data using single-quoted scalars and a
// 2-space indent.
//
// Marshal errors yield an empty string.
func DumpYaml(data any) string {
	yamlBytes, _ := yaml.MarshalWithOptions(
		data,
		yaml.UseLiteralStyleIfMultiline(false),
		yaml.UseSingleQuote(true),
		yaml.Indent(2),
	)
	return string(yamlBytes)
}

// Returns a JSON/YAML-serializable representation of data.
//
// A top-level struct is labeled as {TypeName: fields}; nested structs
// contribute their fields inline.
//
// Unsupported kinds (channel, func, ...) are stringified as a fallback.
func Inspect(data any) any {
	v := unwrap(reflect.ValueOf(data))
	if v.IsValid() && v.Kind() == reflect.Struct {
		return map[string]any{v.Type().Name(): inspectFields(v)}
	}
	return inspectValue(v)
}

// Returns the concrete value inside v, dereferencing chained pointers
// and unwrapping interface wrappers. Interface unwrapping is what lets
// Inspect walk an AST tree whose child fields are typed as ast.Node
// (an interface) - without it, the reflection walk stops at every
// polymorphic boundary and the stringify-fallback in inspectValue
// swallows the subtree.
//
// If v is or resolves to a nil pointer/interface, returns the zero
// reflect.Value.
func unwrap(v reflect.Value) reflect.Value {
	for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	return v
}

// Returns a JSON/YAML-serializable representation of v.
//
// Structs contribute their fields inline without a type-name wrapper.
func inspectValue(v reflect.Value) any {
	v = unwrap(v)
	if !v.IsValid() {
		return nil
	}
	switch v.Kind() {
	case reflect.Struct:
		return inspectFields(v)

	case reflect.Slice, reflect.Array:
		ret := make([]any, v.Len())
		for i := 0; i < v.Len(); i++ {
			ret[i] = inspectValue(v.Index(i))
		}
		return ret

	case reflect.String:
		return v.String()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint()
	case reflect.Float32, reflect.Float64:
		return v.Float()
	case reflect.Bool:
		return v.Bool()

	default:
		return fmt.Sprintf("%q", v.Interface())
	}
}

// Returns a map of the exported fields of the struct v, keyed by field
// name.
//
// The tokenizer's Next and Prev linked-list pointers are excluded to keep
// dumps scoped to the target node.
func inspectFields(v reflect.Value) map[string]any {
	fields := make(map[string]any)
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		if f.Name == "Next" || f.Name == "Prev" {
			continue
		}
		field := v.Field(i)
		if !field.CanInterface() {
			continue
		}
		fields[f.Name] = inspectValue(field)
	}
	return fields
}
