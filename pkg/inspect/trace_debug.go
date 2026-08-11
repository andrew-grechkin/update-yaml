//go:build debug

// Debug-only trace impls. Compiled into the binary when `go build -tags debug`
// is used; otherwise the stubs in trace_stub.go take their place and inline
// away. Call sites stay unconditional either way.
//
// The pretty-printing side of tracing lives in pkg/dump (no build tag) so
// cmd/probe-yaml can reuse it; release builds see only trace_stub.go here
// and therefore never pull pkg/dump into their link set.

package inspect

import (
	"fmt"
	"os"

	"github.com/goccy/go-yaml/ast"

	"github.com/andrew-grechkin/update-yaml/pkg/dump"
)

func TraceKey(col, indent int) {
	fmt.Fprintf(os.Stderr, "\033[1;96mKEY:\033[0m col=%d indent=%d\n", col, indent)
}

func TraceNode(label string, n ast.Node) {
	if n == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "\033[1;96m%s: %s\033[0m\n", label, n.String())
	fmt.Fprintln(os.Stderr, dump.DumpJson(dump.Inspect(n)))
}

func TraceValue(label string, v any) {
	fmt.Fprintf(os.Stderr, "\033[1;95m%s: %s\033[0m\n", label, dump.DumpJson(dump.Inspect(v)))
}
