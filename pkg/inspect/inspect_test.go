//go:build debug

package inspect

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/goccy/go-yaml/parser"
)

func TestDumpJson(t *testing.T) {
	got := DumpJson(map[string]any{"a": 1, "b": []int{2, 3}})
	// Round-trip parse to compare structurally (map ordering isn't stable).
	var back map[string]any
	if err := json.Unmarshal([]byte(got), &back); err != nil {
		t.Fatalf("DumpJson emitted invalid JSON: %v\noutput: %s", err, got)
	}
	if back["a"].(float64) != 1 {
		t.Errorf("expected a=1, got %v", back["a"])
	}
	if !strings.Contains(got, "  ") {
		t.Errorf("expected 2-space indent in pretty JSON, got %q", got)
	}
}

func TestDumpJsonMarshalError(t *testing.T) {
	// A channel can't be marshalled - MarshalIndent returns err, our
	// swallow contract yields "".
	got := DumpJson(make(chan int))
	if got != "" {
		t.Errorf("expected empty string on marshal error, got %q", got)
	}
}

func TestDumpYaml(t *testing.T) {
	got := DumpYaml(map[string]string{"key": "value"})
	if !strings.Contains(got, "key: ") {
		t.Errorf("expected YAML mapping output, got %q", got)
	}
}

func TestInspectStructWrapsWithTypeName(t *testing.T) {
	type Point struct{ X, Y int }
	got := Inspect(Point{X: 3, Y: 4})
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("expected map output, got %T", got)
	}
	fields, ok := m["Point"].(map[string]any)
	if !ok {
		t.Fatalf("expected top-level {Point: ...}, got %v", m)
	}
	if fields["X"] != int64(3) || fields["Y"] != int64(4) {
		t.Errorf("expected X=3 Y=4, got %v", fields)
	}
}

func TestInspectNestedStructNoDoubleWrap(t *testing.T) {
	// This is the bug we fixed earlier: nested struct fields whose type
	// name matches the field name should NOT be re-wrapped.
	type Inner struct{ V int }
	type Outer struct{ Inner Inner }
	got := Inspect(Outer{Inner: Inner{V: 7}})
	// Expect: {Outer: {Inner: {V: 7}}} - nested Inner is a plain map,
	// not a nested {Inner: {Inner: {...}}}.
	outer := got.(map[string]any)["Outer"].(map[string]any)
	inner := outer["Inner"].(map[string]any)
	if _, wrapped := inner["Inner"]; wrapped {
		t.Errorf("nested struct was double-wrapped: %v", got)
	}
	if inner["V"] != int64(7) {
		t.Errorf("expected V=7, got %v", inner)
	}
}

func TestInspectNilPointer(t *testing.T) {
	var p *struct{ X int }
	if got := Inspect(p); got != nil {
		t.Errorf("expected nil for nil pointer input, got %v", got)
	}
}

func TestInspectScalar(t *testing.T) {
	if got := Inspect("hello"); got != "hello" {
		t.Errorf("expected string passthrough, got %v", got)
	}
	if got := Inspect(42); got != int64(42) {
		t.Errorf("expected int64 for int input, got %v (%T)", got, got)
	}
	if got := Inspect(true); got != true {
		t.Errorf("expected bool passthrough, got %v", got)
	}
}

func TestInspectYAMLNode(t *testing.T) {
	// Verify Inspect works on a real goccy AST node (the primary
	// use case) without panicking; check the outer wrapper shape.
	f, _ := parser.ParseBytes([]byte("key: value\n"), parser.ParseComments)
	got := Inspect(f.Docs[0].Body)
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("expected map output, got %T", got)
	}
	// Body is *ast.MappingNode - after unwrap it's MappingNode struct.
	if _, ok := m["MappingNode"]; !ok {
		t.Errorf("expected top-level {MappingNode: ...}, got %v", m)
	}
}
