package patch

import (
	"strings"
	"testing"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

func TestTrailingNewlines(t *testing.T) {
	cases := map[string]int{
		"":         0,
		"x":        0,
		"x\n":      1,
		"x\n\n":    2,
		"x\n\n\n":  3,
		"\n":       1,
		"\n\n":     2,
		"x\ny":     0,
		"x\ny\n":   1,
		"x\ny\n\n": 2,
	}
	for in, want := range cases {
		if got := TrailingNewlines(in); got != want {
			t.Errorf("TrailingNewlines(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestNeedsChompFix(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{"literal_plain", "k: |\n  x\n", false},        // `|` clip - no fix needed
		{"literal_strip", "k: |-\n  x\n", false},       // `|-` strip - no `+`
		{"keep_one_trailing", "k: |+\n  x\n", false},   // `|+` with 1 trailing NL - goccy handles
		{"keep_two_trailing", "k: |+\n  x\n\n", true},  // `|+` with 2+ trailing NLs - needs fix
		{"folded_keep_two", "k: >+\n  x\n\n", true},    // `>+` also gets the fix
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := parser.ParseBytes([]byte(c.src), parser.ParseComments)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			mn := f.Docs[0].Body.(*ast.MappingNode)
			lit, ok := mn.Values[0].Value.(*ast.LiteralNode)
			if !ok {
				t.Fatalf("value is %T, want LiteralNode", mn.Values[0].Value)
			}
			if got := NeedsChompFix(lit); got != c.want {
				t.Errorf("NeedsChompFix(%q) = %v, want %v", c.src, got, c.want)
			}
		})
	}
}

func TestLiteralBugsWrapsKeepChomp(t *testing.T) {
	src := "k: |+\n  one\n  two\n\n"
	f, err := parser.ParseBytes([]byte(src), parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	LiteralBugs(f.Docs[0].Body)

	mv := f.Docs[0].Body.(*ast.MappingNode).Values[0]
	if _, ok := mv.Value.(*KeepChompLiteral); !ok {
		t.Errorf("LiteralBugs did not wrap `|+` value; got %T", mv.Value)
	}

	// Render must include the trailing blank that goccy would otherwise strip.
	rendered := f.String()
	if !strings.Contains(rendered, "one\n  two\n\n") {
		t.Errorf("render lost trailing blank; got %q", rendered)
	}
}

func TestLastChainToken(t *testing.T) {
	// LiteralNode: LastChainToken returns the content String's token,
	// not the header token.
	src := "k: |+\n  content\n\n"
	f, _ := parser.ParseBytes([]byte(src), parser.ParseComments)
	mv := f.Docs[0].Body.(*ast.MappingNode).Values[0]
	lit := mv.Value.(*ast.LiteralNode)
	tok := LastChainToken(lit)
	if tok == nil {
		t.Fatalf("LastChainToken returned nil for LiteralNode")
	}
	if tok == lit.Start {
		t.Errorf("LastChainToken returned the header token; want the content token")
	}
	if tok != lit.Value.Token {
		t.Errorf("LastChainToken should return lit.Value.Token, got %p want %p", tok, lit.Value.Token)
	}

	// Plain scalar: LastChainToken returns the node's own GetToken().
	src2 := "k: plain\n"
	f2, _ := parser.ParseBytes([]byte(src2), parser.ParseComments)
	mv2 := f2.Docs[0].Body.(*ast.MappingNode).Values[0]
	tok2 := LastChainToken(mv2.Value)
	if tok2 == nil || tok2 != mv2.Value.GetToken() {
		t.Errorf("LastChainToken for plain scalar should be n.GetToken(); got %p want %p", tok2, mv2.Value.GetToken())
	}

	// Nil input: LastChainToken returns nil.
	if LastChainToken(nil) != nil {
		t.Errorf("LastChainToken(nil) should return nil")
	}
}
