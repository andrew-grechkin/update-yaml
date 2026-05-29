# Contributing to update-yaml

## Development Setup

To get started with development, you'll need Go (version 1.25 or higher) installed on your system.
Optionally, you can install `just` for simplified command execution.

### Building the Project

If you have `just` installed, you can build the executable with:

```bash
just build
```

This command compiles the `main.go` file and places the `update-yaml` executable in `$XDG_CACHE_HOME/go/bin`.

Alternatively, without `just`, you can use `go build`:

```bash
go build -o "$(go env GOCACHE)/bin/update-yaml" .
```

Ensure that `$XDG_CACHE_HOME/go/bin` (or `$(go env GOCACHE)/bin`) is included in your system's `PATH` environment
variable to run `update-yaml` from any directory.

### Updating Dependencies

To update the project's Go dependencies and clean up `go.mod` and `go.sum`, use:

```bash
just update
```

## Code Style and Linting

```bash
just fix
```

```bash
just lint
```

## Testing

Run the test suite with:

```bash
just test
```

Test fixtures are located in `test/fixtures`.
