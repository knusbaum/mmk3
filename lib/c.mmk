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
#   - c_library — produces a static archive (.a). Sources are discovered
#     by scanning the `source=DIR` option (flat by default; pass
#     `recursive=1` for nested directories). `source` may list multiple
#     space-separated directories. Set `obj_path=PATH` to place the
#     generated `.o` deps under a build tree instead of next to their
#     sources. Each .c → .o build is handled by the .o pattern rule
#     above.
#
#   - c_shared_lib — links a .so/.dylib/.dll. No source discovery; the
#     user lists explicit deps (object files and/or static archives).
#     The default body invokes ${CC} -shared with $ldflags. Projects
#     needing --whole-archive, patchelf, or other custom linker logic
#     should override the body inline.
#
#   - c_executable — like c_shared_lib but for an executable. Sources
#     can be discovered via `source=DIR` (same as c_library, with the
#     same multi-dir and obj_path support) OR listed as explicit deps;
#     both can coexist (augmented).
#
# Variables read from passthrough scope:
#   CC                  C compiler (default: gcc)
#   AR                  archiver  (default: ar)
#   CFLAGS              flags passed to compile
#   C_BUILD_OPTIONS     additional defines / compile-time options
#
# Per-target options read from rule headers:
#   source              one or more space-separated directories to scan for .c files
#   recursive           non-empty ⇒ recursive scan; empty ⇒ flat (only top-level *.c)
#   obj_path            optional build-tree prefix for the generated .o paths

# ---- Source discovery helpers ------------------------------------------------

# c_sources <dir>... — list .c files under each directory, in order. Flat by
# default (only top-level *.c); set recursive=1 in the call's environment to
# walk subdirs too. Output is sorted within each directory so builds are
# reproducible (some link-order behavior, e.g. weak-symbol resolution under
# the AddressSanitizer interceptor, depends on it). Multiple directories may
# be passed as separate arguments OR as a single space-separated argument
# (callers typically pass `$source` unquoted for the latter).
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

# c_objects <dir>... — list the .o paths corresponding to .c sources under
# each directory. Honors `recursive` like c_sources. When `obj_path` is set,
# each generated path is prefixed with `${obj_path}/` (any trailing slash on
# the option value is stripped). With obj_path unset, paths sit next to the
# source — the c.mmk default and what the built-in '.o' pattern rule expects.
#
# Example:
#   obj_path=build c_objects src libddtracer
#     → build/src/tracer.o build/libddtracer/agentless.o ...
c_objects() {
    local pfx="${obj_path:+${obj_path%/}/}"
    c_sources "$@" | sed -e "s|^|$pfx|" -e 's/\.c$/.o/'
}

# ---- .o pattern rule ---------------------------------------------------------

file '(.*)\.o' : $1.c {
    ${CC:-gcc} -c ${CFLAGS:-} ${C_BUILD_OPTIONS:-} "$1.c" -o "$target"
}

# ---- c_library ---------------------------------------------------------------

deftype c_library {
    [ -f "$target" ] && (stat -c "%.Y" "$target" 2>/dev/null || stat -f "%m" "$target" 2>/dev/null)
}

defbody c_library : $(c_objects $source) {
    ${AR:-ar} -rcs "$target" "${dep[@]}"
}

defbody c_library clean {
    rm -f "$target" "${dep[@]}"
}

# ---- c_shared_lib ------------------------------------------------------------

deftype c_shared_lib {
    [ -f "$target" ] && (stat -c "%.Y" "$target" 2>/dev/null || stat -f "%m" "$target" 2>/dev/null)
}

defbody c_shared_lib {
    ${CC:-gcc} -shared -o "$target" "${dep[@]}" ${ldflags:-}
}

defbody c_shared_lib clean {
    rm -f "$target"
}

# ---- c_executable ------------------------------------------------------------

deftype c_executable {
    [ -f "$target" ] && (stat -c "%.Y" "$target" 2>/dev/null || stat -f "%m" "$target" 2>/dev/null)
}

defbody c_executable : $([ -n "${source:-}" ] && c_objects $source) {
    ${CC:-gcc} -o "$target" "${dep[@]}" ${ldflags:-}
}

defbody c_executable clean {
    rm -f "$target"
}
