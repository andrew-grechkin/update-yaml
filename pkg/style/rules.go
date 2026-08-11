// Presentation rule for updated string values: fold long single-line values
// into `>` (folded) block form, and route multi-line values through this
// package's `|` renderer that faithfully preserves the parsed content (the
// multi-line branch also compensates for goccy's StringNode render bugs
// - see NOTE on ShouldWrapStringValue).
//
// Everything here is orthogonal to pkg/patch: those helpers exist only
// because goccy is buggy; these define the tool's own opinion about how
// a value should look after an update.
package style

import (
	"regexp"
	"strings"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	"github.com/goccy/go-yaml/token"
	"github.com/mattn/go-runewidth"

	astutil "github.com/andrew-grechkin/update-yaml/pkg/ast"
)

// Wraps an ast.StringNode we created via ValueToNode and re-renders it
// from a caller-supplied raw Go string. Two motivations:
//
//   - Style: for a long single-line value we emit `>` block form with
//     word-wrapping (UseFolded == true).
//   - Bug workaround: for multi-line values we emit `|` block form
//     directly because goccy's StringNode.String() has render bugs
//     (indent-on-empty, TrimSuffix strip). When goccy fixes those, the
//     UseFolded == false branch and ShouldWrapStringValue's multi-line
//     early-return can be removed - see the NOTE on ShouldWrapStringValue.
//
// Val (raw Go string) is stored rather than reading from n.StringNode.Value
// because goccy's ValueToNode bakes surrounding quotes into that field
// when the value would need quoting in plain form. Reading it back would
// embed literal quotes inside a block scalar.
type KeepChompString struct {
	*ast.StringNode
	Val       string
	KeyCol    int
	UseFolded bool
}

