// Transform: apply data (as AST) onto source (also AST). This is the core
// update loop: walk data's mapping tree, and for every leaf key present in
// data either replace the source value, remove it (when data value is
// null), or recurse (when both source and data have MappingNode values).
// New keys from data are placed per the sort-vs-append rule in
// appendMissingEntries.
package transform

import (
	"fmt"
	"strconv"
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
		// scoped file wraps just this doc so replaceAt's yaml.Path lookups
		// resolve inside a single-doc tree.
		a := &docApplier{file: &yamlast.File{Docs: []*yamlast.DocumentNode{doc}}}
		// Doc-root has no parent key; a type-mismatched top level (data
		// not a mapping) has no meaningful leaf-replace, so we just skip.
		if err := a.apply(doc.Body, mergedDocs[i].Body, "$", noReplace); err != nil {
			return err
		}
		// NOTE: monkey-patch for goccy render bugs. See the removal
		// checklist in pkg/patch/goccy.go.
		patch.LiteralBugs(doc.Body)
	}
	return nil
}

func noReplace() error { return nil }

// docApplier scopes one document's update. The file field is only used
// by replaceAt's yaml.Path lookups; the tree traversal (apply, applyMap,
// applySeq) works purely over nodes.
type docApplier struct {
	file *yamlast.File
}

