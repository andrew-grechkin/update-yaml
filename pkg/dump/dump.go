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
// Every struct is labeled as {TypeName: fields} EXCEPT when it sits inside a field whose name already matches the
// struct's type - that would produce redundant `Token: {Token: {...}}` shapes. The rule surfaces polymorphic slots
// (an interface-typed field's concrete type shows up in the output) without cluttering concrete typed fields where
// the type is already obvious from the field name.
//
// Unsupported kinds (channel, func, ...) are stringified as a fallback.
func Inspect(data any) any {
	return inspectValue(reflect.ValueOf(data), "")
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

// Returns a JSON/YAML-serializable representation of v. fieldName is the name of the field that holds this value in
// its parent struct (empty for top-level and for slice elements). When a struct's type name equals fieldName, the
// {TypeName: ...} wrap is omitted - the label would be redundant with the field key that already reads the same.
func inspectValue(v reflect.Value, fieldName string) any {
	v = unwrap(v)
	if !v.IsValid() {
		return nil
	}
	switch v.Kind() {
	case reflect.Struct:
		typeName := v.Type().Name()
		fields := inspectFields(v)
		if typeName == fieldName {
			return fields
		}
		return map[string]any{typeName: fields}

	case reflect.Slice, reflect.Array:
		ret := make([]any, v.Len())
		for i := 0; i < v.Len(); i++ {
			ret[i] = inspectValue(v.Index(i), "")
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

// Returns a map of the exported fields of the struct v, keyed by field name. Passes each field's name down to
// inspectValue so nested structs can decide whether to elide their own type-name wrapper.
//
// The tokenizer's Next and Prev linked-list pointers are excluded to keep dumps scoped to the target node.
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
		fields[f.Name] = inspectValue(field, f.Name)
	}
	return fields
}
