package output

import (
	"bytes"
	"strings"
	"testing"

	"github.com/goccy/go-yaml/parser"
)

func TestWriteEmptyFile(t *testing.T) {
	// parser.ParseBytes on "" returns a single doc with a null body;
	// that's a valid but empty doc. Pass a correctly-sized modified
	// slice - Write panics on a shorter slice, and that's a real
	// contract (caller must size modified to len(file.Docs)).
	f, err := parser.ParseBytes([]byte(""), parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, f, nil, make([]bool, len(f.Docs))); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Empty doc renders as an empty content line plus the trailing \n
	// Write always appends.
	if buf.String() != "\n" {
		t.Errorf("expected '\\n' for empty doc, got %q", buf.String())
	}
}

func TestWriteUnmodifiedDocPassthrough(t *testing.T) {
	src := []byte("key: value # inline\n")
	f, _ := parser.ParseBytes(src, parser.ParseComments)
	var buf bytes.Buffer
	if err := Write(&buf, f, src, []bool{false}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Untouched doc must byte-passthrough (verbatim). A trailing newline
	// is guaranteed by Write's contract.
	got := buf.String()
	if got != string(src) {
		t.Errorf("passthrough diverged\n want %q\n  got %q", src, got)
	}
}

func TestWriteModifiedDocUsesRender(t *testing.T) {
	// Same content, but modified=true means Write uses doc.String()
	// rather than the original bytes. Result is semantically identical
	// but may differ in whitespace.
	src := []byte("key: value\n")
	f, _ := parser.ParseBytes(src, parser.ParseComments)
	var buf bytes.Buffer
	if err := Write(&buf, f, src, []bool{true}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "key: value") {
		t.Errorf("expected 'key: value' in rendered output, got %q", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("expected trailing newline, got %q", got)
	}
}

func TestWriteMultiDocMixedModified(t *testing.T) {
	// Two docs: first modified (rendered), second unmodified (passthrough).
	src := []byte("---\na: 1\n---\nb: 2\n")
	f, _ := parser.ParseBytes(src, parser.ParseComments)
	if len(f.Docs) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(f.Docs))
	}
	var buf bytes.Buffer
	if err := Write(&buf, f, src, []bool{true, false}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "a: 1") || !strings.Contains(got, "b: 2") {
		t.Errorf("expected both docs in output, got %q", got)
	}
	// Doc separator must survive.
	if strings.Count(got, "---") == 0 {
		t.Errorf("expected doc separator, got %q", got)
	}
}

func TestWritePreservesInterDocBlanks(t *testing.T) {
	// Two blank lines between the docs in source must be reproduced
	// in the output.
	src := []byte("---\na: 1\n\n\n---\nb: 2\n")
	f, _ := parser.ParseBytes(src, parser.ParseComments)
	var buf bytes.Buffer
	if err := Write(&buf, f, src, []bool{true, true}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := buf.String()
	// Between the first doc's content and the "---" of the second, we
	// expect 2 blank lines preserved from source. (Total: content \n
	// then \n\n blanks then --- ...)
	if !strings.Contains(got, "1\n\n\n---") {
		t.Errorf("inter-doc blanks not preserved; got %q", got)
	}
}

func TestWriteTrailingNewlineGuaranteed(t *testing.T) {
	// Source has no trailing newline; Write must still emit one.
	src := []byte("key: value")
	f, _ := parser.ParseBytes(src, parser.ParseComments)
	var buf bytes.Buffer
	if err := Write(&buf, f, src, []bool{true}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Errorf("Write did not append trailing newline: %q", buf.String())
	}
}
