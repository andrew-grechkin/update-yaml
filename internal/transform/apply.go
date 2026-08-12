// Transform: apply data (as AST) onto source (also AST). This is the core
// update loop: walk data's mapping tree, and for every leaf key present in
// data either replace the source value, remove it (when data value is
// null), or recurse (when both source and data have MappingNode values).
// New keys from data are placed per the sort-vs-append rule in
// appendMissingEntries.
package transform

import (
	"fmt"
	"strings"

	yaml "github.com/goccy/go-yaml"
	yamlast "github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	"github.com/goccy/go-yaml/token"

	"github.com/andrew-grechkin/update-yaml/internal/ingest"
	"github.com/andrew-grechkin/update-yaml/pkg/ast"
	"github.com/andrew-grechkin/update-yaml/pkg/inspect"
	"github.com/andrew-grechkin/update-yaml/pkg/patch"
	"github.com/andrew-grechkin/update-yaml/pkg/style"
)

// Walks each source doc in parallel with the merged data doc for that
// slot. Slots with no data are skipped, so unmodified docs pass through
// as-is.
func ApplyMergedDocs(file *yamlast.File, mergedDocs []*yamlast.DocumentNode) error {
	if len(file.Docs) == 0 && len(mergedDocs) > 0 && !ingest.IsEmptyDataDoc(mergedDocs[0]) {
		file.Docs = append(file.Docs, newEmptyDoc())
	}
	for i, doc := range file.Docs {
		if i >= len(mergedDocs) || ingest.IsEmptyDataDoc(mergedDocs[i]) {
			continue
		}
		ensureMappingBody(doc)
		scoped := &yamlast.File{Docs: []*yamlast.DocumentNode{doc}}
		dataMap, ok := mergedDocs[i].Body.(*yamlast.MappingNode)
		if !ok {
			continue
		}
		if err := updateAtNode(scoped, doc.Body, "$", dataMap); err != nil {
			return err
		}
		// NOTE: monkey-patch for goccy render bugs. See the removal
		// checklist in pkg/patch/goccy.go.
		patch.LiteralBugs(doc.Body)
	}
	return nil
}

// Swaps a nil or null doc body for a fresh empty block mapping so
// subsequent updates have somewhere to land.
func ensureMappingBody(doc *yamlast.DocumentNode) {
	if doc.Body == nil || ast.IsNullNode(doc.Body) {
		doc.Body = newEmptyBlockMapping()
	}
}

// Empty-doc/empty-mapping constructors that go through the parser rather
// than building AST nodes directly, so Start/End tokens and BaseNode are
// populated. An empty mapping renders as `{}` regardless of IsFlowStyle,
// but once entries are added the flag determines block vs flow output.
func newEmptyDoc() *yamlast.DocumentNode {
	f, _ := parser.ParseBytes([]byte("{}\n"), parser.ParseComments)
	d := f.Docs[0]
	d.Body.(*yamlast.MappingNode).IsFlowStyle = false
	return d
}

func newEmptyBlockMapping() *yamlast.MappingNode {
	return newEmptyDoc().Body.(*yamlast.MappingNode)
}

// Recurses on mapping-vs-mapping rather than replacing wholesale so
// nested comments and unmentioned keys are preserved. Explicit
// *ast.NullNode value in data removes the corresponding key. New keys are
// placed per the sort-vs-append rule in appendMissingEntries.
func updateAtNode(file *yamlast.File, sourceNode yamlast.Node, path string, data *yamlast.MappingNode) error {
	seen, toRemove, err := updateExistingEntries(file, sourceNode, path, data)
	if err != nil {
		return err
	}

	mn, ok := sourceNode.(*yamlast.MappingNode)
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
	file *yamlast.File, sourceNode yamlast.Node, path string, data *yamlast.MappingNode,
) (map[string]bool, map[*yamlast.MappingValueNode]bool, error) {
	seen := make(map[string]bool)
	toRemove := make(map[*yamlast.MappingValueNode]bool)

	for _, sMV := range ast.MappingValues(sourceNode) {
		key := ast.KeyString(sMV.Key)
		if key == "" {
			continue
		}
		dMV := dataValueByKey(data, key)
		if dMV == nil {
			continue
		}
		seen[key] = true
		if ast.IsNullNode(dMV.Value) {
			toRemove[sMV] = true
			continue
		}
		if err := applyValue(file, sMV, dMV, ast.JoinPath(path, key)); err != nil {
			return nil, nil, err
		}
	}

	return seen, toRemove, nil
}

