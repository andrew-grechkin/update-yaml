package main

import (
	"bytes"
	_ "embed"
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
	}
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

// applyValue: the seqIndent dance preserves the source's flush vs.
// extra-indented sequence style on replace. AnchorNode wrappers are
// temporarily unwrapped because yaml.Path can't navigate through them; the
// wrapper is restored on return and its Value field still points to the
// (now-mutated) inner mapping, so the binding survives.
func applyValue(file *ast.File, mv *ast.MappingValueNode, val any, childPath string) error {
	targetVal, restore := unwrapAnchor(mv)
	defer restore()

	if subMap, ok := val.(yaml.MapSlice); ok && mappingValues(targetVal) != nil {
		return updateAtNode(file, targetVal, childPath, subMap)
	}
	seqIndent := true
	if seq, ok := targetVal.(*ast.SequenceNode); ok {
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
	for _, mv := range mn.Values {
		if toRemove[mv] {
			// Goccy attaches the head comment (and its leading blank line)
			// to the removed node, so dropping the node erases the blank
			// too. Promote the blank line to a trailing marker on the
			// previous surviving sibling so it survives as a section break.
			if hasLeadingBlankLine(mv) && len(filtered) > 0 {
				ensureBlankFoot(filtered[len(filtered)-1])
			}
			continue
		}
		filtered = append(filtered, mv)
	}
	mn.Values = filtered
}

func hasLeadingBlankLine(mv *ast.MappingValueNode) bool {
	cg := mv.GetComment()
	if cg == nil || len(cg.Comments) == 0 {
		return false
	}
	tok := cg.Comments[0].Token
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
