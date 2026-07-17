package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"runtime/debug"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	"github.com/goccy/go-yaml/token"
)

//go:embed help.txt
var gnuHelpText []byte

//go:embed README.md
var readmeContent []byte

func printReadme(out io.Writer) error {
	fmt.Fprint(out, string(readmeContent))
	return nil
}

func printVersion(out io.Writer) error {
	if info, ok := debug.ReadBuildInfo(); ok {
		output, _ := json.MarshalIndent(info.Main, "", "  ")
		fmt.Fprintln(out, string(output))
	} else {
		fmt.Fprintln(out, "{}")
	}
	return nil
}

func printHelp(out io.Writer) error {
	fmt.Fprint(out, string(gnuHelpText))
	return nil
}

func main() {
	if err := run(os.Args, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 2 {
		arg := args[1]
		switch arg {
		case "--version", "-v":
			return printVersion(stdout)
		case "--man", "-m":
			return printReadme(stdout)
		case "--help", "-h":
			return printHelp(stdout)
		}
	}

	stdinBytes, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("error reading stdin: %w", err)
	}

	mergedDocs, err := mergeDataFiles(args[1:])
	if err != nil {
		return err
	}

	file, err := parser.ParseBytes(stdinBytes, parser.ParseComments)
	if err != nil {
		return fmt.Errorf("error parsing stdin yaml: %w", err)
	}

	// If data files were provided, they must cover every STDIN doc. Extra
	// data docs are ignored (a separate concern), but a shortfall is fatal:
	// silently leaving later stdin docs unupdated tends to hide config bugs.
	if len(args) > 1 && len(mergedDocs) < len(file.Docs) {
		return fmt.Errorf("stdin has %d documents but data covers only %d", len(file.Docs), len(mergedDocs))
	}

	if err := applyMergedDocs(file, mergedDocs); err != nil {
		return err
	}

	return writeOutput(stdout, file, stdinBytes, modifiedDocs(file, mergedDocs))
}

// modifiedDocs reports per-doc whether updates were applied. Untouched docs
// are passed through verbatim from the source to dodge goccy's cosmetic
// re-rendering (e.g. `{ }` -> `{}`).
func modifiedDocs(file *ast.File, mergedDocs []yaml.MapSlice) []bool {
	modified := make([]bool, len(file.Docs))
	for i := range file.Docs {
		if i < len(mergedDocs) && len(mergedDocs[i]) > 0 {
			modified[i] = true
		}
	}
	return modified
}

// applyMergedDocs scopes each doc into a temporary single-doc *ast.File
// because yaml.Path operations otherwise match every doc in the file at once.
// The DocumentNode pointer is shared, so mutations propagate back.
//
// Empty-stream and null/nil-body docs are lazily promoted to empty block
// mappings only when data has updates for that slot. Slots with no data
// keep their original body, so an unmodified `---\nnull` survives intact.
func applyMergedDocs(file *ast.File, mergedDocs []yaml.MapSlice) error {
	detectedStyle = detectStyle(file)
	if len(file.Docs) == 0 && len(mergedDocs) > 0 {
		file.Docs = append(file.Docs, newEmptyDoc())
	}
	for i, doc := range file.Docs {
		if i >= len(mergedDocs) || len(mergedDocs[i]) == 0 {
			continue
		}
		ensureMappingBody(doc)
		scoped := &ast.File{Docs: []*ast.DocumentNode{doc}}
		if err := updateAtNode(scoped, doc.Body, "$", mergedDocs[i]); err != nil {
			return err
		}
		// NOTE: monkey-patch for goccy render bugs. When a goccy release ships
		// the LiteralNode.String() TrimRight-strip fix for `|+`/`>+`, comment
		// out this line to disable the walker; the wrapper types then become
		// dead code but harmlessly stay compiled.
		patchLiteralNodeBugs(doc.Body)
	}
	return nil
}

// patchLiteralNodeBugs walks a modified doc and wraps every `|+`/`>+`
// LiteralNode so the trailing blank lines the `+` chomp is supposed to keep
// survive re-render. Goccy's own LiteralNode.String() does a bare
// TrimRight(origin, "\n") that strips exactly those blanks. The wrapper
// delegates to goccy for the rest of the render (folded wrap, comments,
// origin-preserved indent) and just appends the newlines the trim ate.
//
// This walker touches source-parsed nodes too - not to change their content,
// but to prevent goccy from silently dropping content it was asked to keep.
// The upstream fix is stuck in an unmerged pull request.
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

