# `c.mmk` — C build helpers

**Status: shipped (`lib/c.mmk`).** C is harder than Go: there's no
built-in incremental build tool to wrap, so mmk has to actually model
source-to-object dependencies. The key design choice, worked out against a
real mixed C/Go cross-compiled project (see [case-study.md](case-study.md)):
a `source=DIR` option is the right parameter for source discovery — not the
dep list — and the `defbody` dep clause (see
[language-extensions.md](language-extensions.md#implemented-defbody-dep-clause))
is what makes that option do real work.

A well-structured C project decomposes into component libraries (each
mapping to a directory), handled by `c_library`, plus a final link step
for a shared library (`c_shared_lib`) or executable (`c_executable`).

## Source discovery helpers

`c_sources <dir>...` lists `.c` files under each directory (flat by
default; `recursive=1` walks subdirectories), sorted within each directory
for reproducible link order. `c_objects <dir>...` maps that to the
corresponding `.o` paths, honoring `recursive` the same way and prefixing
with `obj_path` if set (so generated objects can live under a build tree
instead of next to their sources):

```bash
c_sources() {
    local d
    for d in "$@"; do
        if [ -n "${recursive:-}" ]; then
            find "$d" -name '*.c' -type f | sort
        else
            find "$d" -maxdepth 1 -name '*.c' -type f | sort
        fi
    done
}

c_objects() {
    local pfx="${obj_path:+${obj_path%/}/}"
    c_sources "$@" | sed -e "s|^|$pfx|" -e 's/\.c$/.o/'
}
```

Both accept multiple space-separated directories in `$source`, so a
`c_library` can pull from more than one source tree.

## Stdlib pattern rule for `.o` files

Every C project needs this. Parameterized by passthrough variables so
projects can set their own flags without overriding the rule. To customize
(e.g. add a generated-header dep), define your own `'(.*)\.o'` pattern
rule *before* `include c.mmk` — pattern resolution is first-match-wins, so
the user's takes precedence:

```bash
file '(.*)\.o' : $1.c {
    ${CC:-gcc} -c ${CFLAGS:-} ${C_BUILD_OPTIONS:-} "$1.c" -o "$target"
}
```

## `c_library`

Produces a static archive (`.a`). The `defbody` dep clause scans
`$source` (via `c_objects`) for the `.o` files it needs; the `.o` pattern
rule above builds each one.

```bash
deftype c_library {
    [ -f "$target" ] && (stat -c "%Y" "$target" 2>/dev/null || stat -f "%m" "$target" 2>/dev/null)
}

defbody c_library : $(c_objects $source) {
    ${AR:-ar} -rcs "$target" "${dep[@]}"
}

defbody c_library clean {
    rm -f "$target" "${dep[@]}"
}
```

Options: `source=DIR` (one or more space-separated directories, required),
`recursive=1` (default: flat), `obj_path=PATH` (default: objects sit next
to their sources).

```bash
c_library libcore.a     source=./core :
c_library libplatform.a source=./platform :
c_library liblang.a     source=./lang recursive=1 :
```

The target name *is* the archive path — `$target` is `libcore.a`.

## `c_shared_lib`

Links a shared library (`.so`/`.dylib`/`.dll`) from object files and
static archives. No source discovery — deps are explicit (the component
libraries plus any entry-point `.o`).

```bash
deftype c_shared_lib {
    [ -f "$target" ] && (stat -c "%Y" "$target" 2>/dev/null || stat -f "%m" "$target" 2>/dev/null)
}

defbody c_shared_lib {
    ${CC:-gcc} -shared -o "$target" "${dep[@]}" ${ldflags:-}
}

defbody c_shared_lib clean {
    rm -f "$target"
}
```

Projects that need `--whole-archive` (Linux) or `-force_load` (macOS) to
pull in every symbol from a static archive — rather than only the ones
already referenced — need a custom body. The type still provides the
freshness check and `clean` verb for free; only the build body is
overridden:

```bash
c_shared_lib libwidget.so ldflags="$LDFLAGS" : entry.o libcore.a libplatform.a liblang.a {
    gcc -shared -o "$target" entry.o \
        -Wl,--whole-archive libcore.a libplatform.a liblang.a -Wl,--no-whole-archive \
        $CFLAGS $LDFLAGS
}
```

## `c_executable`

Builds a C executable. Sources can be discovered via `source=DIR` (same
multi-dir and `obj_path` support as `c_library`) and/or listed as explicit
deps — both can coexist, since the dep clause augments rather than
replaces explicit deps.

```bash
deftype c_executable {
    [ -f "$target" ] && (stat -c "%Y" "$target" 2>/dev/null || stat -f "%m" "$target" 2>/dev/null)
}

defbody c_executable : $([ -n "${source:-}" ] && c_objects $source) {
    ${CC:-gcc} -o "$target" "${dep[@]}" ${ldflags:-}
}

defbody c_executable clean {
    rm -f "$target"
}
```
