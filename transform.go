package main

// Transform: apply data (as AST) onto source (also AST). This is the core
// update loop: walk data's mapping tree, and for every leaf key present in
// data either replace the source value, remove it (when data value is
// null), or recurse (when both source and data have MappingNode values).
// New keys from data are appended in data-tree order.

import (
	"fmt"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/token"
)

// Walks each source doc in parallel with the merged data doc for that
// slot. Slots with no data are skipped, so unmodified docs pass through
// as-is.
func applyMergedDocs(file *ast.File, mergedDocs []*ast.DocumentNode) error {
	if len(file.Docs) == 0 && len(mergedDocs) > 0 && !isEmptyDataDoc(mergedDocs[0]) {
		file.Docs = append(file.Docs, newEmptyDoc())
	}
	for i, doc := range file.Docs {
		if i >= len(mergedDocs) || isEmptyDataDoc(mergedDocs[i]) {
			continue
		}
		ensureMappingBody(doc)
		scoped := &ast.File{Docs: []*ast.DocumentNode{doc}}
		dataMap, ok := mergedDocs[i].Body.(*ast.MappingNode)
		if !ok {
			continue
		}
		if err := updateAtNode(scoped, doc.Body, "$", dataMap); err != nil {
			return err
		}
		// NOTE: monkey-patch for goccy render bugs. See the removal
		// checklist in patch_goccy.go.
		patchLiteralNodeBugs(doc.Body)
	}
	return nil
}

// Recurses on mapping-vs-mapping rather than replacing wholesale so
// nested comments and unmentioned keys are preserved. Explicit
// *ast.NullNode value in data removes the corresponding key. New keys are
// placed per the sort-vs-append rule in appendMissingEntries.
func updateAtNode(file *ast.File, sourceNode ast.Node, path string, data *ast.MappingNode) error {
	seen, toRemove, err := updateExistingEntries(file, sourceNode, path, data)
	if err != nil {
		return err
	}

	mn, ok := sourceNode.(*ast.MappingNode)
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
	if err := appendMissingEntries(mn, data, seen); err != nil {
		return err
	}
	// A block mapping we've fully emptied via null-removal (and appended
	// nothing new to) renders as a broken `parent:\n{}` under goccy's
	// rules because the parent's MappingValueNode.toString() picks the
	// "value on next line" branch before the "empty map" branch. Force
	// flow style so it lands as `parent: {}` on one line. Guarded on
	// `len(toRemove) > 0` so a map that started empty (freshly-created
	// source doc with nothing yet to append) doesn't get the flag.
	if len(mn.Values) == 0 && len(toRemove) > 0 {
		mn.IsFlowStyle = true
	}
	return nil
}

// Walks source's mapping values, matches each against data's mapping by
// key, and either applies the update, marks the source entry for removal
// (data value is null), or recurses (both are maps).
func updateExistingEntries(
	file *ast.File, sourceNode ast.Node, path string, data *ast.MappingNode,
) (map[string]bool, map[*ast.MappingValueNode]bool, error) {
	seen := make(map[string]bool)
	toRemove := make(map[*ast.MappingValueNode]bool)

	for _, sMV := range mappingValues(sourceNode) {
		key := keyString(sMV.Key)
		if key == "" {
			continue
		}
		dMV := dataValueByKey(data, key)
		if dMV == nil {
			continue
		}
		seen[key] = true
		if isNullNode(dMV.Value) {
			toRemove[sMV] = true
			continue
		}
		if err := applyValue(file, sMV, dMV, joinPath(path, key)); err != nil {
			return nil, nil, err
		}
	}

	return seen, toRemove, nil
}

// Looks up a key in data's mapping. Returns nil when absent.
func dataValueByKey(mn *ast.MappingNode, key string) *ast.MappingValueNode {
	if mn == nil {
		return nil
	}
	for _, mv := range mn.Values {
		if keyString(mv.Key) == key {
			return mv
		}
	}
	return nil
}

