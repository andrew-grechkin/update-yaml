package main

// Monkey-patches for goccy's block-scalar render bugs. All of this file
// exists to work around bugs in github.com/goccy/go-yaml that upstream
// hasn't merged a fix for. When those bugs are fixed:
//
//   1. Drop the patchLiteralNodeBugs(doc.Body) call site in applyMergedDocs.
//   2. Delete the suppressCheckLineBreakForKeepChomp call in applyValue.
//
// The wrapper types and helpers here then become dead code that can be
// removed at leisure; they compile without harm in the meantime.
//
// The bugs:
//   - ast.LiteralNode.String() does TrimRight(origin, "\n"), stripping the
//     trailing blank lines the `+` chomp is meant to preserve.
//   - After yaml.Path.ReplaceWithNode swaps a value, the next sibling's
//     Key.Prev still points at the pre-swap source token, so goccy's
//     checkLineBreak fires an inter-record blank based on stale positions.

import (
	"math"
	"strings"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/token"
)

// patchLiteralNodeBugs walks a modified doc and wraps every `|+`/`>+`
// LiteralNode so the trailing blank lines the `+` chomp is supposed to keep
// survive re-render. The wrapper delegates to goccy for the rest of the
// render (folded wrap, comments, origin-preserved indent) and just appends
// the newlines the trim ate.
//
// This walker touches source-parsed nodes too - not to change their content,
// but to prevent goccy from silently dropping content it was asked to keep.
func patchLiteralNodeBugs(node ast.Node) {
	switch n := node.(type) {
	case *ast.MappingNode:
		for _, mv := range n.Values {
			patchLiteralNodeBugs(mv)
		}
	case *ast.MappingValueNode:
		if lit, ok := n.Value.(*ast.LiteralNode); ok && needsKeepChompFix(lit) {
			n.Value = wrapKeepChompLiteral(lit)
			return
		}
		patchLiteralNodeBugs(n.Value)
	case *ast.SequenceNode:
		for i, v := range n.Values {
			if lit, ok := v.(*ast.LiteralNode); ok && needsKeepChompFix(lit) {
				n.Values[i] = wrapKeepChompLiteral(lit)
				continue
			}
			patchLiteralNodeBugs(v)
		}
	case *ast.AnchorNode:
		patchLiteralNodeBugs(n.Value)
	}
}

// needsKeepChompFix reports whether goccy's LiteralNode.String() would drop
// content that `+` chomp is meant to preserve: a `|+`/`>+` header plus a
// value with more than one trailing newline. `+` with a single trailing
// newline renders correctly today (goccy strips it, the enclosing join adds
// one back), so no wrap is needed there.
func needsKeepChompFix(lit *ast.LiteralNode) bool {
	if lit.Start == nil || lit.Value == nil {
		return false
	}
	header := strings.TrimSpace(lit.Start.Value)
	if len(header) < 2 || header[1] != '+' {
		return false
	}
	return trailingNewlineCount(lit.Value.Value) >= 2
}

// keepChompLiteral wraps a `|+` / `>+` ast.LiteralNode and appends the
// trailing blank lines goccy's own LiteralNode.String() strips via
// TrimRight(origin, "\n"). The wrapped base render is otherwise correct
// (origin-preserved folded wrap, comments, indent), so we delegate to it
// and only fix the trailing-strip.
//
// Wrapping is safe because MappingValueNode.toString() dispatches via the
// ScalarNode interface (n.Value.String()), so our override fires. Goccy
// currently only calls the unexported stringWithoutComment on keys, never
// on values, so the promoted-but-not-overridden method is out of the
// render path for our purpose.
type keepChompLiteral struct {
	*ast.LiteralNode
}

func (n *keepChompLiteral) String() string {
	base := n.LiteralNode.String()
	// -1 because the enclosing MappingNode join adds one newline back.
	extra := trailingNewlineCount(n.LiteralNode.Value.Value) - 1
	if extra > 0 {
		base += strings.Repeat("\n", extra)
	}
	return base
}

// wrapKeepChompLiteral wraps `lit` and pins its content token's source line
// to MaxInt so goccy's checkLineBreak on the next sibling sees a negative
// gap and doesn't add its own blank on top of ours. Without this, the
// wrapper's trailing content plus goccy's checkLineBreak double up and we
// emit one blank too many.
func wrapKeepChompLiteral(lit *ast.LiteralNode) *keepChompLiteral {
	if lit.Value != nil && lit.Value.Token != nil && lit.Value.Token.Position != nil {
		lit.Value.Token.Position.Line = math.MaxInt32
	}
	return &keepChompLiteral{LiteralNode: lit}
}

// suppressCheckLineBreakForKeepChomp bumps the pre-swap value token's line
// to MaxInt so that when we replace a source scalar with a `+`-chomp block
// scalar carrying 2+ trailing newlines, goccy's checkLineBreak on the next
// sibling doesn't add an inter-record blank on top of our wrapper's
// trailing content. For clip/strip we leave the source line alone so
// goccy's natural blank between records survives.
func suppressCheckLineBreakForKeepChomp(oldValTok *token.Token, val string) {
	if trailingNewlineCount(val) < 2 {
		return
	}
	if oldValTok == nil || oldValTok.Position == nil {
		return
	}
	oldValTok.Position.Line = math.MaxInt32
}

// trailingNewlineCount is a shared helper: used both to decide whether the
// LiteralNode wrap is warranted and to gate the pre-swap-token suppression
// above.
func trailingNewlineCount(s string) int {
	return len(s) - len(strings.TrimRight(s, "\n"))
}

// lastChainToken returns the last source-token of a value node in the
// tokenizer's Prev/Next chain - what the following sibling's Key.Prev
// points at. For a LiteralNode that's the content String's token (Value's
// GetToken returns the header token, which is not what we want here); for
// plain and quoted scalars the value node's own GetToken suffices.
func lastChainToken(n ast.Node) *token.Token {
	if lit, ok := n.(*ast.LiteralNode); ok && lit.Value != nil {
		return lit.Value.Token
	}
	if n == nil {
		return nil
	}
	return n.GetToken()
}
