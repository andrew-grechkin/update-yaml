package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

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

	mergedDocs, err := readAndMergeDataFiles(args[1:])
	if err != nil {
		return err
	}

	file, err := parser.ParseBytes(stdinBytes, parser.ParseComments)
	if err != nil {
		return fmt.Errorf("error parsing stdin yaml: %w", err)
	}
	detectedStyle = detectStyle(file)

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

// Reports per-doc whether updates were applied. Untouched docs are
// passed through verbatim from the source to dodge goccy's cosmetic
// re-rendering (e.g. `{ }` -> `{}`).
func modifiedDocs(file *ast.File, mergedDocs []*ast.DocumentNode) []bool {
	modified := make([]bool, len(file.Docs))
	for i := range file.Docs {
		if i < len(mergedDocs) && !isEmptyDataDoc(mergedDocs[i]) {
			modified[i] = true
		}
	}
	return modified
}

// Swaps a nil or null doc body for a fresh empty block mapping so
// subsequent updates have somewhere to land.
func ensureMappingBody(doc *ast.DocumentNode) {
	if doc.Body == nil || isNullNode(doc.Body) {
		doc.Body = newEmptyBlockMapping()
	}
}

// Empty-doc/empty-mapping constructors that go through the parser rather
// than building AST nodes directly, so Start/End tokens and BaseNode are
// populated. An empty mapping renders as `{}` regardless of IsFlowStyle,
// but once entries are added the flag determines block vs flow output.
func newEmptyDoc() *ast.DocumentNode {
	f, _ := parser.ParseBytes([]byte("{}\n"), parser.ParseComments)
	d := f.Docs[0]
	d.Body.(*ast.MappingNode).IsFlowStyle = false
	return d
}

func newEmptyBlockMapping() *ast.MappingNode {
	return newEmptyDoc().Body.(*ast.MappingNode)
}

type style struct {
	indent                    int // spaces per indent level (block mappings)
	maxLineWidth              int // column budget: over this, plain scalars fold into `>` block form
	spacesBeforeInlineComment int // gap between a scalar value and its inline `#` comment
}

// The active style, set once at the top of run() and read by the render
// helpers below. Package-level because run isn't reentrant. Defaults
// align with goccy's own conventions: 2-space indent, 1 space before an
// inline comment, 120-column plain-scalar budget.
var detectedStyle = style{
	indent:                    2,
	maxLineWidth:              120,
	spacesBeforeInlineComment: 1,
}

// Overrides defaults with source-file evidence. Currently just indent
// width: the first parent->child mapping pair sets the step. Quote-style
// detection was removed with the AST-driven data path - data-provided
// values honor whatever quote style data used.
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
	return s
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

// Uses goccy's serializer for modified docs and splices the original
// source bytes verbatim for untouched ones, so cosmetic round-trip
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

// Counts consecutive blank source lines immediately above the doc's
// first-token line. A blank line is one containing only spaces and tabs.
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

// Uses each doc's first-token line (which goccy reports reliably)
// instead of Position.Offset, which goccy can mis-account across
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

// Builds a 1-based table of line-start byte offsets, matching goccy's
// Position.Line indexing; entry 0 is unused.
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