// keepChompString wraps an ast.StringNode we created via ValueToNode when
// its value carries embedded newlines. Goccy's StringNode.String() renders
// multi-line values by prepending `space+indent` to every content line
// including empty ones (producing "    \n" instead of "\n") and applies its
// own TrimSuffix that discards `+` chomp trailing blanks. Ours renders from
// the parsed value directly so both are preserved.
// keepChompString stores the raw Go string (`val`) alongside the wrapped
// node because goccy's StringNode.Value bakes surrounding quotes into the
// stored string for values that would need quoting in plain form. Reading
// that back would embed literal quotes inside a block scalar - so we render
// from the caller-supplied raw value instead. `useFolded` records the
// header choice made at wrap time so String() doesn't need to re-derive
// it (and doesn't need to know the key name).
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

// shouldFold picks `>` (folded) over `|` (literal). `>` only pays off when
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

// literalHeader / foldedHeader translate a trailing-newline count into the
// right chomp indicator for `|`-family and `>`-family block scalars.
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

// shouldWrapStringValue applies the block-form rule for updated string
// values. Wrap when either:
//   - value carries embedded newlines (plain can't represent them), or
//   - value is a single line long enough that its plain-form record exceeds
//     detectedStyle.maxLineWidth AND has whitespace to fold on AND has no leading or
//     trailing whitespace (block scalars strip both).
//
// Anything else stays plain, letting goccy render as it always did.
//
// NOTE: two motivations mixed here. The `strings.Contains(val, "\n")`
// early-return is a monkey-patch for goccy's StringNode multi-line render
// bugs; when a goccy release ships those fixes, delete that early-return so
// only the style-selection rule (the long-single-line path) remains active.
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

// plainRecordLen estimates the record's `<indent><key>: <value>` column
// count if val were emitted plain (quoted if content demands it). It only
// has to be exact around the detectedStyle.maxLineWidth boundary; a 2-char miss at the
// quoting decision changes plain-vs-block only at that exact 120/122 edge.
func plainRecordLen(val, key string, keyCol int) int {
	valLen := len(val)
	if needsQuoting(val) {
		valLen += 2
	}
	return (keyCol - 1) + len(key) + len(": ") + valLen
}

// needsQuoting mirrors goccy's plain-scalar restrictions closely enough for
// width estimation. Not a substitute for goccy's real quoting decision -
// only used to size the record for the detectedStyle.maxLineWidth comparison.
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

// foldWrap breaks `s` across lines no wider than `width`, splitting only on
// single spaces. Runs of two or more spaces stay atomic inside a segment -
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

// ensureMappingBody swaps a nil or null doc body for a fresh empty block
// mapping so subsequent updates have somewhere to land.
func ensureMappingBody(doc *ast.DocumentNode) {
	if doc.Body == nil {
		doc.Body = newEmptyBlockMapping()
		return
	}
	if _, isNull := doc.Body.(*ast.NullNode); isNull {
		doc.Body = newEmptyBlockMapping()
	}
}

// newEmptyDoc and newEmptyBlockMapping go through the parser rather than
// constructing AST nodes directly, so Start/End tokens and BaseNode are
// populated. An empty mapping renders as `{}` regardless of IsFlowStyle, but
// once entries are added the flag determines block vs flow output.
func newEmptyDoc() *ast.DocumentNode {
	f, _ := parser.ParseBytes([]byte("{}\n"), parser.ParseComments)
	d := f.Docs[0]
	d.Body.(*ast.MappingNode).IsFlowStyle = false
	return d
}

func newEmptyBlockMapping() *ast.MappingNode {
	f, _ := parser.ParseBytes([]byte("{}\n"), parser.ParseComments)
	mn := f.Docs[0].Body.(*ast.MappingNode)
	mn.IsFlowStyle = false
	return mn
}

type style struct {
	indent                    int  // spaces per indent level (block mappings)
	singleQuote               bool // prefer single quotes for values that need quoting
	maxLineWidth              int  // column budget: over this, plain scalars fold into `>` block form
	spacesBeforeInlineComment int  // gap between a scalar value and its inline `#` comment
}

// detectedStyle is set once at the top of applyMergedDocs and read by the
// marshalling helpers below. Package-level because run isn't reentrant; same
// pattern as the env var reads. Defaults align with goccy's own conventions:
// 2-space indent, 1 space before an inline comment, 120-column plain-scalar
// budget.
var detectedStyle = style{
	indent:                    2,
	maxLineWidth:              120,
	spacesBeforeInlineComment: 1,
}

