package style

import (
	"strings"
	"testing"
)

// FoldWrap must budget by display columns, not bytes. Each CJK
// character occupies 2 columns; wrapping a pure-CJK string with a
// 6-column budget should give 3 chars per line, not 6.
func TestFoldWrapMultibyte(t *testing.T) {
	cases := []struct {
		name  string
		input string
		width int
		want  []string
	}{
		{
			name:  "ascii unchanged",
			input: "the quick brown fox jumps over lazy dog",
			width: 15,
			want:  []string{"the quick brown", "fox jumps over", "lazy dog"},
		},
		{
			name:  "cjk atoms budget by display columns",
			input: "一 二 三 四 五 六 七 八",
			width: 6,
			// Each CJK char is 2 display columns; "X Y" is 5 columns,
			// "X Y Z" would be 8 (> 6), so 2 atoms per line.
			want: []string{"一 二", "三 四", "五 六", "七 八"},
		},
		{
			name:  "mixed ascii and cjk",
			input: "hi 世界 foo bar さよなら",
			width: 10,
			// "hi 世界" = 2 + 1 + 4 = 7 cols; "hi 世界 foo" = 7 + 1 + 3 = 11 > 10
			want: []string{"hi 世界", "foo bar", "さよなら"},
		},
		{
			name:  "byte-length budget would over-pack",
			input: "一二 三四",
			width: 5,
			// Each pair is 4 cols; two pairs joined = 9 cols. Budget 5
			// forces split. If FoldWrap measured by bytes, each utf-8
			// char is 3 bytes, joined pairs = 13 bytes, would still
			// split -- but wrongly at different boundaries. This case
			// mostly proves the display measurement is being called.
			want: []string{"一二", "三四"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FoldWrap(c.input, c.width)
			if strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Errorf("FoldWrap(%q, %d) = %q, want %q", c.input, c.width, got, c.want)
			}
		})
	}
}

