// Multi-document byte splicing: renders modified docs via goccy and
// splices the original source bytes verbatim for untouched ones, so
// cosmetic round-trip changes (e.g. `{ }` -> `{}`, quote-style drift on
// keys never touched) don't leak into the output. Inter-doc blank lines
// are preserved by stripping trailing newlines from each part and joining
// with a separator computed from the source.
package output

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/goccy/go-yaml/ast"
)

// Writes file to stdout, using goccy's serializer for docs flagged in
// modified and splicing the original source bytes verbatim for the rest.
// A trailing newline is always emitted at the end.
func Write(stdout io.Writer, file *ast.File, original []byte, modified []bool) error {
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
