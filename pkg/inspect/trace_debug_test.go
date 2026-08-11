//go:build debug

// Mirror of trace_stub_test.go for the real Trace impls: same call
// shapes, same edge-case inputs, verifies nothing panics and the
// stderr side effects don't surface as errors. Under `!debug` the
// stub file's test runs instead; the two together give pkg/inspect
// coverage in both build variants.

package inspect

import (
	"testing"

	"github.com/goccy/go-yaml/parser"
)

func TestTraceHelpersDoNotPanic(t *testing.T) {
	// Same inputs as the stub test - nil node, empty label, negative
	// ints - just verified against the real impl which writes to
	// stderr.
	TraceKey(0, 0)
	TraceKey(-1, -1)
	TraceNode("label", nil)
	TraceValue("label", nil)
	TraceValue("empty", "")

	// Realistic call with a live AST node.
	f, _ := parser.ParseBytes([]byte("k: v\n"), parser.ParseComments)
	TraceNode("body", f.Docs[0].Body)
	TraceValue("file", f)
}
