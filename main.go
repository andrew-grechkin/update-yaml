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

// modifiedDocs reports per-document whether updates were applied. A document
// is considered modified if data exists for that index and the data has at
// least one key. Untouched docs are passed through verbatim from the source
// to avoid goccy's cosmetic re-rendering (e.g. `{ }` → `{}`).
func modifiedDocs(file *ast.File, mergedDocs []yaml.MapSlice) []bool {
	modified := make([]bool, len(file.Docs))
	for i := range file.Docs {
		if i < len(mergedDocs) && len(mergedDocs[i]) > 0 {
			modified[i] = true
		}
	}
	return modified
}

// applyMergedDocs walks file.Docs and applies merged data per-index. Each doc
// is updated through a temporary single-doc *ast.File wrapping the same
// DocumentNode pointer, because yaml.Path operations otherwise match every
// doc in the file at once. Mutations propagate back to file.
func applyMergedDocs(file *ast.File, mergedDocs []yaml.MapSlice) error {
	indent := detectIndent(file)
	for i, doc := range file.Docs {
		if doc.Body == nil {
			continue
		}
		if i >= len(mergedDocs) || len(mergedDocs[i]) == 0 {
			continue
		}
		scoped := &ast.File{Docs: []*ast.DocumentNode{doc}}
		if err := updateAtNode(scoped, doc.Body, "$", mergedDocs[i], indent); err != nil {
			return err
		}
	}
	return nil
}

// detectIndent infers the block-mapping indent (in spaces) used in the source
// by scanning the AST for the first parent→child mapping pair and measuring
// the column gap. Defaults to 2 when nothing decisive is found, so newly
// synthesized entries match the source's visual style.
func detectIndent(file *ast.File) int {
	const def = 2
	for _, doc := range file.Docs {
		if n := findIndentInNode(doc.Body); n > 0 {
			return n
		}
	}
	return def
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

// writeOutput renders the result. For docs flagged as modified it uses
// goccy's serializer; for untouched docs it splices the original source bytes
// verbatim, so cosmetic round-trip changes (e.g. `{ }` → `{}`, quote-style
// drift on keys never touched) don't leak into the output
func writeOutput(stdout io.Writer, file *ast.File, original []byte, modified []bool) error {
	parts := make([]string, len(file.Docs))
	starts := docByteStarts(file, original)
	for i, doc := range file.Docs {
		if modified[i] {
			parts[i] = doc.String()
			continue
		}
		parts[i] = strings.TrimRight(string(original[starts[i]:starts[i+1]]), "\n")
	}
	out := strings.Join(parts, "\n")
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	_, err := fmt.Fprint(stdout, out)
	return err
}

// docByteStarts returns N+1 byte offsets such that doc i occupies
// original[s[i]:s[i+1]]. Boundaries are computed from each doc's first-token
// line (which goccy reports reliably) translated to byte offsets via a line
// table built from the source. Token Position.Offset is unreliable here:
// goccy can mis-account it across block-literal scalars, so don't use it.
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

// lineOffsets returns a table where entry i is the byte offset of the start
// of line i (1-based). Entry 0 is unused.
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

// mergeDataFiles reads each file as multi-document YAML and merges across
// files index-by-index: result[i] is the merge of all files' i-th document.
// Files with fewer docs contribute nothing past their last doc.
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

// mergeInto merges src into dst. Order is "first-occurrence wins": existing
// dst keys keep their slot; new src keys are appended at the end. Values
// follow last-write-wins semantics, including explicit nils, which the walker
// later interprets as "remove this key from the output".
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

// updateAtNode walks a mapping node and updates values for keys present in
// data. When both sides are mappings I recurse instead of replacing wholesale,
// so that nested comments and keys not mentioned in data are preserved.
// Keys present in data but absent from the mapping are appended at the end
// (recursively, for nested mappings) in alphabetical order. An explicit nil
// value in data removes the corresponding key from the output (or skips
// appending if the key did not exist).
func updateAtNode(file *ast.File, node ast.Node, path string, data yaml.MapSlice, indent int) error {
	seen, toRemove, err := updateExistingEntries(file, node, path, data, indent)
	if err != nil {
		return err
	}

	mn, ok := node.(*ast.MappingNode)
	if !ok {
		return nil
	}
	// Snapshot the first-key column before removal. Goccy's MappingNode.Start
	// points at the ':' token of its first child, not the key, so once Values
	// is empty, startPos() returns a column that's offset by the key length.
	// That throws off mn.Merge's column math then append a new entry,
	// indenting it to where the colon used to be.
	var firstKeyCol int
	if len(mn.Values) > 0 {
		firstKeyCol = mn.Values[0].Key.GetToken().Position.Column
	}
	removeMarkedEntries(mn, toRemove)
	if len(mn.Values) == 0 && firstKeyCol > 0 && mn.Start != nil {
		mn.Start.Position.Column = firstKeyCol
	}
	return appendMissingEntries(mn, data, seen, indent)
}

// updateExistingEntries iterates the mapping's existing entries and either
// replaces, recurses into, or marks them for removal based on data. Returns
// the set of keys it touched and the set of MappingValueNodes to remove.
func updateExistingEntries(
	file *ast.File, node ast.Node, path string, data yaml.MapSlice, indent int,
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
		if err := applyValue(file, mv, val, joinPath(path, key), indent); err != nil {
			return nil, nil, err
		}
	}
	return seen, toRemove, nil
}

