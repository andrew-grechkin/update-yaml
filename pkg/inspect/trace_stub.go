//go:build !debug

// No-op trace stubs for release builds. The debug-tagged file trace_debug.go
// replaces these when compiled with `-tags debug`.

package inspect

import "github.com/goccy/go-yaml/ast"

func TraceKey(col, indent int)           {}
func TraceNode(label string, n ast.Node) {}
func TraceValue(label string, v any)     {}
