# c.mmk — C build helpers for mmk.
#
# Provides:
#
#   - A default '(.*)\.o' pattern rule that compiles a .c to a .o using
#     the project's $CFLAGS and $C_BUILD_OPTIONS. To customize (e.g. add
#     a generated header dep), define your own '(.*)\.o' pattern rule
#     BEFORE `include c.mmk` — pattern resolution is first-match-wins,
#     so the user's takes precedence.
#
#   - Pattern verbs '[fmt <file>.c]' / '[fmt-check <file>.c]' wrapping
#     clang-format.
#
#   - c_library — produces a static archive (.a). Sources are discovered
#     by scanning the `source=DIR` option (flat by default; pass
#     `recursive=1` for nested directories). Each .c → .o build is
#     handled by the .o pattern rule above.
#
#   - c_shared_lib — links a .so/.dylib/.dll. No source discovery; the
#     user lists explicit deps (object files and/or static archives).
#     The default body invokes ${CC} -shared with $ldflags. Projects
#     needing --whole-archive, patchelf, or other custom linker logic
#     should override the body inline.
#
#   - c_executable — like c_shared_lib but for an executable. Sources
#     can be discovered via `source=DIR` (same as c_library) OR listed
#     as explicit deps; both can coexist (augmented).
#
# Variables read from passthrough scope:
#   CC                  C compiler (default: gcc)
#   AR                  archiver  (default: ar)
#   CFLAGS              flags passed to compile
#   C_BUILD_OPTIONS     additional defines / compile-time options

# ---- Source discovery helper -------------------------------------------------

# Used by c_library / c_executable defbody dep clauses to enumerate .c
# files under $source. recursive=1 triggers a deep find; otherwise we only
# pick up *.c at the top of $source.
__c_find_sources() {
    if [ -n "${recursive:-}" ]; then
        find "$1" -name '*.c' -type f
    else
        find "$1" -maxdepth 1 -name '*.c' -type f
    fi
}

# ---- .o pattern rule ---------------------------------------------------------

file '(.*)\.o' : $1.c {
    ${CC:-gcc} -c ${CFLAGS:-} ${C_BUILD_OPTIONS:-} "$1.c" -o "$target"
}

# ---- clang-format pattern verbs ----------------------------------------------

[fmt '(.*\.[ch])']       { clang-format -i "$target"; }
[fmt-check '(.*\.[ch])'] { clang-format --dry-run --Werror "$target"; }

# ---- c_library ---------------------------------------------------------------

deftype c_library {
    [ -f "$target" ] && (stat -c "%Y" "$target" 2>/dev/null || stat -f "%m" "$target" 2>/dev/null)
}

defbody c_library : $(__c_find_sources "$source" | sed 's/\.c$/.o/') {
    ${AR:-ar} -rcs "$target" "${dep[@]}"
}

defbody c_library clean {
    rm -f "$target" "${dep[@]}"
}

# ---- c_shared_lib ------------------------------------------------------------

deftype c_shared_lib {
    [ -f "$target" ] && (stat -c "%Y" "$target" 2>/dev/null || stat -f "%m" "$target" 2>/dev/null)
}

defbody c_shared_lib {
    ${CC:-gcc} -shared -o "$target" "${dep[@]}" ${ldflags:-}
}

defbody c_shared_lib clean {
    rm -f "$target"
}

# ---- c_executable ------------------------------------------------------------

deftype c_executable {
    [ -f "$target" ] && (stat -c "%Y" "$target" 2>/dev/null || stat -f "%m" "$target" 2>/dev/null)
}

defbody c_executable : $([ -n "${source:-}" ] && __c_find_sources "$source" | sed 's/\.c$/.o/') {
    ${CC:-gcc} -o "$target" "${dep[@]}" ${ldflags:-}
}

defbody c_executable clean {
    rm -f "$target"
}
