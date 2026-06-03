# update-yaml

[![Go Reference](https://pkg.go.dev/badge/github.com/andrew-grechkin/update-yaml.svg)](https://pkg.go.dev/github.com/andrew-grechkin/update-yaml)
[![Go Report Card](https://goreportcard.com/badge/github.com/andrew-grechkin/update-yaml)](https://goreportcard.com/report/github.com/andrew-grechkin/update-yaml)

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
update-yaml <<< 'name: old # some name' <(echo 'name: new')
```

```bash
# source from STDIN, multiple data files merged in order (later wins)
update-yaml < source.yaml base.yaml override.yaml
```

## OPTIONS

- -h, --help Display help message
- -m, --man Display full readme (tip: update-yaml --man | colored-md)
- -v, --version Display version information (tip: update-yaml --version | jq -r .Version)

## ENVIRONMENT

- `UPDATE_YAML_PREFER_ORDER_PRESERVED` - when set, new keys are appended at the end of their mapping in the order
  declared by data files, instead of being spliced in alphabetically among the existing keys (the default).
- `UPDATE_YAML_PREFER_SINGLE_QUOTE` - when set, values that need quoting are emitted with single quotes regardless of
  what the source file uses. Plain strings stay plain - quotes are never added to values that don't need them, and
  existing quoted values keep their original style on replace.

## INSTALLATION

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
- Preserves key order of the original document
- Auto-detects and preserves indent width (block mappings) and sequence indent style (flush vs. extra-indented)
- Auto-detects single vs. double quote preference from the source for new emissions; replaced values keep their original
  quote style when meaningful, or drop quotes the new value doesn't need
- Deep merges multiple data files (like Helm) with first-occurrence-wins ordering: later files override values, but new
  keys are appended at the end while existing keys keep their slot
- Explicit `null` in data removes the corresponding key from the output
- Anchors (`&name`) and aliases (`*name`) are preserved
- Multi-document YAML supported on both sides (STDIN doc[i] is updated by merged data doc[i])

## USAGE

### Single data file

Update with inline data:

```bash
update-yaml <<< 'name: old # some name' <(echo 'name: new')
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

Keys present in data but absent from the source are spliced in among the existing keys at the position they belong
alphabetically. When the source mapping is empty (just being created), keys are appended in the order data declares
them, since there is no existing order to fit into:

```bash
update-yaml <<< $'name: svc\ndescription: desc' <(echo $'version: 1.0\ndescription: my service')
```

Set `UPDATE_YAML_PREFER_ORDER_PRESERVED` to switch to "append in data order" for non-empty mappings as well.

### Sequence indent style is preserved

YAML allows two block-sequence styles: items flush with the parent key, or indented one level deeper. The first style
the source uses (per file) is auto-detected and applied to existing sequences when their value is replaced. New
sequences (added as part of a freshly-appended key) use the indented form, which is the more readable default.

```yaml
# flush style - preserved on replace
items:
- a
- b

# indented style - also preserved on replace
items:
  - a
  - b
```

### Quote style is auto-detected

The first explicitly-quoted string in the source decides the preference for newly-emitted values that need quoting
(numbers-as-strings, looks-like-booleans, etc.). Plain strings stay plain - quotes are never added to values that don't
need them. When replacing a quoted value with another value that also needs quoting, the original quote style is
preserved; if the new value can render plain, it does, dropping unnecessary quotes the source had.

Set `UPDATE_YAML_PREFER_SINGLE_QUOTE` to skip detection and always prefer single quotes.

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

### Folded block scalars (`>`) become literal (`|`) on replace

When a value uses YAML's folded-style block scalar (`>`, with or without `+`/`-` chomping indicators), and that key is
modified by data, the output uses the literal style (`|`) instead.

Reason: the underlying YAML library (`github.com/goccy/go-yaml`) exposes a `UseLiteralStyleIfMultiline` marshalling
option but no folded-style analogue, so multi-line strings always render as literal blocks. Untouched docs pass through
verbatim and are not affected; only the replaced value loses its folded style.

If preserving `>` matters for a specific key, leave it untouched and put any related changes on different keys.

## AUTHOR

- Andrew Grechkin

## LICENSE

This project is licensed under the GNU General Public License Version 2 (GPLv2).
See the `LICENSE` file for details.
