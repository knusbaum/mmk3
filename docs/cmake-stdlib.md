# `cmake.mmk` — cmake and external-source helpers

**Status: shipped (`lib/cmake.mmk`).** cmake is a dependency primitive,
not just a legacy wrapper. Even a native-mmk build needs `cmake_project`
for external dependencies that ship their own `CMakeLists.txt` (curl,
PCRE2, and similar). It's also the pragmatic answer for platforms where
the rest of a project's build is native mmk (say, Linux via `c.mmk`) but
one platform's toolchain is far more naturally driven through cmake (say,
Windows via MSVC) — rather than forcing that platform onto types it
doesn't fit.

## `cmake_project`

```bash
deftype cmake_project { return 1; }

defbody cmake_project {
    cmake -B "$build_dir" ${preset:+--preset "$preset"} "${source:-.}"
    cmake --build "$build_dir" -j
}

defbody cmake_project install {
    cmake --install "$build_dir" --prefix "${prefix:-dist}"
}

defbody cmake_project test {
    ctest --test-dir "$build_dir" ${ctest_args:-}
}

defbody cmake_project clean {
    rm -rf "$build_dir"
}
```

The `deftype` always reports "absent" so the body runs on every
invocation — cmake's own incremental tracking decides what actually
rebuilds. Consumers that need a real freshness signal should depend on the
specific output file (e.g. `<build_dir>/foo.dll`) rather than on the
`cmake_project` target itself, since the target's own freshness check is
unconditional.

Options: `build_dir=` (required), `source=.` (default: current dir),
`preset=` (optional; omit for projects without `CMakePresets.json`),
`prefix=dist` (install prefix for `[install]`), `ctest_args=`.

```bash
cmake_project windows on $WIN_IMAGE build_dir=$WIN_BUILD_DIR preset=$CMAKE_PRESET source=src/windows
[test windows]  on $WIN_IMAGE : windows { wine64 "$WIN_BUILD_DIR/widget-tests.exe"; }
[shell windows] on $WIN_IMAGE { bash; }
```

`[install windows]` and `[clean windows]` come from the type's defbodies
for free. The `wine64` test invocation stays custom — it's not a `ctest`
suite.

## `git_source`

For external dependencies that need to be fetched at build time. Clones a
repo at a specific tag; freshness is keyed on the requested tag — it
re-runs only when `tag=` changes or the working tree is missing, checked
via a sentinel file (`.mmk-git-tag`) rather than trusting the repo's own
state, since `git`'s notion of "up to date" doesn't track "checked out at
the tag mmk was asked for."

```bash
deftype git_source {
    sentinel="$target/.mmk-git-tag"
    [ -f "$sentinel" ] || exit 1
    [ "$(cat "$sentinel")" = "$tag" ] || exit 1
    stat -c "%Y" "$sentinel" 2>/dev/null || stat -f "%m" "$sentinel" 2>/dev/null
}

defbody git_source {
    if [ -d "$target/.git" ]; then
        git -C "$target" fetch --quiet
    else
        rm -rf "$target"
        git clone --quiet "$repo" "$target"
    fi
    git -C "$target" checkout --quiet "$tag"
    printf '%s' "$tag" > "$target/.mmk-git-tag"
}

defbody git_source clean {
    rm -rf "$target"
}
```

Options: `repo=` (required), `tag=` — tag, branch, or commit hash
(required).

```bash
git_source build/deps/mjson repo=https://github.com/cesanta/mjson.git tag=032a2ea

file build/deps/mjson/mjson.o : build/deps/mjson/src/mjson.c { ... }
```

For cmake-based dependencies, `git_source` feeds into `cmake_project`:

```bash
git_source build/deps/curl-src  repo=https://github.com/curl/curl.git       tag=curl-8_5_0
git_source build/deps/pcre2-src repo=https://github.com/PCRE2Project/pcre2.git tag=pcre2-10.42

cmake_project build/deps/curl source=build/deps/curl-src build_dir=build/deps/curl-build : build/deps/curl-src {
    cmake -B "$build_dir" "$source" -DBUILD_CURL_EXE=OFF -DCURL_STATICLIB=ON ...
    cmake --build "$build_dir" -j
}
```

Note: fetching at build time means network access during the build. The
alternative is baking dependencies into a container image the build runs
inside (via mmk's `image` runner type) — trading network-at-build-time for
image-rebuild-on-dependency-bump. Either is valid; `git_source` is the
right primitive when in-image dependencies aren't feasible (e.g. a platform
whose build doesn't run in a container at all, like native Windows).
