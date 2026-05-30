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

defbody go_exe {
    mkdir -p "$(dirname "$target")"
    CGO_ENABLED=${cgo:-0} go build ${ldflags:+-ldflags="$ldflags"} -o "$target" "${pkg:-.}"
}

defbody go_exe clean {
    rm -f "$target"
}

defbody go_exe test {
    go test "${pkg:-.}"
}
