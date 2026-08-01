// Generic AST helpers around github.com/goccy/go-yaml/ast. Small utilities
// that any goccy-based tool benefits from; independent of the update-yaml
// pipeline.
package ast

import (
	"strings"

	yaml "github.com/goccy/go-yaml"
	yamlast "github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/token"
)

// Reports whether n is the explicit YAML null token (either `null` / `~`
// or an empty value that parsed to *ast.NullNode). Data semantics: a null
// in the data tree means "delete this key from source".
func IsNullNode(n yamlast.Node) bool {
	_, ok := n.(*yamlast.NullNode)
	return ok
}

// Extracts the string form of a mapping key. StringNode is the common
// case; anything else falls back to the node's .String() form.
func KeyString(node yamlast.Node) string {
	if s, ok := node.(*yamlast.StringNode); ok {
		return s.Value
	}
	return node.String()
}

// Non-destructive anchor peel: returns n's inner value if n is an
// AnchorNode, else n itself. Sufficient for callers that only need to
// see through the wrapper for type-inspection or transparent recursion.
func UnwrapAnchor(n yamlast.Node) yamlast.Node {
	if a, ok := n.(*yamlast.AnchorNode); ok {
		return a.Value
	}
	return n
}

// Returns the mapping values contained in node - either the whole Values
// slice for *MappingNode or a single-element slice for *MappingValueNode.
// Anything else returns nil.
func MappingValues(node yamlast.Node) []*yamlast.MappingValueNode {
	switch n := node.(type) {
	case *yamlast.MappingNode:
		return n.Values
	case *yamlast.MappingValueNode:
		return []*yamlast.MappingValueNode{n}
	}
	return nil
}

// Writes col to the node's token Position.Column in one nil-safe step.
func SetPositionColumn(n yamlast.Node, col int) {
	t := n.GetToken()
	if t == nil || t.Position == nil {
		return
	}
	t.Position.Column = col
}

// Writes both Column and IndentLevel on a token in one nil-safe step.
// Used where a swap places a scalar value at the parent key's anchor.
func SetTokenColAndIndent(t *token.Token, col, indent int) {
	if t == nil || t.Position == nil {
		return
	}
	t.Position.Column = col
	t.Position.IndentLevel = indent
}

// Returns the yaml.Path result at file, or nil on any lookup error.
func NodeAt(p *yaml.Path, file *yamlast.File) yamlast.Node {
	n, err := p.FilterFile(file)
	if err != nil {
		return nil
	}
	return n
}

// Uses goccy's reserved-char escape for non-identifier keys:
// `$.foo.'bar.baz-*'.hoge`, with `\` escaping `'` and `\` itself. Bracket
// form `$['key']` is rejected by goccy's PathString.
func JoinPath(prefix, key string) string {
	if IsSimpleIdent(key) {
		if prefix == "$" {
			return "$." + key
		}
		return prefix + "." + key
	}
	q := strings.ReplaceAll(key, `\`, `\\`)
	q = strings.ReplaceAll(q, "'", `\'`)
	return prefix + ".'" + q + "'"
}

// Reports whether s is a bare identifier (letters, digits after first,
// underscore, hyphen) that survives a yaml.Path without escaping.
func IsSimpleIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if !IsIdentRune(r, i == 0) {
			return false
		}
	}
	return true
}

// Reports whether r is legal in an identifier position; digits are only
// allowed when isFirst is false.
func IsIdentRune(r rune, isFirst bool) bool {
	switch {
	case r >= 'a' && r <= 'z':
		return true
	case r >= 'A' && r <= 'Z':
		return true
	case r == '_' || r == '-':
		return true
	case !isFirst && r >= '0' && r <= '9':
		return true
	}
	return false
}

// Returns the token goccy uses when deciding whether to insert a blank
// line above this entry - the head comment start if there is one, else
// the key token.
func FirstEntryToken(mv *yamlast.MappingValueNode) *token.Token {
	if cg := mv.GetComment(); cg != nil && len(cg.Comments) > 0 {
		return cg.Comments[0].Token
	}
	return mv.Key.GetToken()
}

// Reports whether the source had a blank line immediately above this
// entry. A gap of more than one line between the entry's first source
// token and its Prev means an intervening blank line - the section break
// we want to keep across a removal.
func HasLeadingBlankLine(mv *yamlast.MappingValueNode) bool {
	tok := FirstEntryToken(mv)
	if tok == nil || tok.Prev == nil {
		return false
	}
	return tok.Position.Line-tok.Prev.Position.Line > 1
}

// Reports whether t is a single- or double-quoted scalar token type.
// Callers use this to detect explicit-quote hints on scalars before
// deciding whether to strip them (see style.UnquoteSafeStrings).
func IsExplicitQuote(t token.Type) bool {
	return t == token.SingleQuoteType || t == token.DoubleQuoteType
}

// Pre-order AST walker. Invokes fn on every node from root down;
// returning false from fn skips recursion into that node's children.
// Anchor wrappers are peeled transparently so callers can ignore them.
// Suitable for read-only inspection and in-place field mutation;
// passes that need to swap a node's type (replace a StringNode with a
// LiteralNode etc) should use ReplaceLeaves instead.
func Walk(n yamlast.Node, fn func(yamlast.Node) bool) {
	if n == nil || !fn(n) {
		return
	}
	switch v := n.(type) {
	case *yamlast.MappingNode:
		for _, mv := range v.Values {
			Walk(mv, fn)
		}
	case *yamlast.MappingValueNode:
		Walk(v.Key, fn)
		Walk(v.Value, fn)
	case *yamlast.SequenceNode:
		for _, c := range v.Values {
			Walk(c, fn)
		}
	case *yamlast.AnchorNode:
		Walk(v.Value, fn)
	}
}

// Walks root and lets fn swap any value-slot node (mapping entry value,
// sequence element, anchor inner value) for a replacement. If fn
// returns non-nil, the slot is updated and traversal stops descending
// into the replaced subtree; if fn returns nil, walking continues into
// the original node. Complement to Walk for passes that need to change
// node types rather than mutate fields.
func ReplaceLeaves(root yamlast.Node, fn func(yamlast.Node) yamlast.Node) {
	switch v := root.(type) {
	case *yamlast.MappingNode:
		for _, mv := range v.Values {
			ReplaceLeaves(mv, fn)
		}
	case *yamlast.MappingValueNode:
		if r := fn(v.Value); r != nil {
			v.Value = r
		} else {
			ReplaceLeaves(v.Value, fn)
		}
	case *yamlast.SequenceNode:
		for i, c := range v.Values {
			if r := fn(c); r != nil {
				v.Values[i] = r
			} else {
				ReplaceLeaves(c, fn)
			}
		}
	case *yamlast.AnchorNode:
		if r := fn(v.Value); r != nil {
			v.Value = r
		} else {
			ReplaceLeaves(v.Value, fn)
		}
	}
}

// Mirrors goccy's checkLineBreak for a single token: raw source-line diff
// minus any newlines carried in the previous token's origin. When the
// result is positive, goccy prepends a "\n" during render.
func GoccyWouldInsertBlank(t *token.Token) bool {
	if t == nil || t.Prev == nil {
		return false
	}
	lineDiff := t.Position.Line - t.Prev.Position.Line - 1
	if lineDiff <= 0 {
		return false
	}
	adjustment := 0
	if t.Prev.Type == token.StringType {
		adjustment = strings.Count(strings.TrimRight(strings.TrimSpace(t.Prev.Origin), "\n"), "\n")
	}
	return lineDiff-adjustment > 0
}