// applyValue updates mv to reflect val: recurses into a sub-mapping when
// both sides are mappings, otherwise replaces the scalar/sequence value.
// When both old and new are sequences, the original sequence's indent style
// (flush vs. extra-indented entries) is preserved; otherwise default to
// extra-indented, which is the more readable form for newly-emitted YAML.
// AnchorNode wrappers (`key: &name <value>`) are temporarily unwrapped so
// yaml.Path can navigate into the inner mapping (it can't see through
// anchors); the wrapper is reattached after the recursion, and since its
// Value field points to the same (now-mutated) mapping the binding survives.
func applyValue(file *ast.File, mv *ast.MappingValueNode, val any, childPath string, indent int) error {
	targetVal, restore := unwrapAnchor(mv)
	defer restore()

	if subMap, ok := val.(yaml.MapSlice); ok && mappingValues(targetVal) != nil {
		return updateAtNode(file, targetVal, childPath, subMap, indent)
	}
	seqIndent := true
	if seq, ok := targetVal.(*ast.SequenceNode); ok {
		if _, ok := val.([]any); ok && !seq.IsFlowStyle && len(seq.Values) > 0 {
			seqIndent = seq.Start.Position.Column > mv.Key.GetToken().Position.Column
		}
	}
	if err := replaceAt(file, childPath, val, indent, seqIndent); err != nil {
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
			// Goccy attaches the head comment (and any blank line preceding
			// it) to the removed node, so dropping the node erases the blank
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

// hasLeadingBlankLine reports whether the node's head comment is preceded by
// a blank line in the source (i.e., the comment's first token sits more than
// one line below the previous token).
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

// ensureBlankFoot attaches an empty FootComment so the node renders with a
// trailing newline. Skipped when the node already has a FootComment.
func ensureBlankFoot(mv *ast.MappingValueNode) {
	if mv.FootComment != nil {
		return
	}
	mv.FootComment = &ast.CommentGroupNode{}
}

// appendMissingEntries adds entries for keys present in data but absent from
// the mapping (and not nil - explicit nils don't materialize new keys).
// Iterates data in its declared order so the appended keys mirror how the
// user wrote them in the source data files.
func appendMissingEntries(mn *ast.MappingNode, data yaml.MapSlice, seen map[string]bool, indent int) error {
	for _, item := range data {
		k, ok := item.Key.(string)
		if !ok || seen[k] || item.Value == nil {
			continue
		}
		extra, err := buildSingleEntryMapping(k, item.Value, indent)
		if err != nil {
			return fmt.Errorf("building entry %q: %w", k, err)
		}
		mn.Merge(extra)
	}
	return nil
}

func buildSingleEntryMapping(key string, val any, indent int) (*ast.MappingNode, error) {
	out, err := yaml.MarshalWithOptions(yaml.MapSlice{{Key: key, Value: val}}, yaml.Indent(indent), yaml.IndentSequence(true))
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

// joinPath builds a goccy YAMLPath. Simple identifiers use dotted form;
// anything else is wrapped in single quotes (goccy's reserved-char escape:
// `$.foo.'bar.baz-*'.hoge`, with `\` escaping `'` and `\` itself).
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

// replaceAt replaces the value at path with the marshalled form of val,
// preserving any inline/trailing comment that was on the old value and the
// original scalar quoting style. goccy's ReplaceWithReader otherwise drops
// the trailing comment and re-quotes scalars using its own heuristics.
func replaceAt(file *ast.File, path string, val any, indent int, seqIndent bool) error {
	out, err := yaml.MarshalWithOptions(val, yaml.Indent(indent), yaml.IndentSequence(seqIndent))
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