// Looks up a key in data's mapping. Returns nil when absent.
func dataValueByKey(mn *yamlast.MappingNode, key string) *yamlast.MappingValueNode {
	if mn == nil {
		return nil
	}
	for _, mv := range mn.Values {
		if ast.KeyString(mv.Key) == key {
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
func applyValue(file *yamlast.File, sourceMv, dataMv *yamlast.MappingValueNode, childPath string) error {
	sourceVal, restore := swapUnwrapAnchor(sourceMv)
	defer restore()

	if sMap, ok := sourceVal.(*yamlast.MappingNode); ok {
		if dMap, ok := dataMv.Value.(*yamlast.MappingNode); ok {
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
	oldValTok := patch.LastChainToken(sourceMv.Value)
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
	// (flush = 0, indented = style.Active.Indent). We restore that choice
	// post-replace so data-verbatim styling wins over goccy's inflated
	// positions.
	seqAddIndent := dataSequenceAddIndent(dataMv)
	if err := replaceAt(file, childPath, dataMv.Value, keyCol, keyIndent, seqAddIndent); err != nil {
		return fmt.Errorf("replacing %s: %w", childPath, err)
	}

	processDataValue(sourceMv, keyCol, oldValTok)
	return nil
}

// Destructive anchor unwrap for use around yaml.Path traversal.
// yaml.PathString / ReplaceWithNode can't navigate through AnchorNode,
// so we temporarily swap mv.Value with the anchor's inner value and
// return a restore func the caller defers. This is an update-yaml
// internal workaround; general callers (including the sibling
// format-yaml module) use the non-destructive ast.UnwrapAnchor instead.
func swapUnwrapAnchor(mv *yamlast.MappingValueNode) (yamlast.Node, func()) {
	anchor, ok := mv.Value.(*yamlast.AnchorNode)
	if !ok {
		return mv.Value, func() {}
	}
	mv.Value = anchor.Value
	return anchor.Value, func() { mv.Value = anchor }
}

// Post-processes a data-provided MappingValueNode after it has been placed
// under source (either by a ReplaceWithNode swap in applyValue or by
// realign-then-append in appendMissingEntries). Both paths share the same
// three transformations so update-vs-insert never produce cosmetically
// different output:
//
//   - Null-valued keys anywhere in the subtree are removed. A `null` in
//     data is a delete signal, never a value to render.
//   - Explicitly-quoted scalars whose content parses back plain get their
//     quotes dropped (JSON-style `"port": 9090` → `port: 9090`).
//   - Multi-line or long single-line StringNode values are wrapped in a
//     KeepChompString to render as `|` / `>` block scalars.
//
// oldValTok is the pre-swap token from source's chain that the next
// sibling's Key.Prev still references. When non-nil, its Position.Line is
// bumped to suppress goccy's checkLineBreak from stacking a blank on top
// of a wrapper's trailing content. Pass nil for freshly-appended entries
// that have no pre-swap chain to suppress.
func processDataValue(mv *yamlast.MappingValueNode, keyCol int, oldValTok *token.Token) {
	stripNullEntries(mv.Value)
	style.UnquoteSafeStrings(mv)
	// NOTE: mixed motivation on the wrap. See pkg/patch removal checklist.
	// (a) monkey-patch for goccy StringNode multi-line render bugs;
	// (b) style rule: fold long single-line values to `>`.
	wrapStringValueForStyle(mv, keyCol, oldValTok)
	// NOTE: monkey-patch for goccy render bugs. A swapped-in `|+`/`>+`
	// LiteralNode with 2+ trailing newlines needs the pre-swap chain
	// token bumped so goccy's checkLineBreak on the next sibling
	// doesn't stack a blank on top of the wrapper's trailing content.
	// No-op when oldValTok is nil (append path - no swap to suppress).
	if lit, ok := mv.Value.(*yamlast.LiteralNode); ok && patch.NeedsChompFix(lit) {
		patch.SuppressCheckLineBreak(oldValTok, lit.Value.Value)
	}
}

// Applies the block-form style rule to a data-provided StringNode value
// under mv. If the value carries newlines or is a single-line long enough
// to overflow the plain-scalar budget, mv.Value is swapped for a
// KeepChompString wrapper that renders in `|`/`>` block form. Pass a
// non-nil oldValTok when the wrap follows a ReplaceWithNode swap so
// goccy's checkLineBreak on the next sibling doesn't stack a blank on
// top of the wrapper's trailing content; nil for freshly-appended values
// that have no pre-swap chain to suppress.
func wrapStringValueForStyle(mv *yamlast.MappingValueNode, keyCol int, oldValTok *token.Token) {
	s, ok := mv.Value.(*yamlast.StringNode)
	if !ok {
		return
	}
	rawStr := s.Value
	keyName := ast.KeyString(mv.Key)
	if !style.ShouldWrapStringValue(rawStr, keyCol, keyName) {
		return
	}
	mv.Value = &style.KeepChompString{
		StringNode: s,
		Val:        rawStr,
		KeyCol:     keyCol,
		UseFolded:  style.ShouldFold(rawStr, keyName, keyCol),
	}
	if oldValTok != nil {
		patch.SuppressCheckLineBreak(oldValTok, rawStr)
	}
}

func removeMarkedEntries(mn *yamlast.MappingNode, toRemove map[*yamlast.MappingValueNode]bool) {
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
			if ast.HasLeadingBlankLine(mv) && len(filtered) > 0 &&
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
func nextSurvivorHasNaturalBlank(values []*yamlast.MappingValueNode, from int, toRemove map[*yamlast.MappingValueNode]bool) bool {
	for j := from; j < len(values); j++ {
		if toRemove[values[j]] {
			continue
		}
		return ast.GoccyWouldInsertBlank(ast.FirstEntryToken(values[j]))
	}
	return false
}

// Uses an empty FootComment as a marker that goccy renders as a trailing
// newline. No-op if a FootComment already exists.
func ensureBlankFoot(mv *yamlast.MappingValueNode) {
	if mv.FootComment != nil {
		return
	}
	mv.FootComment = &yamlast.CommentGroupNode{}
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
// style.Active.Indent, not whatever indent the data file used. This
// matters when data uses 2-space indent and source uses 4-space (or vice
// versa) - MappingNode.Merge's constant AddColumn delta can't rescale
// per level, so we walk the subtree ourselves.
func appendMissingEntries(mn *yamlast.MappingNode, data *yamlast.MappingNode, seen map[string]bool) error {
	keyCol := targetKeyColumn(mn)
	keepSorted := siblingsAreSorted(mn.Values)
	for _, dMV := range data.Values {
		k := ast.KeyString(dMV.Key)
		if k == "" || seen[k] {
			continue
		}
		if ast.IsNullNode(dMV.Value) {
			continue
		}
		realignAppended(dMV, keyCol)
		// Same post-placement pipeline as applyValue: strip nulls,
		// unquote-where-safe, apply the block-form style rule. Pass
		// nil oldValTok - no ReplaceWithNode happened here, so there
		// is no pre-swap chain to suppress.
		processDataValue(dMV, keyCol, nil)
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
func siblingsAreSorted(values []*yamlast.MappingValueNode) bool {
	sortable := make([]string, 0, len(values))
	for _, mv := range values {
		if k := ast.KeyString(mv.Key); k != mergeKeyName {
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
func insertSorted(mn *yamlast.MappingNode, dMV *yamlast.MappingValueNode, k string) {
	pos := len(mn.Values)
	for i, mv := range mn.Values {
		mk := ast.KeyString(mv.Key)
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
func stripNullEntries(n yamlast.Node) {
	ast.Walk(n, func(node yamlast.Node) bool {
		mn, ok := node.(*yamlast.MappingNode)
		if !ok {
			return true
		}
		filtered := mn.Values[:0]
		for _, mv := range mn.Values {
			if !ast.IsNullNode(mv.Value) {
				filtered = append(filtered, mv)
			}
		}
		mn.Values = filtered
		return true
	})
}

// Returns the column the mapping's existing keys sit at, falling back to
// the mapping's Start.Column when Values is empty.
func targetKeyColumn(mn *yamlast.MappingNode) int {
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
// style.Active.Indent. Block-scalar content columns are also patched so
// their leading indent matches source's expectations.
//
// The top-level key's token also has its Prev pointer cleared: without
// this, goccy's checkLineBreak sees the line gap the key had in the *data*
// file (e.g. a blank line above `new_field` in data) and emits a leading
// blank in source's output that was never asked for. Nested keys keep
// their Prev chains because gaps within data's subtree are legitimate
// formatting the author chose.
func realignAppended(mv *yamlast.MappingValueNode, keyCol int) {
	// Snapshot data's parent-vs-dash offset BEFORE clobbering the key
	// column, so a block-sequence value keeps the author's compact
	// (`- x` under parent) or indented (`  - x`) style.
	seqAddIndent := dataSequenceAddIndent(mv)
	ast.SetPositionColumn(mv.Key, keyCol)
	// NOTE: monkey-patch for goccy render bugs. See the removal checklist
	// in pkg/patch/goccy.go.
	if kt := mv.Key.GetToken(); kt != nil {
		kt.Prev = nil
	}
	childCol := keyCol + style.Active.Indent
	realignValueAt(mv.Value, keyCol, childCol, seqAddIndent)
}

// Rewrites positions for a value node given (a) the parent key's column
// (used for block-scalar content that indents relative to key), (b) the
// child column (used for nested mapping keys), and (c) seqAddIndent: the
// parent-vs-dash offset for a block-sequence value, or -1 for "not a
// block sequence, fall back to style.Active.Indent".
func realignValueAt(n yamlast.Node, keyCol, childCol, seqAddIndent int) {
	switch v := n.(type) {
	case *yamlast.MappingNode:
		for _, child := range v.Values {
			realignAppended(child, childCol)
		}
	case *yamlast.SequenceNode:
		realignSequence(v, keyCol, childCol, seqAddIndent)
	case *yamlast.LiteralNode:
		if v.Value != nil {
			ast.SetPositionColumn(v.Value, childCol)
		}
	case *yamlast.StringNode:
		if strings.Contains(v.Value, "\n") {
			ast.SetPositionColumn(v, childCol)
		}
	case *yamlast.AnchorNode:
		realignValueAt(v.Value, keyCol, childCol, seqAddIndent)
	}
}

// Rewrites a block or flow sequence and its entries. Block sequences
// honor data's parent-vs-dash offset (seqAddIndent) so compact `- x`
// under a key and indented `  - x` both survive; entries sit two cols
// after the dash (`- ` prefix), independent of style.Active.Indent.
func realignSequence(v *yamlast.SequenceNode, keyCol, childCol, seqAddIndent int) {
	if v.IsFlowStyle {
		for _, entry := range v.Values {
			realignValueAt(entry, childCol, childCol+style.Active.Indent, -1)
		}
		return
	}
	addIndent := seqAddIndent
	if addIndent < 0 {
		addIndent = style.Active.Indent
	}
	dashCol := keyCol + addIndent
	if v.Start != nil && v.Start.Position != nil {
		v.Start.Position.Column = dashCol
	}
	entryKeyCol := dashCol + 2
	for _, entry := range v.Values {
		if e, ok := entry.(*yamlast.MappingNode); ok {
			for _, child := range e.Values {
				realignAppended(child, entryKeyCol)
			}
			continue
		}
		realignValueAt(entry, entryKeyCol, entryKeyCol+style.Active.Indent, -1)
	}
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
// picks up style.Active.Indent instead.
func replaceAt(file *yamlast.File, path string, dataNode yamlast.Node, keyCol, keyIndent, seqAddIndent int) error {
	p, err := yaml.PathString(path)
	if err != nil {
		return fmt.Errorf("invalid path %s: %w", path, err)
	}
	inspect.TraceKey(keyCol, keyIndent)
	if n := ast.NodeAt(p, file); n != nil {
		inspect.TraceNode("OLD VALUE", n)
	}
	inspect.TraceNode("NEW NODE", dataNode)

	if err := p.ReplaceWithNode(file, dataNode); err != nil {
		return err
	}
	if n := ast.NodeAt(p, file); n != nil {
		// NOTE: monkey-patch for goccy render bugs. See the removal
		// checklist in pkg/patch/goccy.go.
		if keyCol >= 0 && keyIndent >= 0 {
			realignToKey(n, keyCol, keyIndent, seqAddIndent)
		}
		inspect.TraceNode("NEW VALUE", n)
	}
	return nil
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
// col-1 parent key. Pass -1 to fall back to style.Active.Indent.
func realignToKey(n yamlast.Node, keyCol, keyIndent, seqAddIndent int) {
	switch v := n.(type) {
	case *yamlast.LiteralNode:
		if v.Value != nil {
			ast.SetTokenColAndIndent(v.Value.Token, keyCol, keyIndent)
		}
	case *yamlast.StringNode:
		ast.SetTokenColAndIndent(v.Token, keyCol, keyIndent)
	case *yamlast.MappingNode:
		childCol := keyCol + style.Active.Indent
		for _, child := range v.Values {
			realignAppended(child, childCol)
		}
	case *yamlast.SequenceNode:
		// Delegate to realignValueAt so seq-entry column math stays in
		// one place; seqAddIndent carries the -1 fallback semantics.
		realignValueAt(v, keyCol, keyCol+style.Active.Indent, seqAddIndent)
	}
}

// Returns data's dash column minus its parent key column for a block
// sequence value - the "additional indent" the data author chose (0 =
// flush, N = indented by N). Returns -1 for anything that isn't a block
// sequence, telling replaceAt to fall back to style.Active.Indent.
func dataSequenceAddIndent(mv *yamlast.MappingValueNode) int {
	seq, ok := mv.Value.(*yamlast.SequenceNode)
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
