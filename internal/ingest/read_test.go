package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goccy/go-yaml/parser"
)

func TestIsEmptyDataDoc(t *testing.T) {
	// nil doc
	if !IsEmptyDataDoc(nil) {
		t.Errorf("nil doc should be empty")
	}

	// doc with nil body
	f, _ := parser.ParseBytes([]byte(""), parser.ParseComments)
	if !IsEmptyDataDoc(f.Docs[0]) {
		t.Errorf("doc with nil body should be empty")
	}

	// explicit null doc
	f, _ = parser.ParseBytes([]byte("null\n"), parser.ParseComments)
	if !IsEmptyDataDoc(f.Docs[0]) {
		t.Errorf("null-body doc should be empty")
	}

	// empty mapping
	f, _ = parser.ParseBytes([]byte("{}\n"), parser.ParseComments)
	if !IsEmptyDataDoc(f.Docs[0]) {
		t.Errorf("empty mapping doc should be empty")
	}

	// non-empty mapping
	f, _ = parser.ParseBytes([]byte("a: 1\n"), parser.ParseComments)
	if IsEmptyDataDoc(f.Docs[0]) {
		t.Errorf("mapping with entries should not be empty")
	}
}

func TestReadAndMergeDataFilesEmpty(t *testing.T) {
	// No files -> nil result, no error.
	merged, err := ReadAndMergeDataFiles(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if merged != nil {
		t.Errorf("empty input should return nil, got %v", merged)
	}
}

func TestReadAndMergeDataFilesSingle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.yaml")
	if err := os.WriteFile(path, []byte("k: v\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	merged, err := ReadAndMergeDataFiles([]string{path})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(merged) != 1 {
		t.Fatalf("expected 1 doc slot, got %d", len(merged))
	}
	if IsEmptyDataDoc(merged[0]) {
		t.Errorf("merged doc should not be empty")
	}
}

// Two files whose top-level keys overlap: later value wins per the
// "last-write-wins on non-map values" merge rule.
func TestReadAndMergeDataFilesLastWriteWins(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.yaml")
	over := filepath.Join(dir, "over.yaml")
	if err := os.WriteFile(base, []byte("k: base\nother: x\n"), 0o644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	if err := os.WriteFile(over, []byte("k: over\n"), 0o644); err != nil {
		t.Fatalf("write over: %v", err)
	}

	merged, err := ReadAndMergeDataFiles([]string{base, over})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rendered := merged[0].String()
	// override took k, but base's "other" survives (first-occurrence
	// order + last-write-wins on values means base contributed "other"
	// and over updated "k").
	if !containsAll(rendered, "k: over", "other: x") {
		t.Errorf("merge lost content; rendered = %q", rendered)
	}
}

func TestReadAndMergeDataFilesMissingFile(t *testing.T) {
	_, err := ReadAndMergeDataFiles([]string{"/no/such/file.yaml"})
	if err == nil {
		t.Errorf("expected error for missing file, got nil")
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
