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
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
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

	return writeOutput(stdout, file)
}

// applyMergedDocs walks file.Docs and applies merged data per-index. Each doc
// is updated through a temporary single-doc *ast.File wrapping the same
// DocumentNode pointer, because yaml.Path operations otherwise match every
// doc in the file at once. Mutations propagate back to file.
func applyMergedDocs(file *ast.File, mergedDocs []map[string]any) error {
	for i, doc := range file.Docs {
		if doc.Body == nil {
			continue
		}
		if i >= len(mergedDocs) || mergedDocs[i] == nil {
			continue
		}
		scoped := &ast.File{Docs: []*ast.DocumentNode{doc}}
		if err := updateAtNode(scoped, doc.Body, "$", mergedDocs[i]); err != nil {
			return err
		}
	}
	return nil
}

func writeOutput(stdout io.Writer, file *ast.File) error {
	out := file.String()
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	_, err := fmt.Fprint(stdout, out)
	return err
}

// mergeDataFiles reads each file as multi-document YAML and merges across
// files index-by-index: result[i] is the merge of all files' i-th document.
// Files with fewer docs contribute nothing past their last doc.
func mergeDataFiles(files []string) ([]map[string]any, error) {
	maxLen := 0
	perFile := make([][]map[string]any, 0, len(files))
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
	merged := make([]map[string]any, maxLen)
	for _, docs := range perFile {
		for i, d := range docs {
			merged[i] = mergeInto(merged[i], d)
		}
	}
	return merged, nil
}

func readMultiDoc(path string) ([]map[string]any, error) {
	r, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("error reading %s: %w", path, err)
	}
	defer r.Close()
	dec := yaml.NewDecoder(r)
	var docs []map[string]any
	for {
		var m map[string]any
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

// mergeInto merges src into dst. Later values win including explicit nils,
// which the walker later interprets as "remove this key from the output".
//
// I don't use dario.cat/mergo here. mergo.WithOverride skips zero values in
// src, so a later file's `key: null` silently fails to override an earlier
// non-nil value. mergo.WithOverwriteWithEmptyValue overrides with zero values,
// but treats nil and empty string / empty slice the same - so it would also
// erase keys whose final value happens to be `""` or `[]`, which is wrong.
// I need exactly nil-overrides-non-nil with no other changes to zero-value
// handling, so a small custom merge is clearer than tuning mergo flags.
func mergeInto(dst, src map[string]any) map[string]any {
	if dst == nil {
		return src
	}
	for k, v := range src {
		if subSrc, ok := v.(map[string]any); ok {
			if subDst, ok := dst[k].(map[string]any); ok {
				dst[k] = mergeInto(subDst, subSrc)
				continue
			}
		}
		dst[k] = v
	}
	return dst
}

// updateAtNode walks a mapping node and updates values for keys present in
// data. When both sides are mappings I recurse instead of replacing wholesale,
// so that nested comments and keys not mentioned in data are preserved.
// Keys present in data but absent from the mapping are appended at the end
// (recursively, for nested mappings) in alphabetical order. An explicit nil
// value in data removes the corresponding key from the output (or skips
// appending if the key did not exist).
func updateAtNode(file *ast.File, node ast.Node, path string, data map[string]any) error {
	seen, toRemove, err := updateExistingEntries(file, node, path, data)
	if err != nil {
		return err
	}

	mn, ok := node.(*ast.MappingNode)
	if !ok {
		return nil
	}
	removeMarkedEntries(mn, toRemove)
	return appendMissingEntries(mn, data, seen)
}

// updateExistingEntries iterates the mapping's existing entries and either
// replaces, recurses into, or marks them for removal based on data. Returns
// the set of keys it touched and the set of MappingValueNodes to remove.
func updateExistingEntries(
	file *ast.File, node ast.Node, path string, data map[string]any,
) (map[string]bool, map[*ast.MappingValueNode]bool, error) {
	seen := make(map[string]bool, len(data))
	toRemove := make(map[*ast.MappingValueNode]bool)
	for _, mv := range mappingValues(node) {
		key := keyString(mv.Key)
		if key == "" {
			continue
		}
		val, ok := data[key]
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

// applyValue updates mv to reflect val: recurses into a sub-mapping when
// both sides are mappings, otherwise replaces the scalar/sequence value.
func applyValue(file *ast.File, mv *ast.MappingValueNode, val any, childPath string) error {
	if subMap, ok := val.(map[string]any); ok && mappingValues(mv.Value) != nil {
		return updateAtNode(file, mv.Value, childPath, subMap)
	}
	if err := replaceAt(file, childPath, val); err != nil {
		return fmt.Errorf("replacing %s: %w", childPath, err)
	}
	return nil
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
// Appended in alphabetical order for deterministic output.
func appendMissingEntries(mn *ast.MappingNode, data map[string]any, seen map[string]bool) error {
	missing := make([]string, 0, len(data))
	for k, v := range data {
		if seen[k] || v == nil {
			continue
		}
		missing = append(missing, k)
	}
	sort.Strings(missing)
	for _, k := range missing {
		extra, err := buildSingleEntryMapping(k, data[k])
		if err != nil {
			return fmt.Errorf("building entry %q: %w", k, err)
		}
		mn.Merge(extra)
	}
	return nil
}

func buildSingleEntryMapping(key string, val any) (*ast.MappingNode, error) {
	out, err := yaml.Marshal(map[string]any{key: val})
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
// anything else is quoted via brackets.
func joinPath(prefix, key string) string {
	if isSimpleIdent(key) {
		if prefix == "$" {
			return "$." + key
		}
		return prefix + "." + key
	}
	q := strings.ReplaceAll(key, "'", "''")
	return prefix + "['" + q + "']"
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
// preserving any inline/trailing comment that was on the old value.
// goccy's ReplaceWithReader otherwise drops the trailing comment.
func replaceAt(file *ast.File, path string, val any) error {
	out, err := yaml.Marshal(val)
	if err != nil {
		return fmt.Errorf("marshalling new value: %w", err)
	}
	p, err := yaml.PathString(path)
	if err != nil {
		return fmt.Errorf("invalid path %s: %w", path, err)
	}

	nodeAt := func() ast.Node {
		n, err := p.FilterFile(file)
		if err != nil {
			return nil
		}
		return n
	}

	var savedComment *ast.CommentGroupNode
	if n := nodeAt(); n != nil {
		savedComment = n.GetComment()
	}
	if err := p.ReplaceWithReader(file, bytes.NewReader(out)); err != nil {
		return err
	}
	if savedComment != nil {
		if n := nodeAt(); n != nil {
			_ = n.SetComment(savedComment)
		}
	}
	return nil
}