// detectStyle uses first-occurrence wins: first parent->child mapping pair
// determines indent step, first explicitly-quoted string determines quote
// style. Reconciling mixed styles within a file is the linter's job.
//
// UPDATE_YAML_PREFER_SINGLE_QUOTE skips quote detection. Only affects values
// goccy already decided need quoting; plain strings stay plain.
func detectStyle(file *ast.File) style {
	// Start from the package-level defaults and override only what we
	// detect from the source, so all initial values live in exactly one
	// place (the `detectedStyle` initializer).
	s := detectedStyle
	for _, doc := range file.Docs {
		if n := findIndentInNode(doc.Body); n > 0 {
			s.indent = n
			break
		}
	}
	if os.Getenv("UPDATE_YAML_PREFER_SINGLE_QUOTE") != "" {
		s.singleQuote = true
		return s
	}
	for _, doc := range file.Docs {
		if t := findFirstQuote(doc.Body); t == token.SingleQuoteType {
			s.singleQuote = true
			break
		} else if t == token.DoubleQuoteType {
			break
		}
	}
	return s
}

func findFirstQuote(n ast.Node) token.Type {
	switch v := n.(type) {
	case *ast.StringNode:
		return v.Token.Type
	case *ast.MappingNode:
		for _, mv := range v.Values {
			if t := findFirstQuote(mv); isExplicitQuote(t) {
				return t
			}
		}
	case *ast.MappingValueNode:
		if t := findFirstQuote(v.Key); isExplicitQuote(t) {
			return t
		}
		return findFirstQuote(v.Value)
	case *ast.SequenceNode:
		for _, c := range v.Values {
			if t := findFirstQuote(c); isExplicitQuote(t) {
				return t
			}
		}
	case *ast.AnchorNode:
		return findFirstQuote(v.Value)
	}
	return token.UnknownType
}

func findIndentInNode(n ast.Node) int {
	mn, ok := n.(*ast.MappingNode)
	if !ok || mn.IsFlowStyle {
		return 0
	}
	for _, mv := range mn.Values {
		if child, ok := mv.Value.(*ast.MappingNode); ok && !child.IsFlowStyle && len(child.Values) > 0 {
			parentCol := mv.Key.GetToken().Position.Column
			childCol := child.Values[0].Key.GetToken().Position.Column
			if childCol > parentCol {
				return childCol - parentCol
			}
		}
		if found := findIndentInNode(mv.Value); found > 0 {
			return found
		}
	}
	return 0
}

// writeOutput uses goccy's serializer for modified docs and splices the
// original source bytes verbatim for untouched ones, so cosmetic round-trip
// changes (e.g. `{ }` -> `{}`, quote-style drift on keys never touched)
// don't leak into the output.
//
// Inter-doc blank lines are preserved by stripping ALL trailing newlines
// from each part and then joining with a separator computed from the
// source: one line break plus N extra newlines for N blank lines that
// preceded doc[i+1] in the input. Without this, a passthrough doc carrying
// trailing blanks loses them to the trim, and a modified doc never knew
// about the blanks in the first place.
func writeOutput(stdout io.Writer, file *ast.File, original []byte, modified []bool) error {
	if len(file.Docs) == 0 {
		return nil
	}
	starts := docByteStarts(file, original)
	lineOff := lineOffsets(original)

	parts := make([]string, len(file.Docs))
	for i, doc := range file.Docs {
		if modified[i] {
			parts[i] = strings.TrimRight(doc.String(), "\n")
			continue
		}
		parts[i] = strings.TrimRight(string(original[starts[i]:starts[i+1]]), "\n")
	}

	var sb strings.Builder
	sb.WriteString(parts[0])
	for i := 1; i < len(parts); i++ {
		blanks := blanksBeforeDoc(file.Docs[i], lineOff, original)
		sb.WriteString(strings.Repeat("\n", 1+blanks))
		sb.WriteString(parts[i])
	}
	sb.WriteByte('\n')
	_, err := fmt.Fprint(stdout, sb.String())
	return err
}

// blanksBeforeDoc counts consecutive blank source lines immediately above
// the doc's first-token line. A blank line is one containing only spaces
// and tabs.
func blanksBeforeDoc(d *ast.DocumentNode, lineOff []int, b []byte) int {
	line := docStartLine(d)
	count := 0
	for k := line - 1; k >= 1; k-- {
		if k >= len(lineOff) {
			break
		}
		lineStart := lineOff[k]
		lineEnd := len(b)
		if k+1 < len(lineOff) {
			lineEnd = lineOff[k+1]
		}
		if len(bytes.Trim(b[lineStart:lineEnd], " \t\r\n")) > 0 {
			break
		}
		count++
	}
	return count
}

