package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strconv"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"

	"github.com/andrew-grechkin/update-yaml/internal/ingest"
	"github.com/andrew-grechkin/update-yaml/internal/transform"
	"github.com/andrew-grechkin/update-yaml/pkg/output"
	"github.com/andrew-grechkin/update-yaml/pkg/style"
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

	mergedDocs, err := ingest.ReadAndMergeDataFiles(args[1:])
	if err != nil {
		return err
	}

	file, err := parser.ParseBytes(stdinBytes, parser.ParseComments)
	if err != nil {
		return fmt.Errorf("error parsing stdin yaml: %w", err)
	}
	style.Active = detectStyle(file)

	// If data files were provided, they must cover every STDIN doc. Extra
	// data docs are ignored (a separate concern), but a shortfall is fatal:
	// silently leaving later stdin docs unupdated tends to hide config bugs.
	if len(args) > 1 && len(mergedDocs) < len(file.Docs) {
		return fmt.Errorf("stdin has %d documents but data covers only %d", len(file.Docs), len(mergedDocs))
	}

	if err := transform.ApplyMergedDocs(file, mergedDocs); err != nil {
		return err
	}

	return output.Write(stdout, file, stdinBytes, modifiedDocs(file, mergedDocs))
}

// Reports per-doc whether updates were applied. Untouched docs are
// passed through verbatim from the source to dodge goccy's cosmetic
// re-rendering (e.g. `{ }` -> `{}`).
func modifiedDocs(file *ast.File, mergedDocs []*ast.DocumentNode) []bool {
	modified := make([]bool, len(file.Docs))
	for i := range file.Docs {
		if i < len(mergedDocs) && !ingest.IsEmptyDataDoc(mergedDocs[i]) {
			modified[i] = true
		}
	}
	return modified
}

// Overrides defaults with source-file evidence and the
// UPDATE_YAML_MAX_LINE_WIDTH environment variable. Indent comes from the
// first parent->child mapping pair in source; max line width comes from
// the env var when set to a positive integer, otherwise the default. All
// other fields keep their package-level defaults. Quote-style detection
// was removed with the AST-driven data path - data-provided values honor
// whatever quote style data used.
func detectStyle(file *ast.File) style.Style {
	s := style.Active
	for _, doc := range file.Docs {
		if n := findIndentInNode(doc.Body); n > 0 {
			s.Indent = n
			break
		}
	}
	// Folding is opt-in via UPDATE_YAML_MAX_LINE_WIDTH. Only a positive
	// integer enables it and sets the column budget; unset, empty,
	// non-numeric, zero, and negative all leave folding disabled.
	if w := envPositiveInt("UPDATE_YAML_MAX_LINE_WIDTH"); w > 0 {
		s.MaxLineWidth = w
	}
	return s
}

// Reads an environment variable as a positive int, returning 0 for
// unset, empty, non-numeric, or non-positive values so callers can gate
// behavior with a single `> 0` check.
func envPositiveInt(name string) int {
	v := os.Getenv(name)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	return n
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
