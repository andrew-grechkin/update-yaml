package ast

import (
	"slices"
	"testing"

	yamlast "github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	"github.com/goccy/go-yaml/token"
)

func parseFirstMV(t *testing.T, src string) *yamlast.MappingValueNode {
	t.Helper()
	f, err := parser.ParseBytes([]byte(src), parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	mn, ok := f.Docs[0].Body.(*yamlast.MappingNode)
	if !ok {
		t.Fatalf("body is %T, want MappingNode", f.Docs[0].Body)
	}
	return mn.Values[0]
}

func TestIsNullNode(t *testing.T) {
	null := parseFirstMV(t, "key: null\n").Value
	notNull := parseFirstMV(t, "key: value\n").Value
	if !IsNullNode(null) {
		t.Errorf("IsNullNode(null) = false, want true")
	}
	if IsNullNode(notNull) {
		t.Errorf("IsNullNode(non-null) = true, want false")
	}
}

func TestKeyString(t *testing.T) {
	cases := map[string]string{
		"foo: 1\n":        "foo",
		`"has space": 1`:  "has space",
		"'single': 1":     "single",
		"42: 1":           "42",
	}
	for src, want := range cases {
		got := KeyString(parseFirstMV(t, src).Key)
		if got != want {
			t.Errorf("KeyString(%q) = %q, want %q", src, got, want)
		}
	}
}

func TestIsSimpleIdent(t *testing.T) {
	cases := map[string]bool{
		"foo":        true,
		"foo-bar":    true,
		"foo_bar":    true,
		"foo123":     true,
		"123foo":     false, // digit at start
		"":           false,
		"has space":  false,
		"foo.bar":    false,
		"foo/bar":    false,
	}
	for in, want := range cases {
		if got := IsSimpleIdent(in); got != want {
			t.Errorf("IsSimpleIdent(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestJoinPath(t *testing.T) {
	cases := []struct {
		prefix, key, want string
	}{
		{"$", "foo", "$.foo"},
		{"$.foo", "bar", "$.foo.bar"},
		{"$", "has.dot", "$.'has.dot'"},
		{"$.foo", "with'quote", "$.foo.'with\\'quote'"},
		{"$.foo", `back\slash`, `$.foo.'back\\slash'`},
	}
	for _, c := range cases {
		if got := JoinPath(c.prefix, c.key); got != c.want {
			t.Errorf("JoinPath(%q, %q) = %q, want %q", c.prefix, c.key, got, c.want)
		}
	}
}

func TestIsExplicitQuote(t *testing.T) {
	if !IsExplicitQuote(token.SingleQuoteType) {
		t.Errorf("single quote should be explicit")
	}
	if !IsExplicitQuote(token.DoubleQuoteType) {
		t.Errorf("double quote should be explicit")
	}
	if IsExplicitQuote(token.StringType) {
		t.Errorf("plain string should not be explicit")
	}
}

func TestWalkPreOrder(t *testing.T) {
	f, _ := parser.ParseBytes([]byte("root:\n  a: 1\n  b:\n    c: 2\n"), parser.ParseComments)
	var keys []string
	Walk(f.Docs[0].Body, func(n yamlast.Node) bool {
		if s, ok := n.(*yamlast.StringNode); ok {
			keys = append(keys, s.Value)
		}
		return true
	})
	// Filter to string keys only (goccy parses "1"/"2" as IntegerNode).
	var keyOrder []string
	for _, k := range keys {
		if k != "1" && k != "2" {
			keyOrder = append(keyOrder, k)
		}
	}
	want := []string{"root", "a", "b", "c"}
	if len(keyOrder) != len(want) {
		t.Fatalf("Walk visits: got %q, want %q", keyOrder, want)
	}
	for i, w := range want {
		if keyOrder[i] != w {
			t.Errorf("Walk visits[%d] = %q, want %q", i, keyOrder[i], w)
		}
	}
}

func TestWalkSkipSubtree(t *testing.T) {
	// Returning false at the MappingValueNode whose key is "b" should
	// stop Walk from descending into b's value (which contains "c").
	// "a" is a sibling and must still be visited.
	f, _ := parser.ParseBytes([]byte("root:\n  a: 1\n  b:\n    c: 2\n"), parser.ParseComments)
	var reached []string
	Walk(f.Docs[0].Body, func(n yamlast.Node) bool {
		if mv, ok := n.(*yamlast.MappingValueNode); ok && KeyString(mv.Key) == "b" {
			return false
		}
		if s, ok := n.(*yamlast.StringNode); ok {
			reached = append(reached, s.Value)
		}
		return true
	})
	for _, v := range reached {
		if v == "c" {
			t.Errorf("Walk should have skipped 'c' when b's MV returned false; reached %q", reached)
		}
	}
	if !slices.Contains(reached, "a") {
		t.Errorf("Walk should still visit sibling 'a'; reached %q", reached)
	}
}

func TestReplaceLeavesSwapsValueSlot(t *testing.T) {
	f, _ := parser.ParseBytes([]byte("outer:\n  target: keep\n  other: 1\n"), parser.ParseComments)
	replacement, _ := parser.ParseBytes([]byte("replaced\n"), parser.ParseComments)
	newValue := replacement.Docs[0].Body

	ReplaceLeaves(f.Docs[0].Body, func(n yamlast.Node) yamlast.Node {
		if s, ok := n.(*yamlast.StringNode); ok && s.Value == "keep" {
			return newValue
		}
		return nil
	})

	got := f.String()
	if got == "" || !contains(got, "replaced") {
		t.Errorf("ReplaceLeaves did not swap: output = %q", got)
	}
	if contains(got, "keep") {
		t.Errorf("ReplaceLeaves left old value in place: %q", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestUnwrapAnchor(t *testing.T) {
	// Anchored value: returns the inner, not the AnchorNode wrapper.
	mv := parseFirstMV(t, "key: &anchor value\n")
	inner := UnwrapAnchor(mv.Value)
	if _, ok := inner.(*yamlast.AnchorNode); ok {
		t.Errorf("UnwrapAnchor returned an AnchorNode, want the inner value")
	}
	if _, ok := inner.(*yamlast.StringNode); !ok {
		t.Errorf("UnwrapAnchor should return the StringNode inside; got %T", inner)
	}

	// Non-anchor: passthrough, same reference.
	mvPlain := parseFirstMV(t, "key: value\n")
	if UnwrapAnchor(mvPlain.Value) != mvPlain.Value {
		t.Errorf("UnwrapAnchor on non-anchor should return the node verbatim")
	}
}