func (n *KeepChompString) String() string {
	val := n.Val
	trimmed := strings.TrimRight(val, "\n")
	trailNL := len(val) - len(trimmed)
	indent := strings.Repeat(" ", n.KeyCol-1+Active.Indent)

	var header string
	var lines []string
	if n.UseFolded {
		header = foldedHeader(trailNL)
		lines = FoldWrap(trimmed, Active.MaxLineWidth-len(indent))
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
		sb.WriteString(strings.Repeat(" ", Active.SpacesBeforeInlineComment))
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
//   - value is a single line long enough that its plain-form record
//     exceeds Active.MaxLineWidth AND has whitespace to fold on AND has
//     no leading or trailing whitespace (block scalars strip both).
//
// Anything else stays plain, letting goccy render as it always did.
//
// NOTE: two motivations mixed here. The `strings.Contains(val, "\n")`
// early-return is a monkey-patch for goccy's StringNode multi-line render
// bugs; delete it (per the removal checklist in pkg/patch) to leave only
// the style-selection rule (the long-single-line path) active.
func ShouldWrapStringValue(val string, keyCol int, keyName string) bool {
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
	return exceedsFoldBudget(plainRecordLen(val, keyName, keyCol))
}

// Picks `>` (folded) over `|` (literal). `>` only pays off when
// the content is one paragraph (no mid-newlines) that has whitespace to
// break at AND whose plain rendering wouldn't fit on one line. Everything
// else - multi-line values, single-word values, short values - stays `|`.
func ShouldFold(val, keyName string, keyCol int) bool {
	trimmed := strings.TrimRight(val, "\n")
	if strings.Contains(trimmed, "\n") {
		return false
	}
	if !strings.ContainsAny(trimmed, " \t") {
		return false
	}
	return exceedsFoldBudget(plainRecordLen(val, keyName, keyCol))
}

// Reports whether recordLen exceeds the active fold budget. Returns false
// when folding is disabled (MaxLineWidth == 0), so a `> 0` budget check
// and the "record too long" check live in one place.
func exceedsFoldBudget(recordLen int) bool {
	return Active.MaxLineWidth > 0 && recordLen > Active.MaxLineWidth
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
// were emitted plain (quoted if content demands it). Measures in
// display columns via DisplayWidth so multibyte content triggers the
// fold at the same visual boundary as ASCII. It only has to be exact
// around the Active.MaxLineWidth boundary; a 2-char miss at the
// quoting decision changes plain-vs-block only at that exact 120/122
// edge.
func plainRecordLen(val, key string, keyCol int) int {
	valLen := DisplayWidth(val)
	if NeedsQuoting(val) {
		valLen += 2
	}
	return (keyCol - 1) + DisplayWidth(key) + len(": ") + valLen
}

// Walks a data-provided node and demotes any explicitly-quoted StringNode
// whose value would parse to the same string unquoted. Runs only on
// data-provided nodes so source-verbatim quotes on unchanged values
// survive. Visits both keys and values so JSON-style double-quoted keys
// ("port": 9090) end up as plain YAML keys.
//
// Tracks flow context during descent: once the walker enters a
// flow-style mapping or sequence, every nested string is checked with
// SafeToUnquoteInFlow instead of SafeToUnquote so a value like
// `^[234][0-9]{2}$` (safe in block, would swallow the closing bracket
// in flow) stays quoted.
//
// Rewrites Token.Type, Token.Value, and Token.Origin so goccy re-emits
// the value plain instead of reusing the original "..."-wrapped source
// text. Rewriting all three is required for idempotence: a re-parse of
// the output must produce a StringNode that no longer needs unquoting.
func UnquoteSafeStrings(n ast.Node) {
	unquoteSafeStrings(n, false)
}

func unquoteSafeStrings(n ast.Node, inFlow bool) {
	if n == nil {
		return
	}
	switch v := n.(type) {
	case *ast.MappingNode:
		childFlow := inFlow || v.IsFlowStyle
		for _, mv := range v.Values {
			unquoteSafeStrings(mv, childFlow)
		}
	case *ast.MappingValueNode:
		unquoteSafeStrings(v.Key, inFlow)
		unquoteSafeStrings(v.Value, inFlow)
	case *ast.SequenceNode:
		childFlow := inFlow || v.IsFlowStyle
		for _, c := range v.Values {
			unquoteSafeStrings(c, childFlow)
		}
	case *ast.AnchorNode:
		unquoteSafeStrings(v.Value, inFlow)
	case *ast.StringNode:
		if v.Token == nil || !astutil.IsExplicitQuote(v.Token.Type) {
			return
		}
		safe := SafeToUnquote(v.Value)
		if inFlow {
			safe = SafeToUnquoteInFlow(v.Value)
		}
		if !safe {
			return
		}
		v.Token.Type = token.StringType
		v.Token.Value = v.Value
		v.Token.Origin = v.Value
	}
}

// Scalar forms that YAML 1.1 (still widely used by Ansible, older Puppet,
// Ruby libraries) resolves to booleans. Goccy is YAML 1.2 and round-trips
// them as plain strings, so the parse-based oracle in SafeToUnquote
// wouldn't reject them on its own. Quotes are kept to preserve semantics
// for downstream 1.1 consumers.
var yaml11Bools = map[string]bool{
	"y": true, "Y": true, "yes": true, "Yes": true, "YES": true,
	"n": true, "N": true, "no": true, "No": true, "NO": true,
	"on": true, "On": true, "ON": true,
	"off": true, "Off": true, "OFF": true,
}

// Reports whether v is a YAML 1.1 boolean spelling that older parsers
// (Ansible, PyYAML, Ruby psych) resolve to bool. Quotes on these values
// must survive round-trips to preserve semantics for downstream 1.1
// consumers.
func IsYAML11Bool(v string) bool { return yaml11Bools[v] }

// The YAML 1.1 base-60 integer form, copied verbatim from
// http://yaml.org/type/int.html. Each colon-separated section after the
// first must fall in [0,59].
var sexagesimalPattern = regexp.MustCompile(`^[-+]?[1-9][0-9_]*(:[0-5]?[0-9])+$`)

// Reports whether v resolves to a YAML 1.1 base-60 integer (docker-
// compose port mappings, Ansible time notation like "1:30:00").
// Genuine matches stay quoted so 1.1 consumers keep reading them as
// strings; near-misses (a section >= 60, leading zero) fall through
// as plain strings and would unquote cleanly.
func LooksLikeSexagesimal(v string) bool { return sexagesimalPattern.MatchString(v) }

// Display width of s in monospace columns, counting CJK/emoji per
// runewidth's East Asian Width classification. Byte length is not
// safe for fold-budget computation when input can contain multi-byte
// runes.
func DisplayWidth(s string) int { return runewidth.StringWidth(s) }

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
// The check runs in block context (top-level parse). Callers emitting
// scalars inside a flow-style mapping/sequence should use
// SafeToUnquoteInFlow instead - flow context has stricter plain-scalar
// rules (no bare commas, brackets, or braces) that this function does
// not enforce on its own.
func SafeToUnquote(v string) bool {
	if v == "" {
		return false
	}
	if IsYAML11Bool(v) {
		return false
	}
	if LooksLikeSexagesimal(v) {
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

// Decides whether v is safe to write as a plain scalar inside a
// flow-style mapping or sequence. Layered check: first the block-context
// oracle, then a fast reject on any of the five flow indicators
// (`,`, `[`, `]`, `{`, `}`) that would terminate the scalar early, then
// a flow-context oracle that parses `[v]` and confirms it round-trips
// as a single-element flow sequence carrying v verbatim. The fast
// reject covers the common case cheaply; the oracle catches edge cases
// the character check would miss (whitespace-only strings that flow
// resolves to null, adjacent-indicator patterns that don't survive
// scalar-boundary parsing).
func SafeToUnquoteInFlow(v string) bool {
	if !SafeToUnquote(v) {
		return false
	}
	if strings.ContainsAny(v, "[]{},") {
		return false
	}
	file, err := parser.ParseBytes([]byte("["+v+"]"), 0)
	if err != nil || len(file.Docs) != 1 {
		return false
	}
	seq, ok := file.Docs[0].Body.(*ast.SequenceNode)
	if !ok || !seq.IsFlowStyle || len(seq.Values) != 1 {
		return false
	}
	sn, ok := seq.Values[0].(*ast.StringNode)
	if !ok {
		return false
	}
	return sn.Value == v
}

// Mirrors goccy's plain-scalar restrictions closely enough for width
// estimation. Not a substitute for goccy's real quoting decision - only
// used to size the record for the maxLineWidth comparison.
func NeedsQuoting(s string) bool {
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

// Breaks `s` across lines no wider than `width` display columns,
// splitting only on single spaces. Runs of two or more spaces stay
// atomic inside a segment - splitting a multi-space run would leave a
// wrapped line starting with whitespace, which folded-scalar semantics
// treat as "more indented" and preserve as a literal newline instead
// of folding to a space. Line width is measured via DisplayWidth so
// the wrap stays correct for CJK / emoji input, where a rune's byte
// length and its rendered column count differ.
func FoldWrap(s string, width int) []string {
	if s == "" || width < 1 {
		return []string{s}
	}
	atoms := SplitFoldAtoms(s)
	var lines []string
	var cur strings.Builder
	curWidth := 0
	for _, a := range atoms {
		aw := DisplayWidth(a)
		if cur.Len() == 0 {
			cur.WriteString(a)
			curWidth = aw
			continue
		}
		if curWidth+1+aw <= width {
			cur.WriteByte(' ')
			cur.WriteString(a)
			curWidth += 1 + aw
			continue
		}
		lines = append(lines, cur.String())
		cur.Reset()
		cur.WriteString(a)
		curWidth = aw
	}
	lines = append(lines, cur.String())
	return lines
}

// Splits value at single-space boundaries; multi-space runs stay
// atomic inside a chunk so folded-scalar semantics preserve them
// across the wrap. Callers use this to identify legal break points
// before line-budgeting the result.
func SplitFoldAtoms(s string) []string {
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

