#!/usr/bin/env -S just --one --justfile

export tool := 'update-yaml'

# Build the binary to cache directory
@build: fix
    go build -o "$XDG_CACHE_HOME/go/bin/"

# Install the binary globally
@install:
    go install "$tool"

# Format Go source code
@fix:
    go fmt
    go fix

# Run Go linter
@lint: cc
    go vet

# Report functions over cyclomatic complexity 15 (installs gocyclo if missing)
@cc:
    test -x "$XDG_CACHE_HOME/go/bin/gocyclo" || GOBIN="$XDG_CACHE_HOME/go/bin" go install github.com/fzipp/gocyclo/cmd/gocyclo@latest
    "$XDG_CACHE_HOME/go/bin/gocyclo" -over 15 .

probe: fix
    #!/usr/bin/env -S bash -Eeuo pipefail
    bin="${GOBIN:-${GOPATH:-$HOME/go}/bin}/$tool"
    go build -tags debug -o "$bin"
    source=$'---\nhash:\n  very_long_base_key:\n    a: old value # some comment\n\n    very_long_subkey_b: word\n    # separate key\n    c: "42"\n'
    data=$'---\nhash:\n  very_long_base_key:\n    very_long_subkey_b: |+\n      43\n\n...\n'
    # data=$'---\nhash:\n  b: >\n    hello world\n'
    # data=$'---\nhash:\n  a: |+\n    multi\n    line\n\n\n  b: >\n    hello world\n  c: just words'
    "$bin" <<< "$source" <(echo "$data")

# Update Go dependencies
@update:
    go get -u
    go mod tidy

# Run Go unit tests
@test-unit:
    go test -v ./...

# Run integration tests by driving the binary against fixtures
test-int: build
    #!/usr/bin/env -S bash -Eeuo pipefail

    bin="${GOBIN:-${GOPATH:-$HOME/go}/bin}/$tool"
    failures=0

    for f in test/fixtures/*-expected.yaml; do
        name=$(basename "$f" -expected.yaml)
        source="test/fixtures/${name}-source.yaml"
        if [[ ! -f "$source" ]]; then continue; fi

        data=()
        if [[ -f "test/fixtures/${name}-data.yaml" ]]; then
            data=("test/fixtures/${name}-data.yaml")
        else
            base="test/fixtures/${name}-base.yaml"
            over="test/fixtures/${name}-override.yaml"
            if [[ -f "$base" && -f "$over" ]]; then
                data=("$base" "$over")
            fi
        fi

        echo -n "Testing $name... " >&2
        if result=$("$bin" "${data[@]}" < "$source") && [[ "$result" == "$(cat "$f")" ]]; then
            echo "✓ PASS" >&2
        else
            echo "✗ FAIL" >&2
            diff -u --label "$f" --label "actual" "$f" <(echo "$result") >&2 || true
            failures=$((failures + 1))
        fi
    done

    if (( failures > 0 )); then
        echo "$failures fixture(s) failed" >&2
        exit 1
    fi

# Run all tests
test: lint test-unit test-int
