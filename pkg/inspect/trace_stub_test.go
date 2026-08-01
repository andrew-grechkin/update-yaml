//go:build !debug

// Verifies the release-build stubs don't panic and satisfy the
// unconditional-call contract that transform's replaceAt / applyValue
// rely on. Under -tags debug, trace_debug.go replaces these; that
// build is exercised by inspect_test.go which shares the same tag.

package inspect

import (
	"testing"

	"github.com/goccy/go-yaml/parser"
)

func TestTraceStubsAreNoOp(t *testing.T) {
	// These must not panic even with edge-case inputs (nil node,
	// empty label, extreme int values).
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
