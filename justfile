#!/usr/bin/env -S just --one --justfile

# Build the binary to cache directory
@build:
    go build -o "$XDG_CACHE_HOME/go/bin/"

# Install the binary globally
@install:
    go install update-yaml

# Format Go source code
@fix:
    go fmt

# Run Go linter
@lint: cc
    go vet

# Report functions over cyclomatic complexity 10 (installs gocyclo if missing)
@cc:
    test -x "$XDG_CACHE_HOME/go/bin/gocyclo" || GOBIN="$XDG_CACHE_HOME/go/bin" go install github.com/fzipp/gocyclo/cmd/gocyclo@latest
    "$XDG_CACHE_HOME/go/bin/gocyclo" -over 15 .

# Update Go dependencies
@update:
    go get -u
    go mod tidy

# Run Go unit tests
@test-unit:
    go test -v ./...

# Run integration tests by driving the binary against fixtures
test-int: build
    #!/usr/bin/env bash
    set -Eeuo pipefail

    bin="${GOBIN:-${GOPATH:-$HOME/go}/bin}/update-yaml"

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
            exit 1
        fi
    done

# Run all tests
test: test-unit test-int
