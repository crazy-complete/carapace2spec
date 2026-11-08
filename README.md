# carapace2spec

Static translation of a carapace-sh completer (cobra + carapace, Go) into a
carapace spec (YAML/JSON) — without executing the completer.

## Why

`carapace _carapace spec` only outputs the command/flag structure. Cara reads the same
source tree statically (`golang.org/x/tools/go/packages`) and adds all statically
derivable completion data (values, delimiters, `$multiparts`-syntax, …). Anything
that can only be determined at runtime (callbacks, external actions) is reported
with a warning at `file:line` — not guessed.

## Build

    cd ~/carapace2spec
    go build -o /usr/local/bin/carapace2spec .

## Usage

    carapace2spec [-o output.yaml] [-json] <completer-directory>

Example:

    carapace2spec -o spec.yaml ~/carapace2spec/carapace-bin/completers/common/git_completer

The output follows the carapace spec schema
(`https://carapace.sh/schemas/command.json`). The extra `completion:` section is
carapace2spec-specific (the official `spec` command does not know it).

## Testing

    go test ./...

Requires a carapace-bin checkout in `../carapace-bin` (or `CARA_BIN=/path`),
because the regression tests run against the gold specs generated there
(fixtures: `testdata/<name>/expected.yaml`).

Current cases: grep, git, age, bat, bc, docker — all with full structural parity.