func TestDisplayWidth(t *testing.T) {
	cases := map[string]int{
		"":       0,
		"abc":    3,
		"世": 2, // Chinese: 2 cols
		"a世b": 4,
	}
	for in, want := range cases {
		if got := DisplayWidth(in); got != want {
			t.Errorf("DisplayWidth(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestIsYAML11Bool(t *testing.T) {
	yes := []string{"y", "Y", "yes", "Yes", "YES", "n", "no", "on", "On", "ON", "off", "OFF"}
	no := []string{"", "true", "false", "maybe", "yesyes", "0"}
	for _, v := range yes {
		if !IsYAML11Bool(v) {
			t.Errorf("IsYAML11Bool(%q) = false, want true", v)
		}
	}
	for _, v := range no {
		if IsYAML11Bool(v) {
			t.Errorf("IsYAML11Bool(%q) = true, want false", v)
		}
	}
}

func TestLooksLikeSexagesimal(t *testing.T) {
	yes := []string{"1:30:00", "1:2:3:4", "5:59", "-1:30:00", "+1:30:00"}
	no := []string{"", "80:80", "9:60", "01:01", "1:30:0a", "abc", "123", "1:", ":30"}
	for _, v := range yes {
		if !LooksLikeSexagesimal(v) {
			t.Errorf("LooksLikeSexagesimal(%q) = false, want true", v)
		}
	}
	for _, v := range no {
		if LooksLikeSexagesimal(v) {
			t.Errorf("LooksLikeSexagesimal(%q) = true, want false", v)
		}
	}
}

func TestSafeToUnquote(t *testing.T) {
	// Values that parse back cleanly as a plain string with the same content.
	safe := []string{
		"hello", "server_01", "path-name", "eu-west-1",
		"https://example.com/path", // colons ok when not `: `
	}
	// Values that must stay quoted:
	// - empty
	// - YAML 1.1 booleans
	// - YAML 1.1 sexagesimal
	// - reserved / resolvable words
	// - numeric-looking strings
	// - values with indicator characters
	unsafe := []string{
		"", "yes", "On", "off",
		"1:30:00",
		"true", "false", "null", "~", ".inf",
		"42", "3.14", "0x2A",
		"- dash-start", "? question", ": colon-start",
		"a: b",  // contains `: `
		"a #b",  // contains ` #`
	}
	for _, v := range safe {
		if !SafeToUnquote(v) {
			t.Errorf("SafeToUnquote(%q) = false, want true", v)
		}
	}
	for _, v := range unsafe {
		if SafeToUnquote(v) {
			t.Errorf("SafeToUnquote(%q) = true, want false", v)
		}
	}
}

func TestSafeToUnquoteInFlow(t *testing.T) {
	// Values that round-trip cleanly whether emitted in block or flow.
	safe := []string{
		"hello", "server_01", "path-name", "eu-west-1",
		"https://example.com/path",
	}
	// Values safe in block but unsafe in flow: they contain one of the
	// five flow indicators (`,`, `[`, `]`, `{`, `}`), so a bare emission
	// like `[^[234][0-9]{2}$]` would fail to reparse.
	unsafeInFlow := []string{
		"^[234][0-9]{2}$",
		"foo,bar",
		"a[b", "a]b", "a{b", "a}b",
	}
	// Values already unsafe in block (empty, YAML 1.1 spellings, etc.)
	// are also unsafe in flow.
	unsafeAnywhere := []string{"", "yes", "1:30:00", "true", "42"}
	for _, v := range safe {
		if !SafeToUnquoteInFlow(v) {
			t.Errorf("SafeToUnquoteInFlow(%q) = false, want true", v)
		}
	}
	for _, v := range unsafeInFlow {
		if !SafeToUnquote(v) {
			t.Errorf("precondition: SafeToUnquote(%q) = false, expected true (block-safe)", v)
		}
		if SafeToUnquoteInFlow(v) {
			t.Errorf("SafeToUnquoteInFlow(%q) = true, want false", v)
		}
	}
	for _, v := range unsafeAnywhere {
		if SafeToUnquoteInFlow(v) {
			t.Errorf("SafeToUnquoteInFlow(%q) = true, want false", v)
		}
	}
}

func TestNeedsQuoting(t *testing.T) {
	quoted := []string{"", " leading-space", "\tleading-tab", "-dash", "?q", ":colon", "#hash",
		"trailing ", "trailing:", "a: b", "a #b", "true", "False", "yes"}
	plain := []string{"foo", "server_01", "hello world"}
	for _, v := range quoted {
		if !NeedsQuoting(v) {
			t.Errorf("NeedsQuoting(%q) = false, want true", v)
		}
	}
	for _, v := range plain {
		if NeedsQuoting(v) {
			t.Errorf("NeedsQuoting(%q) = true, want false", v)
		}
	}
}

// ShouldWrapStringValue's behavior depends on Active.MaxLineWidth for the
// long-single-line branch. Save + restore around each case so cases don't
// leak state.
func TestShouldWrapStringValue(t *testing.T) {
	saved := Active
	defer func() { Active = saved }()

	// Multi-line always wraps (regardless of width).
	Active.MaxLineWidth = 0
	if !ShouldWrapStringValue("a\nb", 3, "k") {
		t.Errorf("multi-line value should always wrap")
	}

	// Single-line with no fold-safe whitespace: never wrap.
	Active.MaxLineWidth = 5
	if ShouldWrapStringValue("short", 3, "k") {
		t.Errorf("short value with no spaces should not wrap")
	}

	// Single-line long enough with spaces: wraps if MaxLineWidth is set.
	Active.MaxLineWidth = 20
	long := strings.TrimRight(strings.Repeat("word ", 10), " ") // spaces inside, no trailing
	if !ShouldWrapStringValue(long, 3, "k") {
		t.Errorf("long value with spaces should wrap when Active.MaxLineWidth is set")
	}

	// Same value with MaxLineWidth = 0: fold disabled, don't wrap.
	Active.MaxLineWidth = 0
	if ShouldWrapStringValue(long, 3, "k") {
		t.Errorf("MaxLineWidth == 0 should disable fold; value should not wrap")
	}

	// Leading/trailing whitespace disqualifies block scalar promotion.
	Active.MaxLineWidth = 20
	if ShouldWrapStringValue(" leading space", 3, "k") {
		t.Errorf("leading-whitespace value should not wrap (block scalars strip)")
	}
	if ShouldWrapStringValue("trailing space ", 3, "k") {
		t.Errorf("trailing-whitespace value should not wrap")
	}
}

// Multibyte content should trigger the fold at the same visual
// boundary as ASCII: plainRecordLen must count display columns, not
// bytes. A pure-CJK value that fits within 120 display cols but has
// higher byte count would incorrectly fold if plainRecordLen used
// len().
func TestShouldWrapStringValueMultibyte(t *testing.T) {
	saved := Active
	defer func() { Active = saved }()
	Active.MaxLineWidth = 120

	// 38 "世" separated by spaces = 113 display cols. Record with key
	// "at120" (5 chars, prefix 7) = 120 cols total, AT threshold.
	// Must NOT wrap.
	atLine := strings.TrimSuffix(strings.Repeat("世 ", 37), " ") + " 世"
	if DisplayWidth(atLine) != 113 {
		t.Fatalf("test setup: expected 113 cols, got %d", DisplayWidth(atLine))
	}
	if ShouldWrapStringValue(atLine, 1, "at120") {
		t.Errorf("value at exact line-width boundary (120 cols) should NOT wrap")
	}

	// Same value, key "over120" (7 chars, prefix 9) = 122 cols total.
	// Must wrap.
	if !ShouldWrapStringValue(atLine, 1, "over120") {
		t.Errorf("value over line-width boundary (122 cols) should wrap")
	}
}
