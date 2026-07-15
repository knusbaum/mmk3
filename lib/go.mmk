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
#   go_exe — a Go binary. The target name is the output path. Freshness is
#     the binary's mtime (mmk treats it like a file). mmk does not track
#     individual .go source files, so to rebuild after source changes use
#     `mmk clean <target>` or remove the binary.
#
# Options (both types accept):
#   pkg=<path>      Go package(s) to operate on. Defaults to ./... for
#                   go_module verbs and to . for go_exe build.
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
# registered into the go_mains group. A hand-written go_exe rule for the same
# target name simply overrides the generated one. If there's no go.mod (or no
# `go` binary), this degrades to an empty go_mains group rather than erroring
# — go_module/go_exe keep working regardless.
_mmk_go_mains() {
    modpath=$(go list -m -f '{{.Path}}' 2>/dev/null)
    frag="${TMPDIR:-/tmp}/mmk-go-mains-$(pwd -P | cksum | cut -d' ' -f1).mmk"
    printf '## Every auto-discovered main package.\ngroup go_mains\n\n' > "$frag"
    if [ -n "$modpath" ]; then
        go list -e -f '{{if eq .Name "main"}}{{.ImportPath}}{{end}}' ./... 2>/dev/null |
        while IFS= read -r importpath; do
            [ -z "$importpath" ] && continue
            if [ "$importpath" = "$modpath" ]; then
                rel="${modpath##*/}"
            else
                rel="${importpath#$modpath/}"
            fi
            printf '## Auto-discovered main package: %s\ngo_exe bin/%s pkg=%s into go_mains :\n' "$importpath" "$rel" "$importpath" >> "$frag"
        done
    fi
    echo "$frag"
}

include $(_mmk_go_mains)

# ---- go_module ---------------------------------------------------------------

deftype go_module { return 1; }

defbody go_module {
    go build ${pkg:-./...}
}

defbody go_module test {
    go test ${pkg:-./...} -cover
}

defbody go_module fmt {
    go fmt ${pkg:-./...}
}

defbody go_module fmt-check {
    files=$(gofmt -l .)
    if [ -n "$files" ]; then
        echo "Format check failed:"
        echo "$files"
        exit 1
    fi
}

defbody go_module vet {
    go vet ${pkg:-./...}
}

defbody go_module clean {
    go clean
}

defbody go_module update {
    go get -u ${pkg:-./...}
    go mod tidy
}

# ---- go_exe ------------------------------------------------------------------

deftype go_exe {
    [ -f "$target" ] && (stat -c "%.Y" "$target" 2>/dev/null || stat -f "%m" "$target" 2>/dev/null)
}

defbody go_exe : pre_build {
    mkdir -p "$(dirname "$target")"
    CGO_ENABLED=${cgo:-0} GOOS=${goos:-} GOARCH=${goarch:-} \
        go build ${ldflags:+-ldflags="$ldflags"} -o "$target" "${pkg:-.}"
}

defbody go_exe clean {
    rm -f "$target"
}

defbody go_exe test {
    go test "${pkg:-.}"
}
