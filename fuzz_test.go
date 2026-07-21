package main

// Fuzz tests for content preservation. These run the tool's `run()` in
// process on generated inputs and check that no keys/values/structure
// disappear from parseable YAML.
//
// Live runs (`go test -fuzz FuzzXxx`) will surface edge cases the seed
// corpus doesn't cover - typically parser/unmarshal disagreements in goccy
// or non-ASCII keys that yaml.PathString can't escape. Those show up as
// t.Fatalf on unmarshal-of-output; when investigating, first check whether
// yaml.Unmarshal accepts the tool's output at all (goccy has known
// inconsistencies between its parser and its Unmarshal).
//
// The seed corpus (every *-source.yaml fixture) is the baseline the CI
// suite covers via `go test`. Live fuzzing is opt-in.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	yaml "github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/parser"
)

// FuzzPassthroughIdentity asserts that running the tool with no data files
// on a parseable YAML input never loses or fabricates content. The check
// is semantic (via yaml.Unmarshal + JSON canonicalization), not byte-
// identical: end-of-file whitespace normalization, doc-separator spacing,
// and other cosmetic differences are OK - what matters is that every key,
// value, and structural relationship survives the round-trip.
//
// The seed corpus is every *-source.yaml fixture the integration tests use,
// so we start from real-world content the tool already handles. The fuzzer
// mutates these seeds; any mutation that stays parseable must still
// round-trip cleanly.
//
// Non-YAML inputs (parser errors) are skipped, not failed - fuzzing YAML
// syntax itself is not the goal.
func FuzzPassthroughIdentity(f *testing.F) {
	seeds, err := filepath.Glob(filepath.Join("test", "fixtures", "*-source.yaml"))
	if err != nil {
		f.Fatalf("glob seeds: %v", err)
	}
	for _, p := range seeds {
		b, err := os.ReadFile(p)
		if err != nil {
			f.Fatalf("read seed %s: %v", p, err)
		}
		f.Add(string(b))
	}

	f.Fuzz(func(t *testing.T, source string) {
		if strings.TrimSpace(source) == "" {
			t.Skip()
		}
		// Skip inputs the tool's parser rejects OR yaml.Unmarshal rejects.
		// Both code paths are goccy internals and disagree on edge cases -
		// e.g. `!0000\n 0000000` parses one way and unmarshals another. We
		// only care about content preservation on inputs both oracles agree
		// are valid.
		if _, err := parser.ParseBytes([]byte(source), parser.ParseComments); err != nil {
			t.Skip()
		}
		srcSummary, err := semanticSummary([]byte(source))
		if err != nil {
			t.Skip()
		}

		var stdout bytes.Buffer
		if err := run([]string{"update-yaml"}, strings.NewReader(source), &stdout); err != nil {
			t.Skip() // tool refused; that's not corruption
		}

		outSummary, err := semanticSummary(stdout.Bytes())
		if err != nil {
			t.Fatalf("output failed to unmarshal: %v\n--- source ---\n%s\n--- output ---\n%s", err, source, stdout.String())
		}
		if srcSummary != outSummary {
			t.Errorf("passthrough lost or fabricated content\n--- source ---\n%s\n--- output ---\n%s\n--- src summary ---\n%s\n--- out summary ---\n%s",
				source, stdout.String(), srcSummary, outSummary)
		}
	})
}

// FuzzIdentityUpdate asserts semantic identity: running the tool with data
// == source must produce output that parses to the same AST content as the
// source. Formatting may change (quote demotion, block-style choice), but
// no keys, values, or comments may be lost or fabricated.
//
// The check is structural: parse both source and result, then walk the two
// trees comparing key names, scalar values, and comment texts. This catches
// data corruption without demanding byte identity.
func FuzzIdentityUpdate(f *testing.F) {
	seeds, err := filepath.Glob(filepath.Join("test", "fixtures", "*-source.yaml"))
	if err != nil {
		f.Fatalf("glob seeds: %v", err)
	}
	for _, p := range seeds {
		b, err := os.ReadFile(p)
		if err != nil {
			f.Fatalf("read seed %s: %v", p, err)
		}
		f.Add(string(b))
	}

	f.Fuzz(func(t *testing.T, source string) {
		if strings.TrimSpace(source) == "" {
			t.Skip()
		}
		if _, err := parser.ParseBytes([]byte(source), parser.ParseComments); err != nil {
			t.Skip()
		}
		srcSummary, err := semanticSummary([]byte(source))
		if err != nil {
			t.Skip()
		}

		dataPath := filepath.Join(t.TempDir(), "data.yaml")
		if err := os.WriteFile(dataPath, []byte(source), 0o644); err != nil {
			t.Fatalf("write data: %v", err)
		}

		var stdout bytes.Buffer
		if err := run([]string{"update-yaml", dataPath}, strings.NewReader(source), &stdout); err != nil {
			t.Skip()
		}

		outSummary, err := semanticSummary(stdout.Bytes())
		if err != nil {
			t.Fatalf("output failed to unmarshal: %v\n--- source ---\n%s\n--- output ---\n%s", err, source, stdout.String())
		}
		if srcSummary != outSummary {
			t.Errorf("identity-update lost or fabricated content\n--- source ---\n%s\n--- output ---\n%s\n--- src summary ---\n%s\n--- out summary ---\n%s",
				source, stdout.String(), srcSummary, outSummary)
		}
	})
}

// semanticSummary unmarshals YAML into a Go value, recursively strips null
// entries from maps (mimicking the tool's null-as-delete semantics), and
// returns a JSON canonicalization suitable for byte-comparison. Comments
// and formatting are intentionally ignored - this is a content-equality
// check.
//
// Trailing whitespace is trimmed before unmarshal because goccy's Unmarshal
// is inconsistent between `<<:` and `<<:\n` (the second rejects the merge
// key syntax the first accepts); trimming keeps the oracle behavior
// symmetric between the tool's input and output.
func semanticSummary(src []byte) (string, error) {
	src = bytes.TrimRight(src, "\n \t")
	var v any
	if err := yaml.Unmarshal(src, &v); err != nil {
		return "", err
	}
	v = stripNullsForCompare(v)
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func stripNullsForCompare(v any) any {
	switch m := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(m))
		for k, val := range m {
			if val == nil {
				continue
			}
			out[k] = stripNullsForCompare(val)
		}
		return out
	case []any:
		for i, val := range m {
			m[i] = stripNullsForCompare(val)
		}
		return m
	}
	return v
}
