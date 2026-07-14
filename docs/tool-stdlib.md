# `tool` — pinned local CLI dependencies

**Status: shipped (`lib/tool.mmk`).** Not language-specific — sits
alongside `go.mmk`, `c.mmk`, etc. as its own small stdlib file, since the
pattern it captures shows up in projects of every language: "this build
needs some versioned CLI on `PATH`; install it if it's missing."

## The pattern this replaces

Surveying Makefiles across a range of Go projects turned up the same
hand-written shape repeatedly: a dozen near-identical Make stanzas, each
installing one pinned tool version into a local `bin/` directory, keyed off
file existence so re-runs are no-ops (`controller-gen`, `kustomize`,
`golangci-lint`, `yq`, `jq`, `kind`, and so on — the exact tool varies, the
shape doesn't).

## Design

```bash
group tools

deftype tool into tools {
    p=$(which "$target" 2>/dev/null) || exit 1
    stat -c "%Y" "$p" 2>/dev/null || stat -f "%m" "$p" 2>/dev/null
}

defbody tool {
    echo "tool '$target' has no body — add one that installs it, e.g.:" >&2
    echo "  tool $target { go install <pkg>@<version> }" >&2
    exit 1
}
```

Freshness is "does `which $target` resolve to something, and what's that
binary's mtime" — deliberately just `which`, not a specific expected
install path, so the type doesn't care *how* the tool got onto `PATH` (a
local `GOBIN`, a project-local `.bin` someone's prepended to `PATH`, a
system package manager, whatever).

There's intentionally no default build body. Unlike `go_exe` or
`c_library`, "install this tool" has no sensible generic implementation —
the install command is inherently tool-specific. `defbody tool` exists only
to fail loudly with a hint, rather than silently doing nothing, if someone
declares a `tool` target with no body.

The `deftype tool into tools` clause (see
[language-extensions.md](language-extensions.md#implemented-deftype-into-group-automatic-group-membership))
registers every `tool` target into a `tools` group automatically, with no
per-target `into` clause needed — so a project can depend on "every
declared tool" without enumerating them:

```bash
setup : tools
```

## Usage

```bash
tool controller-gen {
    go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.14.0
}

tool kind {
    brew install kind
}

file config/crd : controller-gen {
    controller-gen crd paths=./... output:crd:artifacts:config=config/crd
}
```

Each `tool` target is just a normal concrete target of type `tool` — anyone
depending on it (`: controller-gen`) gets "ensure this is installed and
up to date" as an ordinary DAG edge, with no special-casing needed elsewhere.
