package main

// Presentation rule for updated string values: fold long single-line values
// into `>` (folded) block form, and route multi-line values through our own
// `|` renderer that faithfully preserves the parsed content (the multi-line
// branch also compensates for goccy's StringNode render bugs - see NOTE on
// shouldWrapStringValue).
//
// Everything here is orthogonal to patch_goccy.go: those helpers exist only
// because goccy is buggy; these define the tool's own opinion about how a
// value should look after an update.

import (
	"regexp"
	"strings"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	"github.com/goccy/go-yaml/token"
)

// Wraps an ast.StringNode we created via ValueToNode and re-renders it
// from a caller-supplied raw Go string. Two motivations:
//
//   - Style: for a long single-line value we emit `>` block form with
//     word-wrapping (useFolded == true).
//   - Bug workaround: for multi-line values we emit `|` block form directly
//     because goccy's StringNode.String() has render bugs (indent-on-empty,
//     TrimSuffix strip). When goccy fixes those, the useFolded == false
//     branch and shouldWrapStringValue's multi-line early-return can be
//     removed - see the NOTE on shouldWrapStringValue.
//
// We store `val` (raw Go string) rather than reading from n.StringNode.Value
// because goccy's ValueToNode bakes surrounding quotes into that field when
// the value would need quoting in plain form. Reading it back would embed
// literal quotes inside a block scalar.
type keepChompString struct {
	*ast.StringNode
	val       string
	keyCol    int
	useFolded bool
}

