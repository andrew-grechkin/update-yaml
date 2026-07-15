package main

import (
	"bytes"
	crypto_rand "crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	var nonce [8]byte
	if _, err := crypto_rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("nonce: %w", err)
	}
	blockNonce = hex.EncodeToString(nonce[:])
	pendingBlocks = nil
	pendingSplices = nil

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
		preserveKeepChompBlocks(doc.Body)
	}
	finalizeBlockPlaceholders()
	return nil
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
	indent      int  // spaces per indent level (block mappings)
	singleQuote bool // prefer single quotes for values that need quoting
}

// detectedStyle is set once at the top of applyMergedDocs and read by the
// marshalling helpers below. Package-level because run isn't reentrant; same
// pattern as the env var reads.
var detectedStyle = style{indent: 2}

// maxLineWidth is the visible-column budget a single record must fit into
// for the value to stay in plain form. Over the budget, block scalars kick in
// (with the exceptions in chooseBlockStyle).
const maxLineWidth = 120

type blockChoice struct {
	folded bool // true = `>`, false = `|`
	chomp  byte // '-', 0 (clip / default), or '+'
}

// pendingBlocks maps unique placeholders to the pre-formatted block-scalar
// bytes that must be spliced into their positions after the AST is rendered.
// Goccy's LiteralNode round-trip drops trailing blank lines under `|+`/`>+`
// and never emits `>` on its own, so we sidestep its serializer entirely for
// block-form values: replace with a plain-scalar placeholder, then swap the
// placeholder for the correct block scalar in the final byte stream.
var pendingBlocks map[string]string

// blockNonce is a per-run random token that guards the placeholder namespace
// against colliding with strings in the source. Regenerated in run() so unit
// tests inside a single process don't share tokens across invocations.
var blockNonce string

// pendingSplices records the token-chain edits that must happen AFTER
// removeMarkedEntries. Rewiring the chain earlier confuses the removal pass's
// hasLeadingBlankBeforeEntry check: the deleted entry's key.Prev would then
// point at our placeholder rather than the natural source predecessor, and
// the check misreads the source's blank-line intent.
type pendingSplice struct {
	first, last *token.Token
	placeholder *token.Token
	bytesCount  int // \n count in the block bytes, drives placeholder Line
}

var pendingSplices []pendingSplice