// docByteStarts uses each doc's first-token line (which goccy reports
// reliably) instead of Position.Offset, which goccy can mis-account across
// block-literal scalars.
func docByteStarts(file *ast.File, original []byte) []int {
	starts := make([]int, len(file.Docs)+1)
	if len(starts) == 1 {
		starts[0] = len(original)
		return starts
	}
	lineOff := lineOffsets(original)
	for i, d := range file.Docs {
		switch line := docStartLine(d); {
		case line >= 1 && line < len(lineOff):
			starts[i] = lineOff[line]
		case line >= len(lineOff):
			starts[i] = len(original)
		}
	}
	starts[0] = 0
	starts[len(file.Docs)] = len(original)
	return starts
}

func docStartLine(d *ast.DocumentNode) int {
	if d.Start != nil {
		return d.Start.Position.Line
	}
	if d.Body != nil {
		return d.Body.GetToken().Position.Line
	}
	return 1
}

// lineOffsets is 1-based to match goccy's Position.Line; entry 0 is unused.
func lineOffsets(b []byte) []int {
	offs := make([]int, 2, 1+strings.Count(string(b), "\n")+1)
	offs[1] = 0
	for i, c := range b {
		if c == '\n' {
			offs = append(offs, i+1)
		}
	}
	return offs
}

// mergeDataFiles merges across files index-by-index. Files with fewer docs
// contribute nothing past their last doc.
func mergeDataFiles(files []string) ([]yaml.MapSlice, error) {
	maxLen := 0
	perFile := make([][]yaml.MapSlice, 0, len(files))
	for _, f := range files {
		docs, err := readMultiDoc(f)
		if err != nil {
			return nil, err
		}
		if len(docs) > maxLen {
			maxLen = len(docs)
		}
		perFile = append(perFile, docs)
	}
	if maxLen == 0 {
		return nil, nil
	}
	merged := make([]yaml.MapSlice, maxLen)
	for _, docs := range perFile {
		for i, d := range docs {
			merged[i] = mergeInto(merged[i], d)
		}
	}
	return merged, nil
}

func readMultiDoc(path string) ([]yaml.MapSlice, error) {
	r, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("error reading %s: %w", path, err)
	}
	defer r.Close()
	dec := yaml.NewDecoder(r, yaml.UseOrderedMap())
	var docs []yaml.MapSlice
	for {
		var m yaml.MapSlice
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("error parsing %s: %w", path, err)
		}
		docs = append(docs, m)
	}
	return docs, nil
}

// mergeInto: order is "first-occurrence wins"; values follow last-write-wins,
// including explicit nils, which the walker later interprets as "remove".
//
// I don't use dario.cat/mergo here. mergo.WithOverride skips zero values in
// src, so a later file's `key: null` silently fails to override an earlier
// non-nil value. mergo.WithOverwriteWithEmptyValue overrides with zero values,
// but treats nil and empty string / empty slice the same - so it would also
// erase keys whose final value happens to be `""` or `[]`, which is wrong.
// I need exactly nil-overrides-non-nil with no other changes to zero-value
// handling, so a small custom merge is clearer than tuning mergo flags.
func mergeInto(dst, src yaml.MapSlice) yaml.MapSlice {
	if len(dst) == 0 {
		return src
	}
	for _, sItem := range src {
		sk, ok := sItem.Key.(string)
		if !ok {
			dst = append(dst, sItem)
			continue
		}
		idx := mapSliceIndex(dst, sk)
		if idx < 0 {
			dst = append(dst, sItem)
			continue
		}
		if dSub, ok := dst[idx].Value.(yaml.MapSlice); ok {
			if sSub, ok := sItem.Value.(yaml.MapSlice); ok {
				dst[idx].Value = mergeInto(dSub, sSub)
				continue
			}
		}
		dst[idx].Value = sItem.Value
	}
	return dst
}

func mapSliceIndex(m yaml.MapSlice, key string) int {
	for i, item := range m {
		if k, ok := item.Key.(string); ok && k == key {
			return i
		}
	}
	return -1
}

func mapSliceLookup(m yaml.MapSlice, key string) (any, bool) {
	if i := mapSliceIndex(m, key); i >= 0 {
		return m[i].Value, true
	}
	return nil, false
}

