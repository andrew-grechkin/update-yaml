package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(name string) string {
	return filepath.Join("test", "fixtures", name)
}

// readFixture reads a fixture file or fatals.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(fixture(name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// runFixtureCase runs the CLI with `source` as stdin and the given fixture
// names as data file arguments, and returns stdout. Fatals on error.
func runFixtureCase(t *testing.T, source string, dataFixtures ...string) string {
	t.Helper()
	args := append([]string{"update-yaml"}, fixturePaths(dataFixtures)...)
	var stdout bytes.Buffer
	if err := run(args, bytes.NewReader(readFixture(t, source)), &stdout); err != nil {
		t.Fatalf("run: %v", err)
	}
	return stdout.String()
}

func fixturePaths(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = fixture(n)
	}
	return out
}

// assertGolden compares got against the fixture's content; fails with a diff.
func assertGolden(t *testing.T, got, expectedFixture string) {
	t.Helper()
	want := string(readFixture(t, expectedFixture))
	if got != want {
		t.Errorf("output mismatch:\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

// assertContains fails if any of the substrings is missing from got.
func assertContains(t *testing.T, got string, substrings ...string) {
	t.Helper()
	for _, s := range substrings {
		if !strings.Contains(got, s) {
			t.Errorf("expected output to contain %q", s)
		}
	}
}

// assertNotContains fails if any of the substrings is present in got.
func assertNotContains(t *testing.T, got string, substrings ...string) {
	t.Helper()
	for _, s := range substrings {
		if strings.Contains(got, s) {
			t.Errorf("expected output NOT to contain %q, got:\n%s", s, got)
		}
	}
}

func TestRunUpdatesAndPreservesComments(t *testing.T) {
	got := runFixtureCase(t, "simple-source.yaml", "simple-base.yaml", "simple-override.yaml")
	assertGolden(t, got, "simple-expected.yaml")

	assertContains(t, got,
		"# Database connection settings",      // head comment preserved
		"host: db.internal # default for dev", // inline comment preserved across replace
		"# timeout in seconds",                // comment above untouched line
		"timeout: 90",                         // value updated by override.yaml
		"- sso",                               // sequence replaced with proper indent
		"  metrics:",                          // nested new key appended under service
		"    enabled: true",                   // ...with correct indent
		"telemetry:",                          // top-level new key appended
		"  endpoint: https://otel.example/v1",
	)

	// Explicit nulls in data must remove the corresponding keys.
	assertNotContains(t, got,
		"port: 5432", // nested null: database.port
		"port: 6432", // (just to make sure no resurrection)
		"debug:",     // top-level null: debug
	)

	// Appended keys must come after existing keys, not before.
	if strings.Index(got, "telemetry:") < strings.Index(got, "service:") {
		t.Errorf("telemetry should be appended after service, got:\n%s", got)
	}
	if strings.Index(got, "  metrics:") < strings.Index(got, "  features:") {
		t.Errorf("metrics should be appended after features, got:\n%s", got)
	}
}

func TestRunNoArgsIsPassthrough(t *testing.T) {
	got := runFixtureCase(t, "simple-source.yaml")
	assertContains(t, got, "host: localhost")
}

// When source siblings are NOT already sorted the tool preserves the data
// author's order for appended keys. Sorted-insertion only kicks in when
// existing keys are ordered; unsorted mappings keep data-tree order.
func TestRunAppendsInDataOrder(t *testing.T) {
	// Source has two keys in author order (m before a) so
	// siblingsAreSorted returns false and new keys append at the end
	// in data-tree order rather than getting sort-inserted.
	source := []byte("m_seed: 1\na_seed: 2\n")
	data := []byte("zeta: 1\nbeta: 2\nalpha: 3\n")
	dataPath := filepath.Join(t.TempDir(), "data.yaml")
	if err := os.WriteFile(dataPath, data, 0o644); err != nil {
		t.Fatalf("write data: %v", err)
	}

	var stdout bytes.Buffer
	if err := run([]string{"update-yaml", dataPath}, bytes.NewReader(source), &stdout); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := "m_seed: 1\na_seed: 2\nzeta: 1\nbeta: 2\nalpha: 3\n"
	if got := stdout.String(); got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

// When source siblings are already sorted, appended keys land at their
// alphabetical position instead of the end - preserving the author's
// ordering convention.
func TestRunSortsAppendedKeysWhenSourceIsSorted(t *testing.T) {
	source := []byte("alpha: 1\ncharlie: 3\n")
	data := []byte("delta: 4\nbravo: 2\n")
	dataPath := filepath.Join(t.TempDir(), "data.yaml")
	if err := os.WriteFile(dataPath, data, 0o644); err != nil {
		t.Fatalf("write data: %v", err)
	}

	var stdout bytes.Buffer
	if err := run([]string{"update-yaml", dataPath}, bytes.NewReader(source), &stdout); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := "alpha: 1\nbravo: 2\ncharlie: 3\ndelta: 4\n"
	if got := stdout.String(); got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

// Updated values keep the quote style the data file used - the tool honors
// data's style verbatim. Appended keys likewise take their style from data.
// Source's own quote style is preserved on untouched values.
func TestRunAppendedKeysHonorDataStyle(t *testing.T) {
	source := []byte(`host: "old"` + "\n")
	dataPath := filepath.Join(t.TempDir(), "data.yaml")
	if err := os.WriteFile(dataPath, []byte("version: '42'\n"), 0o644); err != nil {
		t.Fatalf("write data: %v", err)
	}

	var stdout bytes.Buffer
	if err := run([]string{"update-yaml", dataPath}, bytes.NewReader(source), &stdout); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := "host: \"old\"\nversion: '42'\n"
	if got := stdout.String(); got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestRunHelp(t *testing.T) {
	var stdout bytes.Buffer
	if err := run([]string{"update-yaml", "--help"}, strings.NewReader(""), &stdout); err != nil {
		t.Fatalf("run: %v", err)
	}
	assertContains(t, stdout.String(), "Usage: update-yaml")
}

func TestRunMultiDoc(t *testing.T) {
	got := runFixtureCase(t, "multidoc-source.yaml", "multidoc-data.yaml")
	assertGolden(t, got, "multidoc-expected.yaml")

	if strings.Count(got, "---") != 2 {
		t.Errorf("expected exactly two --- separators (leading + inter-doc), got %d:\n%s", strings.Count(got, "---"), got)
	}
	assertContains(t, got,
		"port: 9090",          // doc[0] app.port updated
		"queue: high",         // doc[1] worker.queue updated
		"new_field: appended", // doc[1] new key
		"extra: keep",         // doc[1] preserved
	)
	assertNotContains(t, got, "debug:") // doc[0] debug removed by null

	// new_field belongs to doc[1], not doc[0].
	if strings.Index(got, "new_field") < strings.Index(got, "---") {
		t.Errorf("new_field should be in doc[1], not doc[0]:\n%s", got)
	}
}

func TestRunFailsWhenDataHasFewerDocs(t *testing.T) {
	source := readFixture(t, "multidoc-source.yaml")
	var stdout bytes.Buffer
	args := []string{"update-yaml", fixture("simple-base.yaml")}
	err := run(args, bytes.NewReader(source), &stdout)
	if err == nil {
		t.Fatalf("expected error for short data, got nil; stdout:\n%s", stdout.String())
	}
	if !strings.Contains(err.Error(), "stdin has 2 documents") {
		t.Errorf("expected error message to mention doc counts, got: %v", err)
	}
}

// The four valid forms for a data file that source a non-first stdin doc.
// All must produce identical output to the all-in-one multidoc-data.yaml case.
func TestRunPlaceholderForms(t *testing.T) {
	cases := []struct {
		name     string
		dataFile string
	}{
		// `{}\n---\n<content>` - no leading start marker
		{"bare", "multidoc-doc1.yaml"},
		// `--- {}\n---\n<content>` - most compact, recommended
		{"inline", "multidoc-inline-placeholder.yaml"},
		// `---\n{}\n---\n<content>` - canonical with start, no footer
		{"canonical no footer", "multidoc-canonical-no-footer.yaml"},
		// `---\n{}\n...\n---\n<content>\n...` - full canonical (jaq/yq output)
		{"canonical full", "multidoc-canonical-form.yaml"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := runFixtureCase(t, "multidoc-source.yaml", "multidoc-doc0.yaml", c.dataFile)
			assertGolden(t, got, "multidoc-expected.yaml")
		})
	}
}
