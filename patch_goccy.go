package main

// Monkey-patches for goccy's block-scalar render bugs. All of this file
// exists to work around bugs in github.com/goccy/go-yaml that upstream
// hasn't merged a fix for.
//
// The bugs:
//   - ast.LiteralNode.String() does TrimRight(origin, "\n"), stripping the
//     trailing blank lines the `+` chomp is meant to preserve.
//   - ast.StringNode.String() renders multi-line values with the content
//     indent stuck onto empty lines ("    \n" instead of "\n") and applies
//     a TrimSuffix that eats `+`-chomp trailing blanks.
//   - After yaml.Path.ReplaceWithNode swaps a value, the value's inner
//     token positions don't inherit the parent key's indent context, and
//     the next sibling's Key.Prev still points at the pre-swap source
//     token - so goccy's checkLineBreak fires a stale inter-record blank.
//   - When an appended MappingValueNode's Key.Token.Prev points into the
//     data file's token stream, goccy's checkLineBreak inherits the layout
//     the data file had (blank lines above the appended key) and re-emits
//     it in source's output.
//
// Complete removal checklist (in order of file, top to bottom):
//
//   patch_goccy.go
//     - Delete this entire file (all wrapper types and helpers).
//
//   transform.go
//     - Delete `patchLiteralNodeBugs(doc.Body)` in applyMergedDocs
//       (marked with NOTE at the call site).
//     - Delete the `realignToKey(n, keyCol, keyIndent)` call in replaceAt
//       (marked with NOTE at the call site).
//     - Delete the `suppressCheckLineBreakForKeepChomp(oldValTok, rawStr)`
//       call in applyValue (marked with NOTE at the call site).
//     - Delete the `kt.Prev = nil` line in realignAppended (marked with
//       NOTE at the call site).
//     - Remove the format-wrap block in applyValue (NOTE-marked). The
//       `keepChompString` type in format_strings.go handles both style
//       (fold long values to `>`) and bug-fix (multi-line StringNode
//       render); once the bug is fixed, the whole wrap block can be
//       deleted OR the wrapper can be simplified to `>` folding only.
//
//   format_strings.go
//     - Delete the `strings.Contains(val, "\n") { return true }`
//       early-return in shouldWrapStringValue (marked with NOTE). The
//       remaining logic implements the style rule, which is not a
//       library workaround.
//
// The wrapper types stay compiled as dead code until removed at leisure.

import (
	"math"
	"strings"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/token"
)

// Walks a modified doc and wraps every `|+`/`>+` LiteralNode so the
// trailing blank lines the `+` chomp is supposed to keep survive
// re-render. The wrapper delegates to goccy for the rest of the render
// (folded wrap, comments, origin-preserved indent) and just appends the
// newlines the trim ate.
//
// Source-parsed nodes get touched too - not to change their content, but
// to prevent goccy from silently dropping content it was asked to keep.
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

// Reports whether goccy's LiteralNode.String() would drop content that
// `+` chomp is meant to preserve: a `|+`/`>+` header plus a value with
// more than one trailing newline. `+` with a single trailing newline
// renders correctly today (goccy strips it, the enclosing join adds one
// back), so no wrap is needed there.
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

// Wraps a `|+` / `>+` ast.LiteralNode and appends the trailing blank
// lines goccy's own LiteralNode.String() strips via
// TrimRight(origin, "\n"). The wrapped base render is otherwise correct
// (origin-preserved folded wrap, comments, indent), so we delegate to it
// and only fix the trailing-strip.
//
// Wrapping is safe because MappingValueNode.toString() dispatches via the
// ScalarNode interface (n.Value.String()), so the override fires. Goccy
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

// Wraps `lit` and pins its content token's source line to MaxInt so
// goccy's checkLineBreak on the next sibling sees a negative gap and
// doesn't add its own blank on top of ours. Without this, the wrapper's
// trailing content plus goccy's checkLineBreak double up and emit one
// blank too many.
func wrapKeepChompLiteral(lit *ast.LiteralNode) *keepChompLiteral {
	if lit.Value != nil {
		pinTokenLineToMax(lit.Value.Token)
	}
	return &keepChompLiteral{LiteralNode: lit}
}

// Bumps the pre-swap value token's line to MaxInt so that when we
// replace a source scalar with a `+`-chomp block scalar carrying 2+
// trailing newlines, goccy's checkLineBreak on the next sibling doesn't
// add an inter-record blank on top of our wrapper's trailing content.
// For clip/strip the source line is left alone so goccy's natural blank
// between records survives.
//
// NOTE: monkey-patch for goccy render bugs. See the removal checklist at
// the top of this file.
func suppressCheckLineBreakForKeepChomp(oldValTok *token.Token, val string) {
	if trailingNewlineCount(val) < 2 {
		return
	}
	pinTokenLineToMax(oldValTok)
}

// Pushes a token's Position.Line to math.MaxInt32 so that goccy's
// checkLineBreak, which computes `next.Position.Line - prev.Position.Line`,
// sees a negative gap and skips its own blank-line insertion. Nil-safe.
func pinTokenLineToMax(t *token.Token) {
	if t == nil || t.Position == nil {
		return
	}
	t.Position.Line = math.MaxInt32
}

// Counts trailing newlines in s. Shared helper: used both to decide
// whether the LiteralNode wrap is warranted and to gate the pre-swap-token
// suppression above.
func trailingNewlineCount(s string) int {
	return len(s) - len(strings.TrimRight(s, "\n"))
}

// Returns the last source-token of a value node in the tokenizer's
// Prev/Next chain - what the following sibling's Key.Prev points at. For
// a LiteralNode that's the content String's token (Value's GetToken
// returns the header token, which is not what we want here); for plain
// and quoted scalars the value node's own GetToken suffices.
func lastChainToken(n ast.Node) *token.Token {
	if lit, ok := n.(*ast.LiteralNode); ok && lit.Value != nil {
		return lit.Value.Token
	}
	if n == nil {
		return nil
	}
	return n.GetToken()
}
