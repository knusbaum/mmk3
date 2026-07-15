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
go_module . :
```

### `go_exe`

A real file-artifact type: the target name *is* the output binary path.
Freshness is just the binary's mtime — mmk doesn't track `.go` sources
individually, so a rebuild after source changes needs `mmk clean <target>`
or removing the binary. Every `go_exe` build unconditionally depends on a
`pre_build` group (whether or not anything registers into it), giving
projects a hook point for codegen without editing generated lines — see
below.

```bash
group pre_build

deftype go_exe {
    [ -f "$target" ] && (stat -c "%Y" "$target" 2>/dev/null || stat -f "%m" "$target" 2>/dev/null)
}

defbody go_exe : pre_build {
    mkdir -p "$(dirname "$target")"
    CGO_ENABLED=${cgo:-0} GOOS=${goos:-} GOARCH=${goarch:-} \
        go build ${ldflags:+-ldflags="$ldflags"} -o "$target" "${pkg:-.}"
}

defbody go_exe clean { rm -f "$target"; }
defbody go_exe test  { go test "${pkg:-.}"; }
```

Options: `pkg=` (default `.`), `ldflags=`, `cgo=` (default `0`), `goos=`/`goarch=`
(default: unset, i.e. host OS/arch — see [GOOS/GOARCH cross-compile
matrices](#goosgoarch-cross-compile-matrices) below).

```bash
include go.mmk
go_exe bin/myapp pkg=./cmd/myapp :
```

Note the trailing `:` — a rule line with only options and no dep list or
body has no `:`/`{` marker for mmk's parser to commit on, so it's silently
treated as inert passthrough bash rather than a target rule (see
[language-extensions.md](language-extensions.md#pitfall-typed-rules-need-a--or--marker)).
`: pre_build` (or any explicit dep list) satisfies this for free; a bare
options-only line needs the empty `:`.

## `pre_build` hook group

**Status: shipped.** Generated (or hand-written) `go_exe` targets
unconditionally depend on a `pre_build` group, whether or not anything is
registered into it yet — this is viable as a default only because of the
zero-member-group fix in
[language-extensions.md](language-extensions.md#implemented-declared-groups-always-get-an-aggregator-zero-member-included);
before that fix, an empty group had no target at all, so the dep would fail
outright on a project with no codegen step.

A project can hang a code-generation step (or any other pre-build step)
off every `go_exe` target with no edits to `go.mmk` itself:

```bash
generate into pre_build : my_generate_tool {
    my_generate_tool ...
}

tool my_generate_tool {
    go install example.com/some/generator@v1.2.3
}
```

## GOOS/GOARCH cross-compile matrices

**Status: shipped.** A survey of Makefiles across a range of existing Go
projects (see [case-study.md](case-study.md)) found five of thirteen
cross-compiling via either a hand-written shell `for os/arch` loop or a Make
static-pattern rule over `GOOS`×`GOARCH`. mmk already generalizes this with
its `for ... in [...]` matrix clause — `go_exe` just reads `$goos`/`$goarch`
if the matrix sets them (shown above); no loop-writing, no
static-pattern-rule tricks needed:

```bash
go_exe "bin/myapp-$goos-$goarch" for goos in [darwin linux windows] for goarch in [amd64 arm64] \
    exclude [goos=windows goarch=arm64] :
```

## Automatic `main`-package discovery

**Status: shipped.** A blank `Mmkfile` containing only `include go.mmk`
lists every buildable program in the current directory's module via `mmk
-list`, with no `go_exe` line written by hand.

This needs no parser changes — `include $(...)` (see
[language-extensions.md](language-extensions.md#existing-mechanism-include--for-generated-targets))
already lets an include path be the output of an arbitrary command.
`go.mmk` defines a passthrough bash function, `_mmk_go_mains`, and includes
its own output:

```bash
include $(_mmk_go_mains)   # inside go.mmk
```

`_mmk_go_mains` resolves the current module with `go list -m`, lists every
`main` package under the current directory with `go list -e -f '{{if eq
.Name "main"}}{{.ImportPath}}{{end}}' ./...` (`-e` so one broken package
doesn't abort discovery of the rest), and for each one writes a `go_exe`
line into a generated `.mmk` fragment, then prints that fragment's path:

```bash
group go_mains

go_exe bin/cmd/server pkg=example.com/org/foo/cmd/server into go_mains :
go_exe bin/cmd/worker pkg=example.com/org/foo/cmd/worker into go_mains :
```

Target names mirror the package's import path relative to the module root
(`bin/<relpath>`, not just the basename) since two `main` packages can share
a directory name across different subtrees, and target names must be
unique. `/` is a legal target-name character, so this needs no escaping.
`pkg=` is set to the full import path rather than a `./`-relative path —
`go build` treats both identically, and the import path falls straight out
of `go list` with no relative-path computation needed. A `main` package
living at the module root itself (no subdirectory to name it from) falls
back to `bin/<module-basename>`, the last segment of the module's import
path. If there's no `go.mod` (or no `go` binary), discovery degrades to an
empty `go_mains` group rather than erroring — `go_module`/`go_exe` and any
hand-written `go_exe` rules keep working regardless.

The fragment is written to a path deterministic in the project's directory
(`${TMPDIR:-/tmp}/mmk-go-mains-<hash of pwd>.mmk`) and overwritten wholesale
on every parse, so there's no accumulation to clean up and nothing is ever
written into the project tree.

Any concrete `go_exe` rule a user writes for one of these target names (e.g.
one that needs custom `ldflags` or a GOOS/GOARCH matrix) simply wins — the
later declaration in file order overrides the earlier, spliced-in one, no
override syntax needed (see
[language-extensions.md](language-extensions.md#implemented-later-target-rule-wins-on-duplicate-non-verb-target)).

Each discovered binary is registered `into go_mains`, so downstream targets
can depend on "every discovered binary" without enumerating them:

```bash
release : go_mains   # depends on every discovered main package's binary
```

Since a discovered `go_exe` is a normal `go_exe` target, it automatically
gets the `pre_build` dependency described above — no extra wiring needed
for a codegen hook to apply to auto-discovered binaries too.

Known limitations, not fixed: `go list ./...` resolves against the host's
GOOS/GOARCH, so a `main` package gated to a different OS/ARCH by build
constraints won't be discovered on this host. Nested modules (a
subdirectory with its own `go.mod`) are naturally excluded, since `go list
./...` doesn't cross module boundaries.
