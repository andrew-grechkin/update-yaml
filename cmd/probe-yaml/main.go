// probe-yaml reads a YAML document from stdin, parses it (retaining comments), and prints the resulting goccy AST as
// pretty JSON via pkg/dump
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/goccy/go-yaml/parser"

	"github.com/andrew-grechkin/update-yaml/pkg/dump"
)

func main() {
	src, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	file, err := parser.ParseBytes(src, parser.ParseComments)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fmt.Println(dump.DumpJson(dump.Inspect(file)))
}
