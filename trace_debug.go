//go:build debug

// Debug-only trace impls. Compiled into the binary when `go build -tags debug`
// is used; otherwise the stubs in trace_stub.go take their place and inline
// away. Call sites in replaceAt stay unconditional either way.

package main

import (
	"fmt"
	"os"

	"github.com/goccy/go-yaml/ast"
)

func traceKey(col, indent int) {
	fmt.Fprintf(os.Stderr, "\033[1;96mKEY:\033[0m col=%d indent=%d\n", col, indent)
}

func traceNode(label string, n ast.Node) {
	if n == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "\033[1;96m%s: %s\033[0m\n", label, n.String())
	fmt.Fprintln(os.Stderr, DumpJson(Inspect(n)))
}

func traceValue(label string, v any) {
	fmt.Fprintf(os.Stderr, "\033[1;95m%s: %s\033[0m\n", label, DumpJson(Inspect(v)))
}