// updateAtNode recurses on mapping-vs-mapping rather than replacing wholesale
// so nested comments and unmentioned keys are preserved. Explicit nil in data
// removes the corresponding key.
func updateAtNode(file *ast.File, node ast.Node, path string, data yaml.MapSlice) error {
	seen, toRemove, err := updateExistingEntries(file, node, path, data)
	if err != nil {
		return err
	}

	mn, ok := node.(*ast.MappingNode)
	if !ok {
		return nil
	}
	// Goccy's MappingNode.Start points at the ':' token of the first child,
	// not the key, so once Values is empty startPos() returns a column offset
	// by the key length and mn.Merge appends new entries where the colon was.
	// Snapshot the key column up front and restore it post-removal.
	var firstKeyCol int
	if len(mn.Values) > 0 {
		firstKeyCol = mn.Values[0].Key.GetToken().Position.Column
	}
	removeMarkedEntries(mn, toRemove)
	if len(mn.Values) == 0 && firstKeyCol > 0 && mn.Start != nil {
		mn.Start.Position.Column = firstKeyCol
	}
	return appendMissingEntries(mn, data, seen)
}

func updateExistingEntries(
	file *ast.File, node ast.Node, path string, data yaml.MapSlice,
) (map[string]bool, map[*ast.MappingValueNode]bool, error) {
	seen := make(map[string]bool, len(data))
	toRemove := make(map[*ast.MappingValueNode]bool)

	for _, mv := range mappingValues(node) {
		key := keyString(mv.Key)
		if key == "" {
			continue
		}
		val, ok := mapSliceLookup(data, key)
		if !ok {
			continue
		}
		seen[key] = true
		if val == nil {
			toRemove[mv] = true
			continue
		}
		if err := applyValue(file, mv, val, joinPath(path, key)); err != nil {
			return nil, nil, err
		}
	}

	return seen, toRemove, nil
}

// applyValue: recurses on mapping-vs-mapping and delegates all other
// replacements to replaceAt. AnchorNode wrappers are temporarily unwrapped
// because yaml.Path can't navigate through them; the wrapper is restored on
// return and its Value field still points to the (now-mutated) inner
// mapping, so the binding survives.
func applyValue(file *ast.File, mv *ast.MappingValueNode, val any, childPath string) error {
	sourceVal, restore := unwrapAnchor(mv)
	defer restore()

	if subMap, ok := val.(yaml.MapSlice); ok && mappingValues(sourceVal) != nil {
		return updateAtNode(file, sourceVal, childPath, subMap)
	}
	keyTok := mv.Key.GetToken()
	keyCol := keyTok.Position.Column
	// Snapshot the old value's LAST source-chain token before replaceAt
	// swaps mv.Value. yaml.Path.ReplaceWithNode doesn't rewire the token
	// chain, so the next sibling's Key.Prev keeps pointing at this token;
	// if we later wrap the new value with `+` trailing blanks, we bump this
	// token's line to MaxInt to suppress the checkLineBreak blank that
	// would otherwise stack on our wrapper's output.
	oldValTok := lastChainToken(mv.Value)
	if err := replaceAt(file, childPath, val, keyCol, keyTok.Position.IndentLevel); err != nil {
		return fmt.Errorf("replacing %s: %w", childPath, err)
	}
	// NOTE: partially monkey-patch for goccy render bugs. When a goccy
	// release ships StringNode.String()'s multi-line fixes (indent-on-empty
	// and the TrimSuffix that eats `+`-chomp trailing blanks), the multi-line
	// side of this block becomes unnecessary. The long-single-line side
	// implements the tool's own presentation rule (fold to `>` when a plain
	// value would exceed detectedStyle.maxLineWidth) - keep it either way.
	//
	// Data-updated values land as ast.StringNode (via ValueToNode). We wrap
	// ours to (a) fix goccy's multi-line render bugs and (b) apply the
	// block-form rule when the plain form would be too long. Source-parsed
	// StringNodes stay untouched.
	if rawStr, isStr := val.(string); isStr {
		if s, ok := mv.Value.(*ast.StringNode); ok && shouldWrapStringValue(rawStr, keyCol, keyString(mv.Key)) {
			mv.Value = &keepChompString{
				StringNode: s,
				val:        rawStr,
				keyCol:     keyCol,
				useFolded:  shouldFold(rawStr, keyString(mv.Key), keyCol),
			}
			// Only when our wrapper appends its own trailing blanks
			// (`+` chomp with 2+ trailing newlines) do we need to suppress
			// goccy's own checkLineBreak blank; for clip/strip goccy's
			// natural blank between records is the one we still want to keep.
			if trailingNewlineCount(rawStr) >= 2 && oldValTok != nil && oldValTok.Position != nil {
				oldValTok.Position.Line = math.MaxInt32
			}
		}
	}
	return nil
}

