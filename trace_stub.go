//go:build !debug

// No-op trace stubs for release builds. The debug-tagged file trace_debug.go
// replaces these when compiled with `-tags debug`.

package main

import "github.com/goccy/go-yaml/ast"

func traceKey(col, indent int)           {}
func traceNode(label string, n ast.Node) {}
func traceValue(label string, v any)     {}