// Routes a single data->source update:
//   - both values are mappings → recurse into updateAtNode.
//   - anything else → replace source's value with data's node, adopt data's
//     inline comment when present (else keep source's), and re-run the
//     block-form style rule on the resulting node.
//
// AnchorNode wrappers are temporarily unwrapped because yaml.Path can't
// navigate through them; the wrapper is restored on return and its Value
// field still points to the (now-mutated) inner mapping.
func applyValue(file *ast.File, sourceMv, dataMv *ast.MappingValueNode, childPath string) error {
	sourceVal, restore := unwrapAnchor(sourceMv)
	defer restore()

	if sMap, ok := sourceVal.(*ast.MappingNode); ok {
		if dMap, ok := dataMv.Value.(*ast.MappingNode); ok {
			return updateAtNode(file, sMap, childPath, dMap)
		}
	}

	keyTok := sourceMv.Key.GetToken()
	keyCol := keyTok.Position.Column
	keyIndent := keyTok.Position.IndentLevel
	// Snapshot before the swap: the token that'll still be pointed at by
	// the next sibling's Key.Prev after ReplaceWithNode (chain isn't
	// rewired). If we later wrap with `+` trailing blanks, we bump its
	// line to MaxInt to keep goccy's checkLineBreak from stacking a blank.
	oldValTok := lastChainToken(sourceMv.Value)
	// If data's own node has no inline comment, transfer source's over so
	// it survives the swap. When data has a comment, data wins.
	if dataMv.Value.GetComment() == nil {
		if sc := sourceMv.Value.GetComment(); sc != nil {
			_ = dataMv.Value.SetComment(sc)
		}
	}
	// Snapshot data's additional indent BEFORE ReplaceWithNode mutates it.
	// For a block sequence, data's dash column relative to its own parent
	// key column ("additional indent") is the author's stylistic choice
	// (flush = 0, indented = detectedStyle.indent). We restore that choice
	// post-replace so data-verbatim styling wins over goccy's inflated
	// positions.
	seqAddIndent := dataSequenceAddIndent(dataMv)
	if err := replaceAt(file, childPath, dataMv.Value, keyCol, keyIndent, seqAddIndent); err != nil {
		return fmt.Errorf("replacing %s: %w", childPath, err)
	}

	// NOTE: mixed motivation. The wrap block below serves TWO purposes:
	// (a) monkey-patch for goccy StringNode multi-line render bugs
	//     (only fires when shouldWrapStringValue's `\n` early-return
	//     triggered - see the NOTE on that function); AND
	// (b) style rule: fold long single-line plain values to `>`.
	// See the removal checklist in patch_goccy.go. If you extract the
	// style rule into a separate format-yaml utility, delete this whole
	// block; the tool will honor data's style verbatim.
	if s, ok := sourceMv.Value.(*ast.StringNode); ok {
		rawStr := s.Value
		if shouldWrapStringValue(rawStr, keyCol, keyString(sourceMv.Key)) {
			sourceMv.Value = &keepChompString{
				StringNode: s,
				val:        rawStr,
				keyCol:     keyCol,
				useFolded:  shouldFold(rawStr, keyString(sourceMv.Key), keyCol),
			}
			suppressCheckLineBreakForKeepChomp(oldValTok, rawStr)
		}
	}
	// NOTE: monkey-patch for goccy render bugs. See the removal checklist
	// in patch_goccy.go. If data replaced source's value with a `+`-chomp
	// LiteralNode carrying 2+ trailing newlines, the same checkLineBreak
	// stacking that hits StringNode wraps also hits LiteralNode swaps: the
	// next sibling's Key.Prev still points at source's pre-swap token, so
	// goccy adds a blank on top of the wrapper's trailing content. Bump
	// the pre-swap token line so the diff goes negative.
	if lit, ok := sourceMv.Value.(*ast.LiteralNode); ok && needsKeepChompFix(lit) {
		suppressCheckLineBreakForKeepChomp(oldValTok, lit.Value.Value)
	}
	// After any wrapping decision, drop explicit quotes on data-provided
	// scalars whose content is plain-safe. This runs on the whole subtree
	// so nested map/sequence values benefit too.
	unquoteSafeStrings(sourceMv.Value)
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

// Finds the next entry that will survive the removal pass and reports
// whether goccy will naturally emit a blank line above it - via the same
// checkLineBreak math it uses at render time. Returning true means we
// should skip promoting a FootComment on the preceding entry; goccy
// already covers the section break.
func nextSurvivorHasNaturalBlank(values []*ast.MappingValueNode, from int, toRemove map[*ast.MappingValueNode]bool) bool {
	for j := from; j < len(values); j++ {
		if toRemove[values[j]] {
			continue
		}
		return goccyWouldInsertBlank(firstEntryToken(values[j]))
	}
	return false
}

// Returns the token goccy uses when deciding whether to insert a blank
// line above this entry - the head comment start if there is one, else
// the key token.
func firstEntryToken(mv *ast.MappingValueNode) *token.Token {
	if cg := mv.GetComment(); cg != nil && len(cg.Comments) > 0 {
		return cg.Comments[0].Token
	}
	return mv.Key.GetToken()
}

// Mirrors goccy's checkLineBreak for a single token: raw source-line diff
// minus any newlines carried in the previous token's origin. When the
// result is positive, goccy prepends a "\n" during render.
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

// Reports whether the source had a blank line immediately above this
// entry. A gap of more than one line between the entry's first source
// token and its Prev means an intervening blank line - the section break
// we want to keep across a removal.
func hasLeadingBlankLine(mv *ast.MappingValueNode) bool {
	tok := firstEntryToken(mv)
	if tok == nil || tok.Prev == nil {
		return false
	}
	return tok.Position.Line-tok.Prev.Position.Line > 1
}

// Uses an empty FootComment as a marker that goccy renders as a trailing
// newline. No-op if a FootComment already exists.
func ensureBlankFoot(mv *ast.MappingValueNode) {
	if mv.FootComment != nil {
		return
	}
	mv.FootComment = &ast.CommentGroupNode{}
}

// Splices every data entry not already handled by updateExistingEntries
// into mn. Placement depends on the existing siblings' order: if they
// were alphabetically sorted before this call, each new key lands at its
// own alphabetical position (preserving the author's convention);
// otherwise entries are appended at the end in data-tree order.
//
// Null-valued data entries are ignored here (they were only meaningful
// as "remove" markers, which don't apply to keys not present in source).
// Null leaves nested inside an appended subtree are dropped too - a data
// author who sends `null` never intends the token to appear in output.
//
// Each appended entry is realigned so nested indent follows source's
// detectedStyle.indent, not whatever indent the data file used. This
// matters when data uses 2-space indent and source uses 4-space (or vice
// versa) - MappingNode.Merge's constant AddColumn delta can't rescale
// per level, so we walk the subtree ourselves.
func appendMissingEntries(mn *ast.MappingNode, data *ast.MappingNode, seen map[string]bool) error {
	keyCol := targetKeyColumn(mn)
	keepSorted := siblingsAreSorted(mn.Values)
	for _, dMV := range data.Values {
		k := keyString(dMV.Key)
		if k == "" || seen[k] {
			continue
		}
		if isNullNode(dMV.Value) {
			continue
		}
		stripNullEntries(dMV.Value)
		realignAppended(dMV, keyCol)
		unquoteSafeStrings(dMV)
		if keepSorted {
			insertSorted(mn, dMV, k)
		} else {
			mn.Values = append(mn.Values, dMV)
		}
	}
	return nil
}

// Reports whether the mapping's existing keys sit in ascending order.
// An empty or single-key mapping is trivially sorted.
//
// The YAML merge key `<<:` is pinned to its source position by
// convention and excluded from the sortedness check.
func siblingsAreSorted(values []*ast.MappingValueNode) bool {
	sortable := make([]string, 0, len(values))
	for _, mv := range values {
		if k := keyString(mv.Key); k != mergeKeyName {
			sortable = append(sortable, k)
		}
	}
	for i := 1; i < len(sortable); i++ {
		if sortable[i-1] > sortable[i] {
			return false
		}
	}
	return true
}

// Places dMV at the first index whose existing key sorts after k, keeping
// the mapping alphabetically ordered. Called only when the mapping was
// already sorted, so a single pass preserves the invariant across every
// new entry in a batch.
//
// The merge key `<<:` is skipped over during the position search so it
// stays pinned regardless of where sort order would otherwise land the
// new entry.
func insertSorted(mn *ast.MappingNode, dMV *ast.MappingValueNode, k string) {
	pos := len(mn.Values)
	for i, mv := range mn.Values {
		mk := keyString(mv.Key)
		if mk == mergeKeyName {
			continue
		}
		if mk > k {
			pos = i
			break
		}
	}
	mn.Values = append(mn.Values, nil)
	copy(mn.Values[pos+1:], mn.Values[pos:])
	mn.Values[pos] = dMV
}

// The YAML merge-key indicator (`<<`), semantically distinct from a
// regular key: it references another mapping to be merged in and by
// convention stays at the top of the mapping.
const mergeKeyName = "<<"

// Removes null-valued keys from an appended subtree. Sequence entries
// are left alone: a null in a list means "the list has a null at position
// N", not "skip this entry".
func stripNullEntries(n ast.Node) {
	switch v := n.(type) {
	case *ast.MappingNode:
		filtered := v.Values[:0]
		for _, mv := range v.Values {
			if isNullNode(mv.Value) {
				continue
			}
			stripNullEntries(mv.Value)
			filtered = append(filtered, mv)
		}
		v.Values = filtered
	case *ast.SequenceNode:
		for _, entry := range v.Values {
			stripNullEntries(entry)
		}
	case *ast.AnchorNode:
		stripNullEntries(v.Value)
	}
}

// Returns the column the mapping's existing keys sit at, falling back to
// the mapping's Start.Column when Values is empty.
func targetKeyColumn(mn *ast.MappingNode) int {
	if len(mn.Values) > 0 {
		if t := mn.Values[0].Key.GetToken(); t != nil && t.Position != nil {
			return t.Position.Column
		}
	}
	if mn.Start != nil && mn.Start.Position != nil {
		return mn.Start.Position.Column
	}
	return 1
}

// Walks a data-provided MappingValueNode being appended and rewrites Key
// columns at each depth so nested indents follow source's
// detectedStyle.indent. Block-scalar content columns are also patched so
// their leading indent matches source's expectations.
//
// The top-level key's token also has its Prev pointer cleared: without
// this, goccy's checkLineBreak sees the line gap the key had in the *data*
// file (e.g. a blank line above `new_field` in data) and emits a leading
// blank in source's output that was never asked for. Nested keys keep
// their Prev chains because gaps within data's subtree are legitimate
// formatting the author chose.
func realignAppended(mv *ast.MappingValueNode, keyCol int) {
	setPositionColumn(mv.Key, keyCol)
	// NOTE: monkey-patch for goccy render bugs. See the removal checklist
	// in patch_goccy.go.
	if kt := mv.Key.GetToken(); kt != nil {
		kt.Prev = nil
	}
	childCol := keyCol + detectedStyle.indent
	realignValueAt(mv.Value, keyCol, childCol)
}

// Rewrites positions for a value node given (a) the parent key's column
// (used for block-scalar content that indents relative to key) and (b)
// the child column (used for nested mapping/sequence keys).
func realignValueAt(n ast.Node, keyCol, childCol int) {
	switch v := n.(type) {
	case *ast.MappingNode:
		for _, child := range v.Values {
			realignAppended(child, childCol)
		}
	case *ast.SequenceNode:
		// Block-style sequence with IndentSequence=true renders entries at
		// keyCol + indent (deeper than key); flow-style renders inline and
		// column doesn't matter. Either way, recurse into entries with
		// childCol as the anchor.
		for _, entry := range v.Values {
			realignValueAt(entry, childCol, childCol+detectedStyle.indent)
		}
	case *ast.LiteralNode:
		if v.Value != nil {
			setPositionColumn(v.Value, childCol)
		}
	case *ast.StringNode:
		if strings.Contains(v.Value, "\n") {
			setPositionColumn(v, childCol)
		}
	case *ast.AnchorNode:
		realignValueAt(v.Value, keyCol, childCol)
	}
}

func setPositionColumn(n ast.Node, col int) {
	t := n.GetToken()
	if t == nil || t.Position == nil {
		return
	}
	t.Position.Column = col
}

// Writes both Column and IndentLevel on a token in one nil-safe step.
// Used by realignToKey where the swap places a scalar value at the parent
// key's anchor.
func setTokenColAndIndent(t *token.Token, col, indent int) {
	if t == nil || t.Position == nil {
		return
	}
	t.Position.Column = col
	t.Position.IndentLevel = indent
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

// Reports whether n is the explicit YAML null token (either `null` / `~`
// or an empty value that parsed to *ast.NullNode). Data semantics: a null
// in the data tree means "delete this key from source".
func isNullNode(n ast.Node) bool {
	_, ok := n.(*ast.NullNode)
	return ok
}

// Swaps the value at `path` for `dataNode` directly. Realignment is
// required because goccy's ReplaceWithNode drops the parent key's indent
// context for multi-line values.
//
// keyCol/keyIndent are the source column and indent level of the parent
// mapping key. Pass -1 for either when the caller doesn't have mapping
// context; alignment patching is skipped in that case. seqAddIndent is
// data's own dash-vs-key offset for a block sequence value; -1 means
// "not a block sequence, ignore" and the mapping/scalar realignment path
// picks up detectedStyle.indent instead.
func replaceAt(file *ast.File, path string, dataNode ast.Node, keyCol, keyIndent, seqAddIndent int) error {
	p, err := yaml.PathString(path)
	if err != nil {
		return fmt.Errorf("invalid path %s: %w", path, err)
	}
	traceKey(keyCol, keyIndent)
	if n := nodeAt(p, file); n != nil {
		traceNode("OLD VALUE", n)
	}
	traceNode("NEW NODE", dataNode)

	if err := p.ReplaceWithNode(file, dataNode); err != nil {
		return err
	}
	if n := nodeAt(p, file); n != nil {
		// NOTE: monkey-patch for goccy render bugs. See the removal
		// checklist in patch_goccy.go.
		if keyCol >= 0 && keyIndent >= 0 {
			realignToKey(n, keyCol, keyIndent, seqAddIndent)
		}
		traceNode("NEW VALUE", n)
	}
	return nil
}

// Returns data's dash column minus its parent key column for a block
// sequence value - the "additional indent" the data author chose (0 =
// flush, N = indented by N). Returns -1 for anything that isn't a block
// sequence, telling replaceAt to fall back to detectedStyle.indent.
func dataSequenceAddIndent(mv *ast.MappingValueNode) int {
	seq, ok := mv.Value.(*ast.SequenceNode)
	if !ok || seq.IsFlowStyle {
		return -1
	}
	if seq.Start == nil || seq.Start.Position == nil {
		return -1
	}
	kt := mv.Key.GetToken()
	if kt == nil || kt.Position == nil {
		return -1
	}
	return seq.Start.Position.Column - kt.Position.Column
}

// Pushes the new value token's column/indent back onto the parent key's
// alignment. `>` / `|` block scalars land inside a LiteralNode whose
// Value holds the content token; plain/quoted values land directly in a
// StringNode. Mapping/Sequence values are walked so their children land
// relative to the parent key - goccy's ReplaceWithNode inflates nested
// columns by the old scalar's position instead of anchoring to the key.
//
// For block sequences, seqAddIndent carries data's own dash-vs-key offset
// so `- x` at col 1 stays flush and `- x` at col 3 stays indented under a
// col-1 parent key. Pass -1 to fall back to detectedStyle.indent.
func realignToKey(n ast.Node, keyCol, keyIndent, seqAddIndent int) {
	switch v := n.(type) {
	case *ast.LiteralNode:
		if v.Value != nil {
			setTokenColAndIndent(v.Value.Token, keyCol, keyIndent)
		}
	case *ast.StringNode:
		setTokenColAndIndent(v.Token, keyCol, keyIndent)
	case *ast.MappingNode:
		childCol := keyCol + detectedStyle.indent
		for _, child := range v.Values {
			realignAppended(child, childCol)
		}
	case *ast.SequenceNode:
		addIndent := seqAddIndent
		if addIndent < 0 {
			addIndent = detectedStyle.indent
		}
		entryCol := keyCol + addIndent
		if v.Start != nil && v.Start.Position != nil {
			v.Start.Position.Column = entryCol
		}
		for _, entry := range v.Values {
			realignValueAt(entry, entryCol, entryCol+detectedStyle.indent)
		}
	}
}

func nodeAt(p *yaml.Path, file *ast.File) ast.Node {
	n, err := p.FilterFile(file)
	if err != nil {
		return nil
	}
	return n
}

// Extracts the string form of a mapping key. StringNode is the common
// case; anything else falls back to the node's .String() form.
func keyString(node ast.Node) string {
	if s, ok := node.(*ast.StringNode); ok {
		return s.Value
	}
	return node.String()
}

// Uses goccy's reserved-char escape for non-identifier keys:
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