func unwrapAnchor(mv *ast.MappingValueNode) (ast.Node, func()) {
	anchor, ok := mv.Value.(*ast.AnchorNode)
	if !ok {
		return mv.Value, func() {}
	}
	mv.Value = anchor.Value
	return anchor.Value, func() { mv.Value = anchor }
}

func removeMarkedEntries(mn *ast.MappingNode, toRemove map[*ast.MappingValueNode]bool) {
	if len(toRemove) == 0 {
		return
	}
	filtered := mn.Values[:0]
	for i, mv := range mn.Values {
		if toRemove[mv] {
			// Goccy attaches the head comment (and its leading blank line)
			// to the removed node, so dropping the node erases the blank
			// too. Promote the blank line to a trailing marker on the
			// previous surviving sibling so it survives as a section break.
			// Skip the promotion when goccy will re-insert a blank on its
			// own (via checkLineBreak on the next surviving entry's first
			// token), otherwise we'd get two blanks stacked.
			if hasLeadingBlankLine(mv) && len(filtered) > 0 &&
				!nextSurvivorHasNaturalBlank(mn.Values, i+1, toRemove) {
				ensureBlankFoot(filtered[len(filtered)-1])
			}
			continue
		}
		filtered = append(filtered, mv)
	}
	mn.Values = filtered
}

// nextSurvivorHasNaturalBlank finds the next entry that will survive the
// removal pass and reports whether goccy will naturally emit a blank line
// above it - via the same checkLineBreak math it uses at render time.
// Returning true means we should skip promoting a FootComment on the
// preceding entry; goccy already covers the section break.
func nextSurvivorHasNaturalBlank(values []*ast.MappingValueNode, from int, toRemove map[*ast.MappingValueNode]bool) bool {
	for j := from; j < len(values); j++ {
		if toRemove[values[j]] {
			continue
		}
		return goccyWouldInsertBlank(firstEntryToken(values[j]))
	}
	return false
}

// firstEntryToken returns the token goccy uses when deciding whether to
// insert a blank line above this entry - the head comment start if there
// is one, else the key token.
func firstEntryToken(mv *ast.MappingValueNode) *token.Token {
	if cg := mv.GetComment(); cg != nil && len(cg.Comments) > 0 {
		return cg.Comments[0].Token
	}
	return mv.Key.GetToken()
}

