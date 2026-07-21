# update-yaml

[![Go Reference](https://pkg.go.dev/badge/github.com/andrew-grechkin/update-yaml.svg)](https://pkg.go.dev/github.com/andrew-grechkin/update-yaml)

A CLI filter for updating YAML documents from one or more YAML/JSON data files while preserving comments, key order and
formatting of the original.

Tailored for developers who maintain **hand-edited config files**, **Kubernetes manifests**, **Helm values**, **CI/CD
configs**, or any YAML file where comments and structure matter and a full re-marshal would lose them.

While round-tripping YAML through generic tools (`yq`, `jaq`, ad-hoc scripts) is easy, doing it without destroying
comments, blank lines and the author's intent is not. That's why it is important to separate 2 concerns for this task:
update YAML file and calculate data for update. When these 2 concerns are separated any tool or any language can be used
to prepare a new data set and what is left for `update-yaml` is just take the provided data and inject it into correct
place into existing YAML keeping it as close as possible to the original document.

## SYNOPSIS

```bash
# source from STDIN, single data file
update-yaml <<< 'name: old # some comment' <(echo 'name: new # new comment')
```

```bash
# source from STDIN, multiple data files merged in order (later wins)
update-yaml < source.yaml base.yaml override.yaml
```

## OPTIONS

- -h, --help Display help message
- -m, --man Display full readme (tip: update-yaml --man | colored-md)
- -v, --version Display version information (tip: update-yaml --version | jq -r .Version)

## INSTALLATION

### Using `mise`

```bash
mise use go:github.com/andrew-grechkin/update-yaml@latest
```

### Building from source

```bash
go install github.com/andrew-grechkin/update-yaml@latest
```

By default, `go install` creates binaries in `$GOBIN` or `$GOPATH/bin`.
To make sure you can use the installed binary you need to add this directory to your path.

```bash
# ensure the go install binaries are in your PATH, consider adding to your shell startup config
export PATH="${GOBIN:-${GOPATH:-$HOME/go}/bin}:$PATH"
```

## FEATURES

- Pure CLI filter: reads source YAML from STDIN, writes updated result to STDOUT
- Preserves head, inline and trailing comments on untouched and replaced values
- Data-provided inline comments override the source's on leaf updates; source's comment is kept when data has none
- Preserves key order of the original document; keys added by data land at their alphabetical position when the
  surrounding siblings are already sorted, otherwise appended at the end in data-tree order
- Auto-detects and preserves source's indent width (block mappings); appended subtrees are rescaled to source's indent
  even when the data file used a different step
- Honors data's style (block vs. flow, single vs. double quote) verbatim on the values it provides
- Values that data quoted but that don't need quoting are unquoted on emission (numeric-looking, `true`/`false`/`null`,
  and YAML 1.1 boolean words like `yes`/`no`/`on`/`off` stay quoted to preserve semantics for older parsers)
- Long single-line plain values fold to `>` block form when the record would exceed 120 columns
- Deep merges multiple data files with first-occurrence-wins ordering; later files override values, new keys appended
- Explicit `null` in data removes the corresponding key from the output
- Anchors (`&name`) and aliases (`*name`) are preserved
- Multi-document YAML supported on both sides (STDIN doc[i] is updated by merged data doc[i])

## USAGE

### Single data file

Update with inline data:

```bash
update-yaml <<< 'name: old # some comment' <(echo 'name: new # new comment')
```

Or with actual files:

```bash
update-yaml < source.yaml data.yaml
```

Or [UUOC](<https://en.wikipedia.org/wiki/Cat_(Unix)#Useless_use_of_cat>), if one would like:

```bash
cat source.yaml | update-yaml data.yaml
```

### Multiple data files with deep merge

```bash
update-yaml << EO_INPUT <(echo 'replicas: 5') <(echo 'config: {debug: true, cache: enabled}')
name: svc

# one replica is enough
replicas: 1

config:
  timeout: 30 # timeout is required
  debug: false
EO_INPUT
```

Or with actual files:

```bash
update-yaml < source.yaml base.yaml override.yaml override2.yaml
```

Later files override earlier ones (just like with `helm install -f base.yaml -f override.yaml`).

### Comments and format are preserved

Given a source like:

```yaml
# Database connection settings
database:
  host: localhost # default for dev
  port: 5432
```

And data:

```yaml
database:
  host: db.internal
```

The result keeps both the head comment and the inline comment and only the value changes:

```yaml
# Database connection settings
database:
  host: db.internal # default for dev
  port: 5432
```

### Removing keys with explicit nulls

Setting a key to `null` in the merged data removes it from the output:

```bash
update-yaml << EO_INPUT <(echo 'debug: null')
---
name: svc # this is a name of a service
# debugging disabled
debug: false
EO_INPUT
```

This works at any depth and survives merging - a later data file can null out a key set by an earlier one.

### Appending new keys

Placement depends on what the source's existing siblings look like:

- If the surrounding keys are already in ascending order, each new key is inserted at its own alphabetical position -
  the author clearly kept the mapping sorted, so new entries respect that.
- Otherwise (author-ordered mapping), new keys are appended at the end in the order data declares them.

The YAML merge key `<<:` is pinned to its source position by convention and never gets displaced or slotted past by
sort-insertion.

```bash
# unsorted source -> new keys appended at end, data order preserved
update-yaml <<< $'name: svc\ndescription: desc' <(echo $'version: 1.0\ndescription: my service')

# sorted source -> new keys inserted at their alphabetical position
update-yaml <<< $'alpha: 1\ncharlie: 3' <(echo $'bravo: 2')
```

The appended subtree's nested indent is rescaled to source's indent width, so a data file written with 2-space indent
lands cleanly into a source using 4-space indent (or vice versa).

### Data's style is honored, quotes are dropped when safe

Data provides the value in whatever style it likes - flow (`items: [a, b]`) or block, single-quoted or double-quoted,
plain or block scalar - and that style flows through to the output. The one adjustment: values that data quoted but
that would round-trip fine as plain scalars get their quotes dropped. Numeric-looking strings (`'1982'`), reserved
words (`'true'`, `'null'`, `'~'`), and YAML 1.1 boolean spellings (`'yes'`, `'On'`, `'off'`) stay quoted to preserve
semantics for downstream parsers.

Long single-line plain values whose full record would exceed 120 columns are folded to `>` block form:

```yaml
# data value: "This is a very long single-line description that clearly exceeds the line width and should be broken..."
description: >-
  This is a very long single-line description that clearly exceeds the line width and should be broken into
  multiple lines to improve readability of the YAML file.
```

### Data-provided inline comments override source

If data has an inline comment on a leaf key, it replaces the source's on that key. Source's comment is preserved when
data doesn't provide one. Comments on branch (mapping) keys are never overridden - the tool doesn't rewrite non-leaf
keys, so data's branch-key comment has no attachment point.

```yaml
# source
service:  # some comment on service
  port: 8080  # default for dev

# data
service:  # ignored - service is a branch
  port: 9090  # updated comment

# result
service:  # some comment on service
  port: 9090  # updated comment
```

### Empty input is valid

Empty STDIN is a valid YAML stream containing zero documents. When data is provided, the data becomes the result. A
single `---` marker on STDIN counts as one empty document; data is injected into it and the leading marker is kept.
A `{}` flow-style mapping likewise accepts data, and the flow style is preserved in the output.

```bash
update-yaml <<< ''    <(echo 'foo: bar')   # → foo: bar
update-yaml <<< '---' <(echo 'foo: bar')   # → ---\nfoo: bar
update-yaml <<< '{}'  <(echo 'foo: bar')   # → {foo: bar}
```

### Multi-document YAML

If the source STDIN contains multiple YAML documents (`---` separated), each is updated independently by the merged
data document at the same index. When data files are provided they must cover **every** STDIN doc; fewer data docs than
STDIN docs is an error. Extra data docs (beyond STDIN's count) are ignored.

```bash
update-yaml << EO_INPUT <(echo $'{"debug": null}\n---\n{"size": "micro","service": {"enabled": false}}')
---
name: svc
debug: false
---
service:
  name: svc
  enabled: true
EO_INPUT
```

#### Targeting non-first STDIN docs

A data file may target a non-first STDIN doc by including explicit empty placeholder docs at the front, in canonical
YAML 1.2 stream form. Use `{}` (empty mapping) or `null` as the placeholder body for any doc that should not contribute
updates. Trailing `...` end markers are supported but optional.

The most compact form uses an inline placeholder:

```yaml
--- {}
---
real: updates for the second doc in the input
```

Repeat the `--- {}` line to skip more leading docs. Files emitted by tools that produce canonical streams (e.g. `jaq
--to yaml` or `yq -y`) also work as-is:

```yaml
---
{}
...
---
real: updates for the second doc in the input
...
```

## EXIT CODES

- **0**: Success
- **1**: Parse errors, invalid input, doc count mismatch, or other runtime errors

## GOTCHAS

### [`github.com/goccy/go-yaml`](https://github.com/goccy/go-yaml/pulls) is FULL of bugs and poorly maintained

One can see that there are dozens of pull requests fixing bugs in the library. Maintainers seem just ignore them and
nothing is being fixed for a long time.

This is a bitter irony because author claimed one of the reasons for this library to exist is [poorly maintained](https://github.com/goccy/go-yaml#why-a-new-library) `go-yaml/yaml`.

I'm trying to workaround some of the bugs in my code, but of course something can slip in.

### Consecutive `---` markers collapse into one document

The underlying YAML library (`github.com/goccy/go-yaml`) parses `---\n---\n` as a single document with implicit-null
body, not as two empty documents. If you need N empty documents in a stream, write them with explicit null (or empty
mapping) bodies:

```yaml
--- null
--- null
```

When data is provided for an index which is an explicit `null` or implicit-null `---\n` body is treated as an empty
mapping so the data can land in it. Slots with no data leave the `null` body intact, so unmodified placeholder docs
survive.

## AUTHOR

- Andrew Grechkin

## LICENSE

This project is licensed under the GNU General Public License Version 2 (GPLv2).
See the `LICENSE` file for details.