func (n *keepChompString) String() string {
	val := n.val
	trimmed := strings.TrimRight(val, "\n")
	trailNL := len(val) - len(trimmed)
	indent := strings.Repeat(" ", n.keyCol-1+detectedStyle.indent)

	var header string
	var lines []string
	if n.useFolded {
		header = foldedHeader(trailNL)
		lines = foldWrap(trimmed, detectedStyle.maxLineWidth-len(indent))
	} else {
		header = literalHeader(trailNL)
		lines = strings.Split(trimmed, "\n")
	}

	var sb strings.Builder
	sb.WriteString(header)
	// Inline comment on the header line, mirroring what
	// ast.LiteralNode.String() does. Without this, an inline comment
	// preserved through the replace path is dropped at render.
	if c := n.GetComment(); c != nil {
		sb.WriteString(strings.Repeat(" ", detectedStyle.spacesBeforeInlineComment))
		sb.WriteString(c.String())
	}
	for _, line := range lines {
		sb.WriteByte('\n')
		if line != "" {
			sb.WriteString(indent)
			sb.WriteString(line)
		}
	}
	// `+` chomp keeps every trailing newline. The enclosing MappingNode join
	// adds one back, so we emit trailNL-1 extras.
	if strings.HasSuffix(header, "+") {
		for range trailNL - 1 {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// Applies the block-form rule for updated string values. Wrap when
// either:
//   - value carries embedded newlines (plain can't represent them), or
//   - value is a single line long enough that its plain-form record exceeds
//     detectedStyle.maxLineWidth AND has whitespace to fold on AND has no
//     leading or trailing whitespace (block scalars strip both).
//
// Anything else stays plain, letting goccy render as it always did.
//
// NOTE: two motivations mixed here. The `strings.Contains(val, "\n")`
// early-return is a monkey-patch for goccy's StringNode multi-line render
// bugs; delete it (per the removal checklist in patch_goccy.go) to leave
// only the style-selection rule (the long-single-line path) active.
func shouldWrapStringValue(val string, keyCol int, keyName string) bool {
	if strings.Contains(val, "\n") {
		return true
	}
	if val == "" {
		return false
	}
	if val[0] == ' ' || val[0] == '\t' {
		return false
	}
	if last := val[len(val)-1]; last == ' ' || last == '\t' {
		return false
	}
	if !strings.ContainsAny(val, " \t") {
		return false
	}
	return plainRecordLen(val, keyName, keyCol) > detectedStyle.maxLineWidth
}

// Picks `>` (folded) over `|` (literal). `>` only pays off when
// the content is one paragraph (no mid-newlines) that has whitespace to
// break at AND whose plain rendering wouldn't fit on one line. Everything
// else - multi-line values, single-word values, short values - stays `|`.
func shouldFold(val, keyName string, keyCol int) bool {
	trimmed := strings.TrimRight(val, "\n")
	if strings.Contains(trimmed, "\n") {
		return false
	}
	if !strings.ContainsAny(trimmed, " \t") {
		return false
	}
	return plainRecordLen(val, keyName, keyCol) > detectedStyle.maxLineWidth
}

// Translates a trailing-newline count into the right chomp indicator for
// `|`-family and `>`-family block scalars.
func literalHeader(trailNL int) string {
	switch trailNL {
	case 0:
		return "|-"
	case 1:
		return "|"
	default:
		return "|+"
	}
}

func foldedHeader(trailNL int) string {
	switch trailNL {
	case 0:
		return ">-"
	case 1:
		return ">"
	default:
		return ">+"
	}
}

// Estimates the record's `<indent><key>: <value>` column count if val
// were emitted plain (quoted if content demands it). It only has to be
// exact around the detectedStyle.maxLineWidth boundary; a 2-char miss at
// the quoting decision changes plain-vs-block only at that exact 120/122
// edge.
func plainRecordLen(val, key string, keyCol int) int {
	valLen := len(val)
	if needsQuoting(val) {
		valLen += 2
	}
	return (keyCol - 1) + len(key) + len(": ") + valLen
}

// Walks a data-provided node and demotes any explicitly-quoted StringNode
// whose value would parse to the same string unquoted. Runs only on
// data-provided nodes so source-verbatim quotes on unchanged values
// survive. Visits both keys and values so JSON-style double-quoted keys
// ("port": 9090) end up as plain YAML keys.
func unquoteSafeStrings(n ast.Node) {
	switch v := n.(type) {
	case *ast.StringNode:
		if v.Token == nil {
			return
		}
		if v.Token.Type != token.SingleQuoteType && v.Token.Type != token.DoubleQuoteType {
			return
		}
		if !safeToUnquote(v.Value) {
			return
		}
		v.Token.Type = token.StringType
	case *ast.MappingNode:
		for _, mv := range v.Values {
			unquoteSafeStrings(mv)
		}
	case *ast.MappingValueNode:
		unquoteSafeStrings(v.Key)
		unquoteSafeStrings(v.Value)
	case *ast.SequenceNode:
		for _, entry := range v.Values {
			unquoteSafeStrings(entry)
		}
	case *ast.AnchorNode:
		unquoteSafeStrings(v.Value)
	}
}

// Scalar forms that YAML 1.1 (still widely used by Ansible, older Puppet,
// Ruby libraries) resolves to booleans. Goccy is YAML 1.2 and round-trips
// them as plain strings, so the parse-based oracle in safeToUnquote
// wouldn't reject them on its own. Quotes are kept to preserve semantics
// for downstream 1.1 consumers.
var yaml11Bools = map[string]bool{
	"y": true, "Y": true, "yes": true, "Yes": true, "YES": true,
	"n": true, "N": true, "no": true, "No": true, "NO": true,
	"on": true, "On": true, "ON": true,
	"off": true, "Off": true, "OFF": true,
}

// The YAML 1.1 base-60 integer form, copied verbatim from
// http://yaml.org/type/int.html. Each colon-separated section after the
// first must fall in [0,59].
var sexagesimalPattern = regexp.MustCompile(`^[-+]?[1-9][0-9_]*(:[0-5]?[0-9])+$`)

// Decides whether v, if written as a plain scalar, would parse back to
// the same string. Uses the parser as its own oracle so we don't have to
// enumerate every indicator character, reserved token, or resolvable type
// keyword - anything goccy parses as a non-StringNode (or a StringNode
// with different content) is unsafe. Additionally rejects YAML 1.1
// spellings that goccy accepts as plain strings but older parsers would
// resolve to non-string types: boolean words and sexagesimal digits
// (`80:80`, `1:30:00`). Real-world files - docker-compose port mappings,
// Ansible playbooks - lean on the 1.1 conventions.
//
// The check runs in block context (top-level parse). A quoted scalar
// inside a flow-style mapping/sequence has stricter plain-scalar rules
// (no bare commas, brackets, or braces); if that comes up, this function
// will need flow-context awareness.
func safeToUnquote(v string) bool {
	if v == "" {
		return false
	}
	if yaml11Bools[v] {
		return false
	}
	if sexagesimalPattern.MatchString(v) {
		return false
	}
	file, err := parser.ParseBytes([]byte(v), 0)
	if err != nil || len(file.Docs) != 1 {
		return false
	}
	sn, ok := file.Docs[0].Body.(*ast.StringNode)
	if !ok {
		return false
	}
	return sn.Value == v
}

// Mirrors goccy's plain-scalar restrictions closely enough for width
// estimation. Not a substitute for goccy's real quoting decision - only
// used to size the record for the maxLineWidth comparison.
func needsQuoting(s string) bool {
	if s == "" {
		return true
	}
	switch s[0] {
	case ' ', '\t', '-', '?', ':', ',', '[', ']', '{', '}', '#', '&', '*', '!', '|', '>', '\'', '"', '%', '@', '`':
		return true
	}
	last := s[len(s)-1]
	if last == ' ' || last == '\t' || last == ':' {
		return true
	}
	if strings.Contains(s, ": ") || strings.Contains(s, " #") {
		return true
	}
	switch strings.ToLower(s) {
	case "true", "false", "null", "yes", "no", "on", "off", "~":
		return true
	}
	return false
}

// Breaks `s` across lines no wider than `width`, splitting only on single
// spaces. Runs of two or more spaces stay atomic inside a segment -
// splitting a multi-space run would leave a wrapped line starting with
// whitespace, which folded-scalar semantics treat as "more indented" and
// preserve as a literal newline instead of folding to a space.
func foldWrap(s string, width int) []string {
	if s == "" || width < 1 {
		return []string{s}
	}
	atoms := splitFoldAtoms(s)
	var lines []string
	var cur strings.Builder
	for _, a := range atoms {
		if cur.Len() == 0 {
			cur.WriteString(a)
			continue
		}
		if cur.Len()+1+len(a) <= width {
			cur.WriteByte(' ')
			cur.WriteString(a)
			continue
		}
		lines = append(lines, cur.String())
		cur.Reset()
		cur.WriteString(a)
	}
	lines = append(lines, cur.String())
	return lines
}

func splitFoldAtoms(s string) []string {
	var atoms []string
	var cur strings.Builder
	i := 0
	for i < len(s) {
		if s[i] != ' ' {
			cur.WriteByte(s[i])
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] == ' ' {
			j++
		}
		if j-i == 1 {
			atoms = append(atoms, cur.String())
			cur.Reset()
		} else {
			cur.WriteString(s[i:j])
		}
		i = j
	}
	atoms = append(atoms, cur.String())
	return atoms
}
