package transform

import (
	"strings"
	"testing"

	yamlast "github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

// ApplyMergedDocs no-ops when mergedDocs is empty.
func TestApplyMergedDocsEmptyData(t *testing.T) {
	src := []byte("keep: this\n")
	f, err := parser.ParseBytes(src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err := ApplyMergedDocs(f, nil); err != nil {
		t.Fatalf("ApplyMergedDocs: %v", err)
	}
	if !strings.Contains(f.String(), "keep: this") {
		t.Errorf("source content lost: %q", f.String())
	}
}

// Basic update path: data replaces an existing scalar.
func TestApplyMergedDocsReplacesScalar(t *testing.T) {
	src := []byte("k: old\n")
	sf, _ := parser.ParseBytes(src, parser.ParseComments)
	df, _ := parser.ParseBytes([]byte("k: new\n"), parser.ParseComments)

	if err := ApplyMergedDocs(sf, df.Docs); err != nil {
		t.Fatalf("ApplyMergedDocs: %v", err)
	}
	out := sf.String()
	if !strings.Contains(out, "k: new") {
		t.Errorf("value not updated; got %q", out)
	}
	if strings.Contains(out, "old") {
		t.Errorf("old value not replaced; got %q", out)
	}
}

// Data null on an existing key removes it.
func TestApplyMergedDocsNullRemovesKey(t *testing.T) {
	sf, _ := parser.ParseBytes([]byte("gone: 1\nkept: 2\n"), parser.ParseComments)
	df, _ := parser.ParseBytes([]byte("gone: null\n"), parser.ParseComments)

	if err := ApplyMergedDocs(sf, df.Docs); err != nil {
		t.Fatalf("ApplyMergedDocs: %v", err)
	}
	out := sf.String()
	if strings.Contains(out, "gone:") {
		t.Errorf("null-marked key not removed; got %q", out)
	}
	if !strings.Contains(out, "kept: 2") {
		t.Errorf("sibling key lost; got %q", out)
	}
}

// New key in data with source not touching it - appended.
func TestApplyMergedDocsAppendsNewKey(t *testing.T) {
	sf, _ := parser.ParseBytes([]byte("existing: 1\n"), parser.ParseComments)
	df, _ := parser.ParseBytes([]byte("appended: 2\n"), parser.ParseComments)

	if err := ApplyMergedDocs(sf, df.Docs); err != nil {
		t.Fatalf("ApplyMergedDocs: %v", err)
	}
	out := sf.String()
	if !strings.Contains(out, "existing: 1") || !strings.Contains(out, "appended: 2") {
		t.Errorf("append did not preserve both keys; got %q", out)
	}
}

// Empty source with non-empty data: source gets a fresh mapping body.
func TestApplyMergedDocsEmptySourceGetsData(t *testing.T) {
	sf, _ := parser.ParseBytes([]byte(""), parser.ParseComments)
	df, _ := parser.ParseBytes([]byte("k: v\n"), parser.ParseComments)

	if err := ApplyMergedDocs(sf, df.Docs); err != nil {
		t.Fatalf("ApplyMergedDocs: %v", err)
	}
	out := sf.String()
	if !strings.Contains(out, "k: v") {
		t.Errorf("empty source did not receive data; got %q", out)
	}
}

// Data doc that maps to nothing (null body) leaves source untouched.
func TestApplyMergedDocsSkipsNullDataDoc(t *testing.T) {
	src := []byte("keep: this\n")
	sf, _ := parser.ParseBytes(src, parser.ParseComments)
	df, _ := parser.ParseBytes([]byte("null\n"), parser.ParseComments)

	if err := ApplyMergedDocs(sf, df.Docs); err != nil {
		t.Fatalf("ApplyMergedDocs: %v", err)
	}
	if !strings.Contains(sf.String(), "keep: this") {
		t.Errorf("source content lost when data is null: %q", sf.String())
	}
}

// Regression: data with a nested null child gets stripped from an
// appended subtree (case #5 from the null-invariants audit).
func TestApplyMergedDocsStripsDataNullsFromAppendedSubtree(t *testing.T) {
	sf, _ := parser.ParseBytes([]byte("host: server\n"), parser.ParseComments)
	df, _ := parser.ParseBytes([]byte("config:\n  a: 1\n  drop: null\n  c: 3\n"), parser.ParseComments)

	if err := ApplyMergedDocs(sf, df.Docs); err != nil {
		t.Fatalf("ApplyMergedDocs: %v", err)
	}
	out := sf.String()
	if strings.Contains(out, "drop:") {
		t.Errorf("data-side null in appended subtree not stripped; got %q", out)
	}
	if !strings.Contains(out, "a: 1") || !strings.Contains(out, "c: 3") {
		t.Errorf("non-null siblings lost; got %q", out)
	}
}

// Sanity: apply doesn't mutate mergedDocs (data slot semantics).
func TestApplyMergedDocsDoesNotCorruptDataSlots(t *testing.T) {
	sf, _ := parser.ParseBytes([]byte("k: old\n"), parser.ParseComments)
	df, _ := parser.ParseBytes([]byte("k: new\n"), parser.ParseComments)
	_ = df.Docs[0].Body.(*yamlast.MappingNode) // ensure typed body

	if err := ApplyMergedDocs(sf, df.Docs); err != nil {
		t.Fatalf("ApplyMergedDocs: %v", err)
	}
	// The doc slot itself should still be usable; not nil, still parses.
	if df.Docs[0] == nil {
		t.Errorf("data doc slot was nulled by apply")
	}
}