// goccyWouldInsertBlank mirrors goccy's checkLineBreak for a single token:
// raw source-line diff minus any newlines carried in the previous token's
// origin. When the result is positive, goccy prepends a "\n" during render.
func goccyWouldInsertBlank(t *token.Token) bool {
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

// hasLeadingBlankLine reports whether the source had a blank line immediately
// above this entry. The entry's first source token is the head comment's
// opening token if there is one, else the key token; either way, a gap of
// more than one line to its Prev means an intervening blank line - the
// section break we want to keep across a removal.
func hasLeadingBlankLine(mv *ast.MappingValueNode) bool {
	var tok *token.Token
	if cg := mv.GetComment(); cg != nil && len(cg.Comments) > 0 {
		tok = cg.Comments[0].Token
	} else {
		tok = mv.Key.GetToken()
	}
	if tok == nil || tok.Prev == nil {
		return false
	}
	return tok.Position.Line-tok.Prev.Position.Line > 1
}

// ensureBlankFoot uses an empty FootComment as a marker that goccy renders
// as a trailing newline. No-op if a FootComment already exists.
func ensureBlankFoot(mv *ast.MappingValueNode) {
	if mv.FootComment != nil {
		return
	}
	mv.FootComment = &ast.CommentGroupNode{}
}

// appendMissingEntries:
//   - mn already has keys: splice each new key at its alphabetical position
//     among the surviving keys (typical YAML config style; readers expect
//     keys near the others they share a prefix with).
//   - mn is empty (freshly built / fully cleared): append in data order,
//     since there's no existing order to fit into.
//
// UPDATE_YAML_PREFER_ORDER_PRESERVED switches to data-order append for
// non-empty mappings too.
func appendMissingEntries(mn *ast.MappingNode, data yaml.MapSlice, seen map[string]bool) error {
	insertSorted := len(mn.Values) > 0 && os.Getenv("UPDATE_YAML_PREFER_ORDER_PRESERVED") == ""
	for _, item := range data {
		k, ok := item.Key.(string)
		if !ok || seen[k] || item.Value == nil {
			continue
		}
		extra, err := buildSingleEntryMapping(k, item.Value)
		if err != nil {
			return fmt.Errorf("building entry %q: %w", k, err)
		}
		if insertSorted {
			mergeSorted(mn, extra)
		} else {
			mn.Merge(extra)
		}
	}
	return nil
}

// mergeSorted is a single-entry variant of (*ast.MappingNode).Merge that
// splices instead of appending. The column-alignment dance mirrors what
// Merge does internally.
func mergeSorted(mn *ast.MappingNode, extra *ast.MappingNode) {
	if len(extra.Values) == 0 {
		return
	}
	if len(extra.Values) != 1 || len(mn.Values) == 0 {
		mn.Merge(extra)
		return
	}
	col := mn.Values[0].Key.GetToken().Position.Column - extra.Values[0].Key.GetToken().Position.Column
	extra.AddColumn(col)

	// Rightmost-less-than insertion. When the existing list is sorted this
	// matches binary insert; when it's not (the source author may have
	// chosen a non-alphabetical layout), the new key still lands in a
	// stable position without disrupting the existing order.
	newKey := keyString(extra.Values[0].Key)
	pos := 0
	for i, mv := range mn.Values {
		if keyString(mv.Key) < newKey {
			pos = i + 1
		}
	}
	mn.Values = append(mn.Values[:pos], append([]*ast.MappingValueNode{extra.Values[0]}, mn.Values[pos:]...)...)
}

func buildSingleEntryMapping(key string, val any) (*ast.MappingNode, error) {
	out, err := yaml.MarshalWithOptions(
		yaml.MapSlice{{Key: key, Value: val}},
		yaml.Indent(detectedStyle.indent),
		yaml.IndentSequence(true),
		yaml.UseSingleQuote(detectedStyle.singleQuote),
	)
	if err != nil {
		return nil, err
	}
	f, err := parser.ParseBytes(out, parser.ParseComments)
	if err != nil {
		return nil, err
	}
	if len(f.Docs) == 0 || f.Docs[0].Body == nil {
		return nil, fmt.Errorf("empty parse result")
	}
	switch b := f.Docs[0].Body.(type) {
	case *ast.MappingNode:
		return b, nil
	case *ast.MappingValueNode:
		return &ast.MappingNode{Values: []*ast.MappingValueNode{b}}, nil
	default:
		return nil, fmt.Errorf("unexpected body type %T", b)
	}
}

func mappingValues(node ast.Node) []*ast.MappingValueNode {
	switch n := node.(type) {
	case *ast.MappingNode:
		return n.Values
	case *ast.MappingValueNode:
		return []*ast.MappingValueNode{n}
	}
	return nil
}

func keyString(node ast.Node) string {
	if s, ok := node.(*ast.StringNode); ok {
		return s.Value
	}
	return node.String()
}

// joinPath uses goccy's reserved-char escape for non-identifier keys:
// `$.foo.'bar.baz-*'.hoge`, with `\` escaping `'` and `\` itself. Bracket
// form `$['key']` is rejected by goccy's PathString.
func joinPath(prefix, key string) string {
	if isSimpleIdent(key) {
		if prefix == "$" {
			return "$." + key
		}
		return prefix + "." + key
	}
	q := strings.ReplaceAll(key, `\`, `\\`)
	q = strings.ReplaceAll(q, "'", `\'`)
	return prefix + ".'" + q + "'"
}

func isSimpleIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if !isIdentRune(r, i == 0) {
			return false
		}
	}
	return true
}

