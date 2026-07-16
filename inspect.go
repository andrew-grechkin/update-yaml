//go:build debug

// Debug-only AST/token inspection helpers. Not compiled unless the binary is
// built with `-tags debug`; call sites in main.go that reference these
// functions must be gated the same way (or left commented out for ad-hoc
// enabling during a debug session).

package main

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/goccy/go-yaml"
)

func DumpJson(data any) string {
	jsonBytes, _ := json.MarshalIndent(data, "", "  ")
	return string(jsonBytes)
}

func DumpYaml(data any) string {
	yamlBytes, _ := yaml.MarshalWithOptions(
		data,
		yaml.UseLiteralStyleIfMultiline(false),
		yaml.UseSingleQuote(true),
		yaml.Indent(2),
	)
	return string(yamlBytes)
}

func Inspect(data any) any {
	value := reflect.ValueOf(data)

	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil
		}
		value = value.Elem()
	}

	switch value.Kind() {
	case reflect.Struct:
		ret := make(map[string]any)
		key := value.Type().Name()

		fields := make(map[string]any)
		for i := 0; i < value.NumField(); i++ {
			f := value.Type().Field(i)
			if f.Name == "Next" || f.Name == "Prev" {
				continue
			}

			if field := value.Field(i); field.CanInterface() {
				fields[f.Name] = Inspect(field.Interface())
			}
		}
		ret[key] = fields
		return ret

	case reflect.Slice, reflect.Array:
		var ret []any
		for i := 0; i < value.Len(); i++ {
			ret = append(ret, Inspect(value.Index(i).Interface()))
		}
		return ret

	case reflect.String:
		return value.String()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return value.Uint()
	case reflect.Float32, reflect.Float64:
		return value.Float()
	case reflect.Bool:
		return value.Bool()

	default:
		// Fallback for complex primitives (channels, funcs, etc.)
		return fmt.Sprintf("%q", data)
	}
}
