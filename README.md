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
# target from STDIN, single data file
update-yaml <<< 'name: old # some name' <(echo 'name: new')
```

```bash
# target from STDIN, multiple data files merged in order (later wins)
update-yaml < target.yaml base.yaml override.yaml
```

## OPTIONS

- -h, --help Display help message
- -m, --man Display full readme (tip: update-yaml --man | colored-md)
- -v, --version Display version information (tip: update-yaml --version | jq -r .Version)

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

- Pure CLI filter: reads target YAML from STDIN, writes updated result to STDOUT
- Preserves head, inline and trailing comments on untouched and replaced values
- Preserves key order of the original document
- Deep merges multiple data files (like Helm) - later files override earlier ones
- Multi-document YAML supported on both sides (STDIN doc[i] is updated by merged data doc[i])
- Explicit `null` in data removes the corresponding key from the output
- Keys present only in data files are appended at the end of their mapping (alphabetical order)

## USAGE

### Single data file

Update with inline data:

```bash
update-yaml <<< 'name: old' <(echo 'name: new')
```

Or with actual files:

```bash
update-yaml < target.yaml data.yaml
```

Or [UUOC](<https://en.wikipedia.org/wiki/Cat_(Unix)#Useless_use_of_cat>), if one would like:

```bash
cat target.yaml | update-yaml data.yaml
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
update-yaml < target.yaml base.yaml override.yaml override2.yaml
```

Later files override earlier ones (just like with `helm install -f base.yaml -f override.yaml`).

### Comments and format are preserved

Given a target like:

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
name: svc
debug: false
EO_INPUT
```

This works at any depth and survives merging - a later data file can null out a key set by an earlier one.

### Appending new keys

Keys present in data but absent from the target are appended at the end of their mapping in alphabetical order:

```bash
update-yaml <<< $'name: svc\ndescription: desc' <(echo $'version: 1.0\ndescription: my service')
```

### Multi-document YAML

If the target STDIN contains multiple YAML documents (`---` separated), each is updated independently by the merged
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

## AUTHOR

- Andrew Grechkin

## LICENSE

This project is licensed under the GNU General Public License Version 2 (GPLv2).
See the `LICENSE` file for details.
