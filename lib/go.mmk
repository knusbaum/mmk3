# go.mmk — Go build helpers for mmk.
#
# Provides two types:
#
#   go_module — a phony target wrapping the standard go tool verbs (build,
#     test, fmt, fmt-check, vet, clean, update). The go toolchain manages
#     its own incremental cache, so the mmk freshness check always reports
#     "needs run"; bodies always invoke the tool and it does the right
#     thing. Useful as a single declaration that gives a project the full
#     verb set out of the box.
#
#   go_exe — a Go binary. The target name is the output path. Like
#     go_module, the mmk freshness check always reports "needs run"; the
#     body always invokes `go build`, and the go tool's own incremental
#     cache makes rebuilding an up-to-date binary cheap.
#
# Options (both types accept):
#   pkg=<path>      Go package(s) to operate on. Defaults to ./... for
#                   go_module verbs. Mandatory for go_exe (build and test) —
#                   there is no default, since a target name alone (e.g.
#                   bin/cmd/server) doesn't reliably imply which package to
#                   build; omitting it is a build-time error, not a silent
#                   fallback to ".".
#   ldflags=<str>   Value passed to -ldflags on go build (go_exe).
#   cgo=0           CGO_ENABLED for the go_exe build (default: 0).
#   goos=<os>       GOOS for the go_exe build (default: unset, i.e. host OS).
#   goarch=<arch>   GOARCH for the go_exe build (default: unset, i.e. host
#                   arch). Meant to be driven by a `for goos in [...] for
#                   goarch in [...]` matrix on the target, not set by hand.
#
# Every go_exe build unconditionally depends on the pre_build group, whether
# or not anything is registered into it. This gives a project a hook point
# for codegen (or any other pre-build step) without editing generated lines:
#
#   generate into pre_build : my_generate_tool {
#       my_generate_tool ...
#   }
#
#   tool my_generate_tool {
#       go install example.com/some/generator@v1.2.3
#   }
group pre_build

# ---- main-package discovery ---------------------------------------------------
#
# Every main package under the current directory's module gets a go_exe
# target for free, named bin/<path-relative-to-module-root> (or
# bin/<module-basename> for a main package at the module root itself), and
# registered into the go_exes group. A hand-written go_exe rule for the same
# target name simply overrides the generated one. If there's no go.mod (or no
# `go` binary), this degrades to an empty go_exes group rather than erroring
# — go_module/go_exe keep working regardless.
_mmk_go_exes() {
    modpath=$(go list -m -f '{{.Path}}' 2>/dev/null)
    frag="${TMPDIR:-/tmp}/mmk-go-mains-$(pwd -P | cksum | cut -d' ' -f1).mmk"
    printf '## Every auto-discovered main package.\ngroup go_exes\n\n' > "$frag"
    if [ -n "$modpath" ]; then
        go list -e -f '{{if eq .Name "main"}}{{.ImportPath}}{{end}}' ./... 2>/dev/null |
        while IFS= read -r importpath; do
            [ -z "$importpath" ] && continue
            if [ "$importpath" = "$modpath" ]; then
                rel="${modpath##*/}"
            else
                rel="${importpath#$modpath/}"
            fi
            printf '## Auto-discovered main package: %s\ngo_exe bin/%s pkg=%s into go_exes :\n' "$importpath" "$rel" "$importpath" >> "$frag"
        done
    fi
    echo "$frag"
}

include $(_mmk_go_exes)

# ---- go_module ---------------------------------------------------------------

## A phony target wrapping the standard go tool verbs (build, test, fmt,
## fmt-check, vet, clean, update). Always reports "needs run" — the go
## toolchain manages its own incremental cache.
deftype go_module { return 1; }

defbody go_module pkg= {
    go build ${pkg:-./...}
}

## Runs `go test` with coverage.
defbody go_module test pkg= {
    go test ${pkg:-./...} -cover
}

## Runs `go fmt`.
defbody go_module fmt pkg= {
    go fmt ${pkg:-./...}
}

## Fails if any file is not gofmt-formatted, without modifying anything.
defbody go_module fmt-check {
    files=$(gofmt -l .)
    if [ -n "$files" ]; then
        echo "Format check failed:"
        echo "$files"
        exit 1
    fi
}

## Runs `go vet`.
defbody go_module vet pkg= {
    go vet ${pkg:-./...}
}

## Runs `go clean`.
defbody go_module clean {
    go clean
}

## Runs `go get -u` followed by `go mod tidy`.
defbody go_module update pkg= {
    go get -u ${pkg:-./...}
    go mod tidy
}

# ---- go_exe ------------------------------------------------------------------

## A Go binary. The target name is the output path. Always reports
## "needs run" — mmk defers to the go tool's own incremental build
## cache, which makes a rebuild of an already-built binary cheap.
deftype go_exe { return 1; }

defbody go_exe pkg= cgo=0 goos= goarch= ldflags= : pre_build {
    if [ -z "$pkg" ]; then
        echo "go_exe $target: pkg= is required (e.g. pkg=./cmd/myapp)" >&2
        exit 1
    fi
    mkdir -p "$(dirname "$target")"
    CGO_ENABLED=${cgo:-0} GOOS=${goos:-} GOARCH=${goarch:-} \
        go build ${ldflags:+-ldflags="$ldflags"} -o "$target" "$pkg"
}

## Removes the built binary.
defbody go_exe clean {
    rm -f "$target"
}

## Runs `go test` on the exe's package.
defbody go_exe test pkg= {
    if [ -z "$pkg" ]; then
        echo "go_exe $target: pkg= is required (e.g. pkg=./cmd/myapp)" >&2
        exit 1
    fi
    go test "$pkg"
}