func isIdentRune(r rune, isFirst bool) bool {
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

// replaceAt preserves the inline/trailing comment and explicit quote style
// across the swap; goccy's ReplaceWithNode otherwise drops the trailing
// comment and re-quotes scalars using its own heuristics. Sequence-indent
// style (flush vs. extra-indented) is also carried over from the source, so
// we don't churn whitespace on lists the caller didn't ask to reshape.
//
// keyCol/keyIndent are the source column and indent level of the mapping key
// this value belongs to. Pass -1 for either when the caller doesn't have the
// mapping context (e.g. sequence entries); alignment patching is skipped in
// that case.
func replaceAt(file *ast.File, path string, val any, keyCol, keyIndent int) error {
	p, err := yaml.PathString(path)
	if err != nil {
		return fmt.Errorf("invalid path %s: %w", path, err)
	}
	traceKey(keyCol, keyIndent)
	ctx := snapshotBeforeReplace(p, file, val, keyCol)
	traceValue("INPUT", val)

	// TODO: use node from data directly.
	newNode, err := yaml.ValueToNode(val,
		yaml.Indent(detectedStyle.indent),
		yaml.IndentSequence(ctx.seqIndent),
		yaml.UseSingleQuote(ctx.preferSingle),
		yaml.UseLiteralStyleIfMultiline(true),
	)
	if err != nil {
		return fmt.Errorf("marshalling new value: %w", err)
	}
	traceNode("NEW NODE", newNode)

	if err := p.ReplaceWithNode(file, newNode); err != nil {
		return err
	}
	restoreAfterReplace(p, file, ctx, keyCol, keyIndent)
	return nil
}

// replaceContext carries the pre-swap state that has to survive across
// goccy's ReplaceWithNode: the comment we intend to reattach, plus the
// encoding preferences (single-quote vs. double, sequence indent style)
// inferred from the source node's shape. Those preferences are handed to
// goccy at node-creation time - swapping a StringNode's Token.Type
// post-marshal would corrupt any content that relied on the original quote
// style's escaping rules (e.g. `"line\n"` -> `'line\n'` would turn the
// escape into a literal backslash-n).
type replaceContext struct {
	savedComment *ast.CommentGroupNode
	preferSingle bool
	seqIndent    bool
}

// snapshotBeforeReplace inspects the node currently at `p` and captures
// everything replaceAt needs after the swap. `seqIndent` defaults to true
// (goccy's own default) and only flips when the source is a block-style
// sequence flush against the parent key. `preferSingle` follows the source
// scalar's own quote style when it's explicitly quoted, otherwise falls
// back to the file-level detection.
func snapshotBeforeReplace(p *yaml.Path, file *ast.File, val any, keyCol int) replaceContext {
	ctx := replaceContext{
		seqIndent:    true,
		preferSingle: detectedStyle.singleQuote,
	}
	n := nodeAt(p, file)
	if n == nil {
		return ctx
	}
	traceNode("OLD VALUE", n)
	ctx.savedComment = n.GetComment()
	switch v := n.(type) {
	case *ast.StringNode:
		switch v.Token.Type {
		case token.SingleQuoteType:
			ctx.preferSingle = true
		case token.DoubleQuoteType:
			ctx.preferSingle = false
		}
	case *ast.SequenceNode:
		if _, ok := val.([]any); ok && !v.IsFlowStyle && len(v.Values) > 0 && keyCol > 0 {
			ctx.seqIndent = v.Start.Position.Column > keyCol
		}
	}
	return ctx
}

// restoreAfterReplace reattaches the pre-swap comment and re-aligns the new
// value token to the parent key's column. Skipping the alignment step when
// keyCol/keyIndent are -1 lets callers without mapping context (e.g. a
// hypothetical sequence-entry replacement) opt out cleanly. Quote style is
// already handled at marshal time via ctx.preferSingle, so nothing to
// restore here for that.
func restoreAfterReplace(p *yaml.Path, file *ast.File, ctx replaceContext, keyCol, keyIndent int) {
	n := nodeAt(p, file)
	if n == nil {
		return
	}
	if keyCol >= 0 && keyIndent >= 0 {
		// Workaround: goccy's ReplaceWithNode drops the parent key's indent
		// context for multi-line values, so we re-align the content token to
		// the key's column ourselves.
		realignToKey(n, keyCol, keyIndent)
	}
	if ctx.savedComment != nil {
		_ = n.SetComment(ctx.savedComment)
	}
	traceNode("NEW VALUE", n)
}

// realignToKey pushes the new value token's column/indent back onto the
// parent key's alignment. `>` / `|` block scalars land inside a LiteralNode
// whose Value holds the content token; plain/quoted values land directly in
// a StringNode. Any other node type has no scalar column to patch.
func realignToKey(n ast.Node, keyCol, keyIndent int) {
	var tok *token.Token
	switch v := n.(type) {
	case *ast.LiteralNode:
		tok = v.Value.Token
	case *ast.StringNode:
		tok = v.Token
	}
	if tok == nil {
		return
	}
	tok.Position.Column = keyCol
	tok.Position.IndentLevel = keyIndent
}

func nodeAt(p *yaml.Path, file *ast.File) ast.Node {
	n, err := p.FilterFile(file)
	if err != nil {
		return nil
	}
	return n
}

func isExplicitQuote(t token.Type) bool {
	return t == token.SingleQuoteType || t == token.DoubleQuoteType
}