// The recursive tree traversal. Same-type containers recurse so source's
// structural formatting is preserved. Same-type leaves that are already
// equal short-circuit. Anything else - leaf value differs, or types
// disagree - invokes replaceLeaf; the caller supplies it with the
// positional context (map-key column, sequence dash column) that
// replaceAt needs to re-anchor data's node under source.
func (a *docApplier) apply(sNode, dNode yamlast.Node, path string, replaceLeaf func() error) error {
	if sMap, ok := sNode.(*yamlast.MappingNode); ok {
		if dMap, ok := dNode.(*yamlast.MappingNode); ok {
			return a.applyMap(sMap, dMap, path)
		}
	}
	if sSeq, ok := sNode.(*yamlast.SequenceNode); ok {
		if dSeq, ok := dNode.(*yamlast.SequenceNode); ok {
			return a.applySeq(sSeq, dSeq, path)
		}
	}
	return replaceLeaf()
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

// Returns an empty AST counterpart of n's container type. Used by the
// seq-append path so the traversal can pair a fresh empty container
// with data's subtree and process it uniformly (null-key removal,
// unquote-safe). Leaves fall through to the caller's leaf-replace.
func emptyLike(n yamlast.Node) yamlast.Node {
	switch n.(type) {
	case *yamlast.MappingNode:
		return newEmptyBlockMapping()
	case *yamlast.SequenceNode:
		f, _ := parser.ParseBytes([]byte("[]\n"), parser.ParseComments)
		seq := f.Docs[0].Body.(*yamlast.SequenceNode)
		seq.IsFlowStyle = false
		return seq
	}
	return n
}

// applyMap walks data's keys, updating source in parallel. Two passes:
// updateOrMarkForRemoval handles keys already in source, then
// appendNewKeys walks new keys (sort-vs-append recomputed against the
// post-removal source so an unsorted source that becomes trivially
// sorted after a delete still gets sort-insert). Source keys not
// mentioned by data pass through untouched.
func (a *docApplier) applyMap(sMap, dMap *yamlast.MappingNode, path string) error {
	// Goccy's MappingNode.Start points at the ':' of the first child; if
	// we empty the map via null-removal the start position drifts.
	var firstKeyCol int
	if len(sMap.Values) > 0 {
		firstKeyCol = sMap.Values[0].Key.GetToken().Position.Column
	}
	toRemove, err := a.updateOrMarkForRemoval(sMap, dMap, path)
	if err != nil {
		return err
	}
	removeMarkedEntries(sMap, toRemove)
	if len(sMap.Values) == 0 && firstKeyCol > 0 && sMap.Start != nil {
		sMap.Start.Position.Column = firstKeyCol
	}
	appendNewKeys(sMap, dMap)
	// A block mapping fully emptied by data-driven processing renders as
	// `parent:\n{}` under goccy; force flow so it lands as `parent: {}`
	// on one line. Guarded on len(dMap.Values) > 0 so a source-only
	// empty map (data mentioned nothing) preserves its original style.
	if len(sMap.Values) == 0 && len(dMap.Values) > 0 {
		sMap.IsFlowStyle = true
	}
	return nil
}

func (a *docApplier) updateOrMarkForRemoval(sMap, dMap *yamlast.MappingNode, path string) (map[*yamlast.MappingValueNode]bool, error) {
	toRemove := map[*yamlast.MappingValueNode]bool{}
	for _, dMV := range dMap.Values {
		key := ast.KeyString(dMV.Key)
		if key == "" {
			continue
		}
		sMV := findMapValue(sMap, key)
		if sMV == nil {
			continue
		}
		if ast.IsNullNode(dMV.Value) {
			toRemove[sMV] = true
			continue
		}
		if err := a.applyMapValue(sMV, dMV, ast.JoinPath(path, key)); err != nil {
			return nil, err
		}
	}
	return toRemove, nil
}

func appendNewKeys(sMap, dMap *yamlast.MappingNode) {
	keyCol := targetKeyColumn(sMap)
	keepSorted := siblingsAreSorted(sMap.Values)
	for _, dMV := range dMap.Values {
		key := ast.KeyString(dMV.Key)
		if key == "" || ast.IsNullNode(dMV.Value) {
			continue
		}
		if findMapValue(sMap, key) != nil {
			continue
		}
		appendOne(sMap, dMV, keyCol, keepSorted)
	}
}

// applyMapValue recurses into a matched source/data key pair, calling
// apply() with the map-key leaf-replace closure. AnchorNode wrappers are
// temporarily unwrapped because yaml.Path can't navigate through them;
// the wrapper is restored on return and its Value field still points to
// the (now-mutated) inner value.
func (a *docApplier) applyMapValue(sMv, dMv *yamlast.MappingValueNode, childPath string) error {
	sourceVal, restore := swapUnwrapAnchor(sMv)
	defer restore()
	return a.apply(sourceVal, dMv.Value, childPath, func() error {
		return a.replaceMapLeaf(sMv, dMv, childPath)
	})
}

// replaceMapLeaf swaps source's value under sMv for data's node and runs
// the tool's post-swap pipeline: null-strip, unquote-safe, block-form
// rule, plus the goccy checkLineBreak workaround. Called only when
// apply() has determined the pair can't be recursed and isn't a no-op.
func (a *docApplier) replaceMapLeaf(sMv, dMv *yamlast.MappingValueNode, path string) error {
	keyTok := sMv.Key.GetToken()
	keyCol := keyTok.Position.Column
	keyIndent := keyTok.Position.IndentLevel
	// Snapshot before the swap: the token that'll still be pointed at by
	// the next sibling's Key.Prev after ReplaceWithNode (chain isn't
	// rewired). If we later wrap with `+` trailing blanks, we bump its
	// line to MaxInt to keep goccy's checkLineBreak from stacking a blank.
	oldValTok := patch.LastChainToken(sMv.Value)
	// If data's own node has no inline comment, transfer source's over so
	// it survives the swap. When data has a comment, data wins.
	if dMv.Value.GetComment() == nil {
		if sc := sMv.Value.GetComment(); sc != nil {
			_ = dMv.Value.SetComment(sc)
		}
	}
	// Snapshot data's additional indent BEFORE ReplaceWithNode mutates it.
	seqAddIndent := dataSequenceAddIndent(dMv)
	if err := replaceAt(a.file, path, dMv.Value, keyCol, keyIndent, seqAddIndent); err != nil {
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	processDataValue(sMv, keyCol, oldValTok)
	return nil
}

// applySeq walks data's sequence, updating source in parallel. Data
// drives the shape: entries past source's length are appended; source
// tail past data's length is trimmed. Each paired index recurses via
// apply(); type-mismatched or differing leaves swap for data's node
// with the sequence's dash column as anchor.
func (a *docApplier) applySeq(sSeq, dSeq *yamlast.SequenceNode, path string) error {
	dashCol := 0
	if sSeq.Start != nil && sSeq.Start.Position != nil {
		dashCol = sSeq.Start.Position.Column
	}
	for i, d := range dSeq.Values {
		entryPath := path + "[" + strconv.Itoa(i) + "]"
		if i >= len(sSeq.Values) {
			// Extend source with an empty counterpart of the same type
			// so the recurse below walks data's subtree through the
			// normal path (null-key removal, unquote-safe, etc.). Leaves
			// don't need an empty counterpart - they short-circuit to
			// leaf-replace which handles them.
			sSeq.Values = append(sSeq.Values, emptyLike(d))
		}
		err := a.apply(sSeq.Values[i], d, entryPath, func() error {
			// Same canonicalization as replaceMapLeaf minus the map-key
			// pipeline (wrap-for-block-form needs a MappingValueNode);
			// unquote-safe fires so 'plain-safe' values under a
			// sequence lose their quotes.
			stripNullEntries(d)
			style.UnquoteSafeStrings(d)
			return replaceAt(a.file, entryPath, d, dashCol+2, 0, -1)
		})
		if err != nil {
			return err
		}
	}
	if len(sSeq.Values) > len(dSeq.Values) {
		sSeq.Values = sSeq.Values[:len(dSeq.Values)]
	}
	return nil
}

// Looks up a key in a mapping. Returns nil when absent.
func findMapValue(mn *yamlast.MappingNode, key string) *yamlast.MappingValueNode {
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

// Realigns dMV, runs the post-placement pipeline, and inserts it into
// sMap either at its alphabetical position (if source was already sorted)
// or appended at the end.
func appendOne(sMap *yamlast.MappingNode, dMV *yamlast.MappingValueNode, keyCol int, keepSorted bool) {
	realignAppended(dMV, keyCol)
	// Same post-placement pipeline as replaceMapLeaf: strip nulls,
	// unquote-where-safe, apply the block-form style rule. Pass nil
	// oldValTok - no ReplaceWithNode happened here, so there is no
	// pre-swap chain to suppress.
	processDataValue(dMV, keyCol, nil)
	if keepSorted {
		insertSorted(sMap, dMV, ast.KeyString(dMV.Key))
	} else {
		sMap.Values = append(sMap.Values, dMV)
	}
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
	// Doc-level head/foot comments live on the first/last mapping entry (goccy has no separate DocumentNode slot for
	// them). If that entry is being removed, its doc-level portion would go with it. Snapshot before the loop so we
	// can re-attach onto whichever entry becomes the new first/last survivor.
	head, foot := extractDocLevelForRemoval(mn.Values, toRemove)
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
	reattachDocLevelAfterRemoval(mn.Values, head, foot)
}

// extractDocLevelForRemoval snapshots the doc-level head/foot comment groups that would be lost if the current first/
// last entry is among the removed. Returns them detached from the source slots so the removal loop doesn't re-add
// them via `filtered = append`. Head extraction splits the first entry's Comment by blank-line gap: the portion
// separated from the key by at least one blank line is doc-level (returned); the entry-adjacent portion stays and
// goes with the removed entry. Foot extraction is simpler: any FootComment on the last entry IS doc-level (mapping
// bodies don't otherwise use FootComment on the last position).
func extractDocLevelForRemoval(values []*yamlast.MappingValueNode, toRemove map[*yamlast.MappingValueNode]bool) (head []*yamlast.CommentNode, foot *yamlast.CommentGroupNode) {
	if len(values) == 0 {
		return nil, nil
	}
	if first := values[0]; toRemove[first] && first.Comment != nil {
		head = takeDocLevelHead(first)
	}
	if last := values[len(values)-1]; toRemove[last] && last.FootComment != nil {
		foot = last.FootComment
		last.FootComment = nil
	}
	return head, foot
}

// takeDocLevelHead splits mv.Comment.Comments by source-line gap: everything before the first blank-line break is
// doc-level and returned (detached from mv), the rest stays on mv.Comment as its key-adjacent head. Mirrors the
// same rule format-yaml's render.ExtractDocLevelHead uses; kept here inline instead of in a shared package because
// the returned slice type (`[]*ast.CommentNode`) differs from the render side's `[]string` for re-attachment.
func takeDocLevelHead(mv *yamlast.MappingValueNode) []*yamlast.CommentNode {
	comments := mv.Comment.Comments
	if len(comments) == 0 {
		return nil
	}
	keyLine := mv.Key.GetToken().Position.Line
	splitAt := 0
	for i := len(comments) - 1; i >= 0; i-- {
		nextLine := keyLine
		if i < len(comments)-1 {
			nextLine = comments[i+1].Token.Position.Line
		}
		if nextLine-comments[i].Token.Position.Line != 1 {
			splitAt = i + 1
			break
		}
	}
	if splitAt == 0 {
		return nil
	}
	docLevel := comments[:splitAt]
	mv.Comment.Comments = comments[splitAt:]
	return docLevel
}

// reattachDocLevelAfterRemoval places the saved doc-level head/foot comment groups back onto the new first/last
// surviving entries. When the mapping ends up empty the comments have nowhere to go and are dropped (no surviving
// entry means goccy would emit an empty body anyway; keeping the comment orphans it).
func reattachDocLevelAfterRemoval(values []*yamlast.MappingValueNode, head []*yamlast.CommentNode, foot *yamlast.CommentGroupNode) {
	if len(values) == 0 {
		return
	}
	if len(head) > 0 {
		first := values[0]
		if first.Comment == nil {
			first.Comment = &yamlast.CommentGroupNode{Comments: head}
		} else {
			first.Comment.Comments = append(head, first.Comment.Comments...)
		}
	}
	if foot != nil {
		last := values[len(values)-1]
		if last.FootComment == nil {
			last.FootComment = foot
		} else {
			last.FootComment.Comments = append(last.FootComment.Comments, foot.Comments...)
		}
	}
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
	// Filter the root map when n itself is one; the walker below only
	// visits maps that are values of a MappingValueNode.
	if mn, ok := n.(*yamlast.MappingNode); ok {
		dropNullChildren(mn, nil)
	}
	ast.Walk(n, func(node yamlast.Node) bool {
		mv, ok := node.(*yamlast.MappingValueNode)
		if !ok {
			return true
		}
		if child, ok := mv.Value.(*yamlast.MappingNode); ok {
			dropNullChildren(child, mv.Key)
		}
		return true
	})
}

// Drops null-valued entries from mn. When mn ends empty and parentKey
// is known, reflows the map with fresh `{}` bracket tokens anchored
// inline next to `parentKey:` - goccy renders emptied block maps as
// `parent:\n  {}` on a broken second line otherwise.
func dropNullChildren(mn *yamlast.MappingNode, parentKey yamlast.Node) {
	filtered := mn.Values[:0]
	removed := 0
	for _, mv := range mn.Values {
		if ast.IsNullNode(mv.Value) {
			removed++
			continue
		}
		filtered = append(filtered, mv)
	}
	mn.Values = filtered
	if len(mn.Values) > 0 || removed == 0 || parentKey == nil {
		return
	}
	fresh, err := parser.ParseBytes([]byte("k: {}\n"), parser.ParseComments)
	if err != nil {
		return
	}
	*mn = *fresh.Docs[0].Body.(*yamlast.MappingNode).Values[0].Value.(*yamlast.MappingNode)
	kt := parentKey.GetToken()
	if kt == nil || mn.Start == nil || mn.End == nil {
		return
	}
	line := kt.Position.Line
	col := kt.Position.Column + len(ast.KeyString(parentKey)) + 2
	mn.Start.Position.Line, mn.Start.Position.Column = line, col
	mn.End.Position.Line, mn.End.Position.Column = line, col+1
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
