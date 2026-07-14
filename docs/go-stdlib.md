# `go.mmk` — Go build helpers

## Shipped today (`lib/go.mmk`)

Go is the easy case: `go build`/`go test` manage their own dependency graph
and caching, so mmk mostly needs to wrap the tool and expose the right
verbs, rather than track individual `.go` files itself.

### `go_module`

A phony target wrapping the standard `go` tool verbs. Its `deftype` always
reports "needs run" — the Go toolchain's own incremental cache handles
staleness, so mmk doesn't try to duplicate that.

```bash
deftype go_module { return 1; }

defbody go_module            { go build ${pkg:-./...}; }
defbody go_module test       { go test ${pkg:-./...} -cover; }
defbody go_module fmt        { go fmt ${pkg:-./...}; }
defbody go_module fmt-check  { files=$(gofmt -l .); [ -z "$files" ] || { echo "$files"; exit 1; }; }
defbody go_module vet        { go vet ${pkg:-./...}; }
defbody go_module clean      { go clean; }
defbody go_module update     { go get -u ${pkg:-./...}; go mod tidy; }
```

Options: `pkg=` (defaults to `./...`).

```bash
include go.mmk
go_module .
```

### `go_exe`

A real file-artifact type: the target name *is* the output binary path.
Freshness is just the binary's mtime — mmk doesn't track `.go` sources
individually, so a rebuild after source changes needs `mmk clean <target>`
or removing the binary.

```bash
deftype go_exe {
    [ -f "$target" ] && (stat -c "%Y" "$target" 2>/dev/null || stat -f "%m" "$target" 2>/dev/null)
}

defbody go_exe {
    mkdir -p "$(dirname "$target")"
    CGO_ENABLED=${cgo:-0} go build ${ldflags:+-ldflags="$ldflags"} -o "$target" "${pkg:-.}"
}

defbody go_exe clean { rm -f "$target"; }
defbody go_exe test  { go test "${pkg:-.}"; }
```

Options: `pkg=` (default `.`), `ldflags=`, `cgo=` (default `0`).

```bash
include go.mmk
go_exe bin/myapp pkg=./cmd/myapp
```

## Planned extensions

Grounded in a survey of Makefiles across a range of existing Go projects
(see [case-study.md](case-study.md)): the plain-build/test/vet/fmt wrapper
above is the case nobody actually needs help with. The real recurring pain
points, ranked by how often they showed up and how much boilerplate they
currently cost:

### Version/ldflags injection

**Status: proposed.** Six out of a baseline of thirteen surveyed projects
hand-rolled the same shape: `git describe --tags --always --dirty` (or a
short SHA plus a dirty-check), baked into the binary via `-X pkg.Var=value`.

Since target bodies re-run on every invocation (not just at parse time),
`go_exe`'s body can compute this itself. Proposed: an opt-in option,
`version_pkg=`:

```bash
defbody go_exe {
    mkdir -p "$(dirname "$target")"
    v=${version:-$(git describe --tags --always --dirty 2>/dev/null)}
    lf="${ldflags:-}${version_pkg:+ -X ${version_pkg}.Version=$v}"
    CGO_ENABLED=${cgo:-0} go build ${lf:+-ldflags="$lf"} -o "$target" "${pkg:-.}"
}
```

```bash
go_exe bin/myapp pkg=./cmd/myapp version_pkg=main
```

One option replaces the copy-pasted `git describe`/dirty-check/`-X` dance.

### GOOS/GOARCH cross-compile matrices

**Status: proposed.** Five of the thirteen surveyed projects cross-compiled
via either a hand-written shell `for os/arch` loop or a Make static-pattern
rule over `GOOS`×`GOARCH`. mmk already generalizes this with its `for ... in
[...]` matrix clause — `go_exe` just needs to read `$goos`/`$arch` if the
matrix sets them:

```bash
defbody go_exe {
    mkdir -p "$(dirname "$target")"
    CGO_ENABLED=${cgo:-0} GOOS=${goos:-} GOARCH=${arch:-} \
        go build ${ldflags:+-ldflags="$ldflags"} -o "$target" "${pkg:-.}"
}
```

```bash
go_exe "bin/myapp-$goos-$arch" for goos in [darwin linux windows] for arch in [amd64 arm64] \
    exclude [goos=windows arch=arm64]
```

No loop-writing, no static-pattern-rule tricks — this is mmk's matrix
feature doing what it already does; `go.mmk` just needs to plumb two more
env vars through the existing body.

### Automatic `main`-package discovery

**Status: proposed.** The goal: a blank `Mmkfile` containing only
`include go.mmk` should list every buildable program in the current
directory's module via `mmk -list`, with no `go_exe` line written by hand.

This needs no parser changes — `include $(...)` (see
[language-extensions.md](language-extensions.md#existing-mechanism-include--for-generated-targets))
already lets an include path be the output of an arbitrary command. A
discovery script walks the module scoped to the current directory (which is
always the `Mmkfile`'s directory — mmk never `chdir`s), and for each `main`
package emits a `go_exe` line into a generated `.mmk` fragment, then prints
that fragment's path:

```bash
go_exe bin/cmd/server pkg=./cmd/server into go_mains
go_exe bin/cmd/worker pkg=./cmd/worker into go_mains
group go_mains
```

Target names mirror the package's path relative to the module root
(`bin/<relpath>`, not just the basename) since two `main` packages can share
a directory name across different subtrees, and target names must be
unique. `/` is a legal target-name character, so this needs no escaping. A
`main` package living at the module root itself (no subdirectory to name it
from) falls back to `bin/<module-basename>`.

`go.mmk` includes this generator itself, so it fires automatically:

```bash
include $(_mmk_go_mains)   # inside go.mmk
```

Then any concrete `go_exe` rule a user writes for one of these target names
(e.g. one that needs custom `ldflags` or a GOOS/GOARCH matrix) simply wins —
concrete rules override whatever was spliced in, no override mechanism
needed.

Each discovered binary is registered `into go_mains`, so downstream targets
can depend on "every discovered binary" without enumerating them:

```bash
release : go_mains   # depends on every discovered main package's binary
```

### `pre_build`/`post_build` hook groups

**Status: proposed**, and now unblocked by the group fix in
[language-extensions.md](language-extensions.md#implemented-declared-groups-always-get-an-aggregator-zero-member-included).
Generated `go_exe` targets can unconditionally depend on a `pre_build`
group (and similarly a `post_build` group for after-build steps), whether
or not anything is registered into it yet:

```bash
go_exe bin/cmd/server pkg=./cmd/server into go_mains : pre_build
group pre_build
```

A user writing a plain `Mmkfile` on top of `include go.mmk` can then hang a
code-generation step (or any other pre-build step) off the discovered
binaries without editing a single generated line:

```bash
generate into pre_build : my_generate_tool {
    my_generate_tool ...
}

tool my_generate_tool {
    go install example.com/some/generator@v1.2.3
}
```

Before the group fix, this only worked once at least one thing was
registered into `pre_build` — a group with no producers had no target at
all, so every `go_exe`'s unconditional `: pre_build` dep would fail outright
on a project with no codegen step. Fixing the zero-member case in core mmk
is what makes this hook-point idiom viable as a default, rather than
something users have to opt into.
