# cmake.mmk — cmake and git_source types for mmk.
#
# Provides:
#
#   cmake_project — wrap a cmake configure+build under a single mmk target,
#     with verb defbodies for build, install, test, and clean. The deftype
#     always reports "absent" so the body runs each time mmk is invoked;
#     cmake's own incremental tracking decides what to actually do. For
#     consumers that need a freshness signal, depend on the specific output
#     file (e.g. `<build_dir>/foo.dll`) rather than on the cmake_project
#     target itself — the file's mtime reflects whether cmake produced new
#     output.
#
#   git_source — clone (or update) a git repo to a specific tag. Freshness
#     is keyed on the requested tag: re-runs only when `tag=` changes or
#     the working tree is missing. After clone+checkout the working tree's
#     directory mtime is the freshness signal consumers see.
#
# Options:
#   cmake_project:
#     build_dir=DIR   cmake binary directory (required)
#     source=DIR      directory containing CMakeLists.txt (default: .)
#     preset=NAME     cmake preset name (optional)
#     prefix=DIR      install prefix used by [install] (default: dist)
#     ctest_args=     extra args for [test]
#
#   git_source:
#     repo=URL        git remote URL (required)
#     tag=REF         tag, branch, or commit hash to check out (required)

# ---- cmake_project -----------------------------------------------------------

## Wraps a cmake configure+build under a single mmk target. Always
## reports "absent" — cmake's own incremental tracking decides what to
## actually rebuild. Depend on a specific output file, not this target,
## if you need a freshness signal.
deftype cmake_project { return 1; }

defbody cmake_project build_dir= source= preset= {
    cmake -B "$build_dir" ${preset:+--preset "$preset"} "${source:-.}"
    cmake --build "$build_dir" -j
}

## Runs `cmake --install`.
defbody cmake_project install build_dir= prefix= {
    cmake --install "$build_dir" --prefix "${prefix:-dist}"
}

## Runs `ctest`.
defbody cmake_project test build_dir= ctest_args= {
    ctest --test-dir "$build_dir" ${ctest_args:-}
}

## Removes the cmake build directory.
defbody cmake_project clean build_dir= {
    rm -rf "$build_dir"
}

# ---- git_source --------------------------------------------------------------

## Clones (or updates) a git repo to a specific tag. Freshness is keyed
## on `tag=` — re-runs only when the tag changes or the working tree is
## missing.
deftype git_source {
    sentinel="$target/.mmk-git-tag"
    [ -f "$sentinel" ] || exit 1
    [ "$(cat "$sentinel")" = "$tag" ] || exit 1
    stat -c "%.Y" "$sentinel" 2>/dev/null || stat -f "%m" "$sentinel" 2>/dev/null
}

defbody git_source repo= tag= {
    if [ -d "$target/.git" ]; then
        git -C "$target" fetch --quiet
    else
        rm -rf "$target"
        git clone --quiet "$repo" "$target"
    fi
    git -C "$target" checkout --quiet "$tag"
    printf '%s' "$tag" > "$target/.mmk-git-tag"
}

## Removes the cloned working tree.
defbody git_source clean {
    rm -rf "$target"
}