// detectStyle uses first-occurrence wins: first parent->child mapping pair
// determines indent step, first explicitly-quoted string determines quote
// style. Reconciling mixed styles within a file is the linter's job.
//
// UPDATE_YAML_PREFER_SINGLE_QUOTE skips quote detection. Only affects values
// goccy already decided need quoting; plain strings stay plain.
func detectStyle(file *ast.File) style {
	s := style{indent: 2}
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

// chooseBlockStyle applies the presentation rule for a string value: block
// form when the plain rendering would exceed maxLineWidth, or when the value
// carries newlines that plain form cannot represent. Three exceptions fall
// back to whatever goccy would emit (plain or single-quoted):
//   - leading whitespace: block scalars strip it
//   - trailing whitespace on the last content line: block scalars strip it too
//   - no whitespace anywhere: nothing to fold on
//
// Returns (nil, ...) when goccy should handle it; otherwise the folded/chomp
// choice governs the raw bytes we splice.
func chooseBlockStyle(s, key string, keyCol int) *blockChoice {
	if hasLeadingScalarWS(s) || hasTrailingScalarWS(strings.TrimRight(s, "\n")) {
		return nil
	}
	trimmed := strings.TrimRight(s, "\n")
	trailNL := len(s) - len(trimmed)
	hasMidNL := strings.Contains(trimmed, "\n")
	hasFoldSpace := strings.ContainsAny(trimmed, " \t")

	plainLen := plainRecordLen(s, key, keyCol)
	if !hasMidNL && trailNL == 0 {
		if !hasFoldSpace {
			return nil // no fold point available
		}
		if plainLen <= maxLineWidth {
			return nil // fits as plain
		}
	}

	// `>` folded is only useful when content actually needs wrapping: it must
	// be a single logical line whose plain rendering exceeds maxLineWidth and
	// has whitespace to break at. Otherwise `|` literal keeps the source shape
	// without folding surprises.
	c := blockChoice{folded: !hasMidNL && hasFoldSpace && plainLen > maxLineWidth}
	switch {
	case trailNL == 0:
		c.chomp = '-'
	case trailNL >= 2:
		c.chomp = '+'
	}
	return &c
}

func hasLeadingScalarWS(s string) bool {
	return s != "" && (s[0] == ' ' || s[0] == '\t')
}

func hasTrailingScalarWS(trimmed string) bool {
	if trimmed == "" {
		return false
	}
	c := trimmed[len(trimmed)-1]
	return c == ' ' || c == '\t'
}

// plainRecordLen estimates the total column count of a `<indent><key>: <value>`
// record if `s` were emitted as a plain scalar (quoted where content forces
// quoting). Used only to compare against maxLineWidth; exact goccy escaping
// isn't needed because block form is picked well before quoting subtleties
// change the answer.
func plainRecordLen(s, key string, keyCol int) int {
	valLen := len(s)
	if needsQuoting(s) {
		valLen += 2
	}
	return (keyCol - 1) + len(key) + len(": ") + valLen
}

// needsQuoting reports whether goccy would emit a plain scalar wrapped in
// quotes. The list matches the plain-scalar restrictions in the YAML 1.2 spec
// closely enough for width estimation - values that trip a subtle case here
// only get 2 chars mis-estimated, which changes the plain-vs-block decision
// only at the exact 120/122 boundary.
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

// buildBlockScalar produces the raw bytes for a `|`/`>` scalar with the given
// chomp, indented for a value at the target column. The result includes the
// header line and content lines but NO trailing newline: the surrounding
// placeholder line's newline provides the terminator when we splice.
func buildBlockScalar(s string, c blockChoice, keyCol int) string {
	trimmed := strings.TrimRight(s, "\n")
	trailNL := len(s) - len(trimmed)
	contentIndent := strings.Repeat(" ", (keyCol-1)+detectedStyle.indent)

	var header strings.Builder
	if c.folded {
		header.WriteByte('>')
	} else {
		header.WriteByte('|')
	}
	if c.chomp != 0 {
		header.WriteByte(c.chomp)
	}

	var lines []string
	if c.folded {
		lines = foldWrap(trimmed, max(1, maxLineWidth-len(contentIndent)))
	} else {
		lines = strings.Split(trimmed, "\n")
	}

	var sb strings.Builder
	sb.WriteString(header.String())
	for _, ln := range lines {
		sb.WriteByte('\n')
		if ln != "" {
			sb.WriteString(contentIndent)
			sb.WriteString(ln)
		}
	}
	// Emit (trailNL - 1) additional bare newlines when chomp keeps trailing
	// blanks. The `-1` accounts for the placeholder line's own newline, which
	// contributes the last one after splicing.
	if c.chomp == '+' && trailNL >= 2 {
		for i := 0; i < trailNL-1; i++ {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// foldWrap breaks `s` into lines no wider than `width`. It first splits into
// "atoms": maximal substrings joined only by multi-space runs. Only the
// single-space gaps between atoms are wrap-safe - breaking inside a
// multi-space run would leave a line beginning with whitespace, which
// folded-scalar semantics treat as a "more indented" line and preserve the
// newline instead of folding to a space.
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

// splitFoldAtoms splits `s` on single-space boundaries only. Runs of two or
// more spaces stay inside the surrounding atom, so the wrapper never picks
// a break point that would split a multi-space region.
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

// stageBlockScalar registers a placeholder for later byte-level substitution
// and returns it as a plain scalar goccy can round-trip without quoting.
func stageBlockScalar(bytesToSplice string) string {
	if pendingBlocks == nil {
		pendingBlocks = make(map[string]string)
	}
	ph := fmt.Sprintf("__uy_%s_%d__", blockNonce, len(pendingBlocks))
	pendingBlocks[ph] = bytesToSplice
	return ph
}

// installBlockPlaceholder swaps the value at `mv` for a plain-scalar
// placeholder whose bytes are substituted for `blockBytes` after render.
// The token-chain splice is deferred to finalizeBlockPlaceholders so it
// doesn't run before removeMarkedEntries reads the source token layout.
func installBlockPlaceholder(mv *ast.MappingValueNode, blockBytes string) {
	first, last := scalarTokenSpan(mv.Value)
	sn := newPlaceholderNode(stageBlockScalar(blockBytes))
	if sn == nil {
		return
	}
	pendingSplices = append(pendingSplices, pendingSplice{
		first:       first,
		last:        last,
		placeholder: sn.Token,
		bytesCount:  strings.Count(blockBytes, "\n"),
	})
	mv.Value = sn
}

// finalizeBlockPlaceholders replays the recorded splices after every AST
// mutation is settled. Position is set to first.Line + newline count in the
// block bytes - the line where the block scalar's last content line lands
// in the output. That value keeps goccy's checkLineBreak crediting the
// multi-line span correctly, so an inter-record blank fires only when the
// source had one after the block.
func finalizeBlockPlaceholders() {
	for _, s := range pendingSplices {
		spliceTokens(s.first, s.last, s.placeholder)
		s.placeholder.Position = &token.Position{
			Line:        s.first.Position.Line + s.bytesCount,
			Column:      s.first.Position.Column,
			IndentNum:   s.first.Position.IndentNum,
			IndentLevel: s.first.Position.IndentLevel,
			Offset:      s.first.Position.Offset,
		}
	}
	pendingSplices = nil
}

// scalarTokenSpan returns the outermost pair of tokens the value node
// occupies in the source stream. Callers use it to splice a replacement
// token in without leaving orphan tokens whose Prev/Next still reference
// the removed node.
func scalarTokenSpan(n ast.Node) (*token.Token, *token.Token) {
	switch v := n.(type) {
	case *ast.LiteralNode:
		if v.Value == nil {
			return v.Start, v.Start
		}
		return v.Start, v.Value.Token
	case *ast.StringNode:
		return v.Token, v.Token
	}
	t := n.GetToken()
	return t, t
}

func spliceTokens(first, last, replacement *token.Token) {
	replacement.Prev = first.Prev
	replacement.Next = last.Next
	if replacement.Prev != nil {
		replacement.Prev.Next = replacement
	}
	if replacement.Next != nil {
		replacement.Next.Prev = replacement
	}
}

// newPlaceholderNode returns a fresh plain-scalar StringNode carrying
// `value`. Round-tripping through the parser wires Prev/Next correctly on
// the internal token so subsequent splicing has a real node to graft.
func newPlaceholderNode(value string) *ast.StringNode {
	f, err := parser.ParseBytes([]byte("k: "+value+"\n"), parser.ParseComments)
	if err != nil {
		return nil
	}
	sn, ok := f.Docs[0].Body.(*ast.MappingNode).Values[0].Value.(*ast.StringNode)
	if !ok {
		return nil
	}
	return sn
}

// preserveKeepChompBlocks walks the modified doc's AST and rewrites every
// `|+`/`>+` LiteralNode as a placeholder plain scalar backed by our own
// pre-formatted bytes. Goccy's LiteralNode.String() applies a bare
// TrimRight(origin, "\n") that discards every trailing newline, silently
// stripping the very content `+` chomp is meant to preserve. Runs after
// updates so already-swapped values aren't visited twice.
func preserveKeepChompBlocks(node ast.Node) {
	switch n := node.(type) {
	case *ast.MappingNode:
		for _, mv := range n.Values {
			preserveKeepChompBlocks(mv)
		}
	case *ast.MappingValueNode:
		if lit, ok := n.Value.(*ast.LiteralNode); ok && isKeepChomp(lit) {
			c := blockChoice{folded: lit.Start.Value[0] == '>', chomp: '+'}
			installBlockPlaceholder(n, buildBlockScalar(lit.Value.Value, c, n.Key.GetToken().Position.Column))
			return
		}
		preserveKeepChompBlocks(n.Value)
	case *ast.SequenceNode:
		for _, v := range n.Values {
			preserveKeepChompBlocks(v)
		}
	case *ast.AnchorNode:
		preserveKeepChompBlocks(n.Value)
	}
}

func isKeepChomp(lit *ast.LiteralNode) bool {
	if lit.Start == nil || lit.Value == nil {
		return false
	}
	header := strings.TrimSpace(lit.Start.Value)
	return len(header) >= 2 && header[1] == '+'
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
	out := substitutePendingBlocks(sb.String())
	_, err := fmt.Fprint(stdout, out)
	return err
}

// substitutePendingBlocks swaps each placeholder plain scalar for the raw
// block-scalar bytes we staged in applyValue. Empty is a no-op.
func substitutePendingBlocks(s string) string {
	for ph, block := range pendingBlocks {
		s = strings.Replace(s, ph, block, 1)
	}
	return s
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

// applyValue: the seqIndent dance preserves the source's flush vs.
// extra-indented sequence style on replace. AnchorNode wrappers are
// temporarily unwrapped because yaml.Path can't navigate through them; the
// wrapper is restored on return and its Value field still points to the
// (now-mutated) inner mapping, so the binding survives.
func applyValue(file *ast.File, mv *ast.MappingValueNode, val any, childPath string) error {
	sourceVal, restore := unwrapAnchor(mv)
	defer restore()

	if subMap, ok := val.(yaml.MapSlice); ok && mappingValues(sourceVal) != nil {
		return updateAtNode(file, sourceVal, childPath, subMap)
	}
	if s, ok := val.(string); ok {
		keyCol := mv.Key.GetToken().Position.Column
		if c := chooseBlockStyle(s, keyString(mv.Key), keyCol); c != nil {
			installBlockPlaceholder(mv, buildBlockScalar(s, *c, keyCol))
			return nil
		}
	}
	seqIndent := true
	if seq, ok := sourceVal.(*ast.SequenceNode); ok {
		if _, ok := val.([]any); ok && !seq.IsFlowStyle && len(seq.Values) > 0 {
			seqIndent = seq.Start.Position.Column > mv.Key.GetToken().Position.Column
		}
	}
	if err := replaceAt(file, childPath, val, seqIndent); err != nil {
		return fmt.Errorf("replacing %s: %w", childPath, err)
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
			// previous surviving sibling so it survives as a section break -
			// but only when goccy's own token-gap math wouldn't already
			// render a blank there, or we'd double it up.
			if hasLeadingBlankBeforeEntry(mv) && len(filtered) > 0 &&
				!nextSurvivorHasNaturalBlank(mn.Values, i+1, toRemove) &&
				!prevHasBlockTrailingBlanks(filtered[len(filtered)-1]) {
				ensureBlankFoot(filtered[len(filtered)-1])
			}
			continue
		}
		filtered = append(filtered, mv)
	}
	mn.Values = filtered
}

// prevHasBlockTrailingBlanks reports whether the preceding surviving entry
// carries a pending block-scalar whose bytes end with a newline - i.e., a
// `|+`/`>+` with 2+ trailing newlines in its parsed value. The block's own
// trailing blank lines already serve as the visual record separator, so
// adding a FootComment blank on top would double it up.
func prevHasBlockTrailingBlanks(mv *ast.MappingValueNode) bool {
	sn, ok := mv.Value.(*ast.StringNode)
	if !ok {
		return false
	}
	bytes, ok := pendingBlocks[sn.Token.Value]
	if !ok {
		return false
	}
	return strings.HasSuffix(bytes, "\n")
}

// nextSurvivorHasNaturalBlank looks ahead past additional deletions to find
// the next surviving entry and asks whether goccy's checkLineBreak would
// insert a blank line above it. When it would, we skip the FootComment
// promotion; otherwise the blank between records disappears with the removed
// entry.
func nextSurvivorHasNaturalBlank(values []*ast.MappingValueNode, from int, toRemove map[*ast.MappingValueNode]bool) bool {
	for j := from; j < len(values); j++ {
		if toRemove[values[j]] {
			continue
		}
		return firstTokenHasSourceBlank(values[j])
	}
	return false
}

// firstTokenHasSourceBlank mirrors goccy's checkLineBreak for the first
// source token of an entry (head comment start if any, else key). Returns
// true when the raw line gap - after crediting multi-line tokens preceding
// it - is > 0, matching the exact condition goccy uses to emit a blank.
func firstTokenHasSourceBlank(mv *ast.MappingValueNode) bool {
	var t *token.Token
	if cg := mv.GetComment(); cg != nil && len(cg.Comments) > 0 {
		t = cg.Comments[0].Token
	}
	if t == nil {
		t = mv.Key.GetToken()
	}
	if t == nil || t.Prev == nil {
		return false
	}
	prev := t.Prev
	lineDiff := t.Position.Line - prev.Position.Line - 1
	if lineDiff <= 0 {
		return false
	}
	adjustment := 0
	if prev.Type == token.StringType {
		adjustment = strings.Count(strings.TrimRight(strings.TrimSpace(prev.Origin), "\n"), "\n")
	}
	return lineDiff-adjustment > 0
}

// hasLeadingBlankBeforeEntry reports whether there is a blank source line
// immediately preceding this mapping entry - the section break a human uses
// for visual grouping. The entry's first source token is the head comment's
// opening line if there is one, else the key line.
func hasLeadingBlankBeforeEntry(mv *ast.MappingValueNode) bool {
	var firstTok *token.Token
	if cg := mv.GetComment(); cg != nil && len(cg.Comments) > 0 {
		firstTok = cg.Comments[0].Token
	}
	if firstTok == nil {
		firstTok = mv.Key.GetToken()
	}
	if firstTok == nil || firstTok.Prev == nil {
		return false
	}
	return firstTok.Position.Line-firstTok.Prev.Position.Line > 1
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
// across the swap; goccy's ReplaceWithReader otherwise drops the trailing
// comment and re-quotes scalars using its own heuristics.
func replaceAt(file *ast.File, path string, val any, seqIndent bool) error {
	out, err := yaml.MarshalWithOptions(
		val,
		yaml.Indent(detectedStyle.indent),
		yaml.IndentSequence(seqIndent),
		yaml.UseSingleQuote(detectedStyle.singleQuote),
	)
	if err != nil {
		return fmt.Errorf("marshalling new value: %w", err)
	}
	p, err := yaml.PathString(path)
	if err != nil {
		return fmt.Errorf("invalid path %s: %w", path, err)
	}

	var (
		savedComment *ast.CommentGroupNode
		savedQuote   token.Type
	)
	if n := nodeAt(p, file); n != nil {
		savedComment = n.GetComment()
		if s, ok := n.(*ast.StringNode); ok {
			savedQuote = s.Token.Type
		}
	}
	if err := p.ReplaceWithReader(file, bytes.NewReader(out)); err != nil {
		return err
	}
	if n := nodeAt(p, file); n != nil {
		if savedComment != nil {
			_ = n.SetComment(savedComment)
		}
		if s, ok := n.(*ast.StringNode); ok && isExplicitQuote(savedQuote) && isExplicitQuote(s.Token.Type) {
			s.Token.Type = savedQuote
		}
	}
	return nil
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
