package main

// Ingest: read data files as YAML/JSON, keep them as AST all the way
// through, and merge N data files into one per source-doc slot. Because
// data now flows through as AST, downstream code can honor whatever style
// the author wrote (block vs flow, single vs double quote, inline
// comments) instead of re-marshaling from Go values.

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

// Reads each file as an AST and merges across files at the doc level.
// The returned slice is indexed by doc slot: entry i is the merged data
// for stdin's doc i, or nil if that slot has no data.
func readAndMergeDataFiles(files []string) ([]*ast.DocumentNode, error) {
	maxLen := 0
	perFile := make([][]*ast.DocumentNode, 0, len(files))
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
	merged := make([]*ast.DocumentNode, maxLen)
	for _, docs := range perFile {
		for i, d := range docs {
			merged[i] = mergeDocs(merged[i], d)
		}
	}
	return merged, nil
}

// Parses a YAML (or JSON, which parses as YAML) file into a slice of
// DocumentNodes. Comments are preserved so data-provided inline comments
// can flow through to the output.
func readMultiDoc(path string) ([]*ast.DocumentNode, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, fmt.Errorf("error reading %s: %w", path, err)
	}
	file, err := parser.ParseBytes(b, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("error parsing %s: %w", path, err)
	}
	return file.Docs, nil
}

// Combines two DocumentNodes into one at the doc level. When both have
// MappingNode bodies the result is a deep merge; otherwise the src doc
// wins wholesale (matches the "last-write-wins for non-map values" rule
// from the pre-AST MapSlice merge).
func mergeDocs(dst, src *ast.DocumentNode) *ast.DocumentNode {
	if dst == nil {
		return src
	}
	if src == nil {
		return dst
	}
	dstMap, dstOk := dst.Body.(*ast.MappingNode)
	srcMap, srcOk := src.Body.(*ast.MappingNode)
	if dstOk && srcOk {
		dst.Body = mergeMappings(dstMap, srcMap)
		return dst
	}
	return src
}

// Deep-merges src into dst. First-occurrence wins for order (matching
// the previous MapSlice behavior); values follow last-write-wins,
// including explicit NullNode which the apply step later interprets as
// "remove". Two matching MappingNode values recurse; mismatched types get
// wholesale overwrite.
func mergeMappings(dst, src *ast.MappingNode) *ast.MappingNode {
	if len(dst.Values) == 0 {
		return src
	}
	for _, sMV := range src.Values {
		sk := keyString(sMV.Key)
		dst = mergeInto(dst, sk, sMV)
	}
	return dst
}

func mergeInto(dst *ast.MappingNode, key string, sMV *ast.MappingValueNode) *ast.MappingNode {
	for _, dMV := range dst.Values {
		if keyString(dMV.Key) != key {
			continue
		}
		if dSub, ok := dMV.Value.(*ast.MappingNode); ok {
			if sSub, ok := sMV.Value.(*ast.MappingNode); ok {
				dMV.Value = mergeMappings(dSub, sSub)
				return dst
			}
		}
		dMV.Value = sMV.Value
		return dst
	}
	dst.Values = append(dst.Values, sMV)
	return dst
}

// Reports whether the doc slot has no updates to apply - used to skip
// source docs that don't need modification. Matches the pre-AST
// `len(mergedDocs[i]) == 0` check.
func isEmptyDataDoc(d *ast.DocumentNode) bool {
	if d == nil || d.Body == nil || isNullNode(d.Body) {
		return true
	}
	if mn, ok := d.Body.(*ast.MappingNode); ok {
		return len(mn.Values) == 0
	}
	return false
}
