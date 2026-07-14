# Case study: restructuring a mixed C/Go project onto the stdlib

This walks through applying [go-stdlib.md](go-stdlib.md),
[c-stdlib.md](c-stdlib.md), and [cmake-stdlib.md](cmake-stdlib.md) to a
real-shaped project — a mixed C/Go daemon ("widgetd") with a shared
library built for Linux, a small set of Go command-line tools, and a
Windows build driven through cmake. Names and internal details are
disguised; the *shape* of the problem — and the restructuring it forces —
is real.

## The problem: source discovery without directory boundaries

```
src/
  *.c              ← 18 shared cross-platform files mixed at root
  entry.c          ← Linux .so entry point (needs excluding from the glob by name)
  lang/
    jvm/cmdline.c   ← only cross-platform file in lang/
    dotnet/, jvm/, nodejs/, php/, python/, ruby/  ← Linux-specific
  linux/            ← Linux-specific implementations
    aarch64/, x86_64/  ← arch-specific
    apps/nginx/
  windows/          ← Windows-specific implementations + its own cmake project
  test/
  include/
```

This is the shape `c_library`'s `source=DIR` option can't express: there's
no directory that's *just* the shared cross-platform code. The existing
hand-written `mmkfile` compensates with an explicit, fragile glob:

```bash
ls ./*.c ./lang/*/*.c ./linux/apps/*/*.c ./linux/*.c ./linux/$ARCH/*.c
```

plus a manual exclusion for the one file at root that's actually the Linux
`.so` entry point, not shared code. The Windows `cmake` build has the
mirror problem: 18 separate `${CMAKE_CURRENT_SOURCE_DIR}/../<file>.c`
references reaching up out of `windows/` to grab the same shared files.

Neither side has a `source=DIR` boundary to point a stdlib type at. Fixing
that is a prerequisite, not a `c.mmk` limitation — it's about the project's
own layout.

## The restructuring

```
src/
  common/          ← NEW: was src/*.c (cross-platform shared code)
    core.c, config.c, log.c, network.c, platform.c, process.c,
    strings.c, uuid.c, arena.c, ... (the 18 shared files, moved here)
    lang/jvm/cmdline.c   ← moved from lang/jvm/ (only cross-platform lang file)
  linux/           ← unchanged, plus additions
    entry.c        ← was: src/entry.c (Linux .so entry point)
    lang/          ← was: src/lang/ (Linux-specific lang implementations)
      dotnet/, jvm/, nodejs/, php/, python/, ruby/
    apps/nginx/    ← unchanged
    aarch64/       ← unchanged
    x86_64/        ← unchanged
    (Linux-specific .c files, unchanged)
  windows/         ← unchanged
  test/            ← unchanged
  include/         ← unchanged
```

Purely file moves — semantically neutral, build output identical. But now
every component maps to a `source=DIR` boundary `c_library` can use
directly, and the Windows cmake references collapse from 18 individual
`../foo.c` paths to a handful of `../common/foo.c` ones.

## The new `mmkfile`, once restructured

```bash
OUT_DIR=${OUT_DIR:-.}
ARCH=$(uname -m)
SO_FILENAME="$OUT_DIR/libwidget.so"

WIDGET_VERSION=${WIDGET_VERSION:-unknown}
INSTALL_PATH=${INSTALL_PATH:-/opt/widgetd/$WIDGET_VERSION}

# A build-time codegen step: a config file gets compiled into a generated
# header so the running binary doesn't need to parse it at startup.
CONFIG_SRC=${CONFIG_SRC:-config/policies.json}
CONFIG_HEADER=${CONFIG_HEADER:-linux/policies_generated.h}

CFLAGS="-std=c23 -fPIC -fno-omit-frame-pointer -I$(pwd)/include/ -I/usr/local/include"
LDFLAGS="-L/usr/local/lib -lwidgetpolicy -lcurl -lmjson"
C_BUILD_OPTIONS="-DINSTALL_PATH=\"$INSTALL_PATH\" -DWIDGET_VERSION=\"$WIDGET_VERSION\""

ASAN_CFLAGS="-g -O0 -fsanitize=address,undefined"
ASAN_LDFLAGS="-fsanitize=address,undefined -rdynamic"
TEST_LDFLAGS="-lpcre2-8"

# Extra dep for every .o file: the generated config header.
C_EXTRA_DEPS="$CONFIG_HEADER"

# The ASan test binary still needs a full source list (different compiler/flags,
# not going through the .o pattern rule).
TEST_SRC=$(ls test/*.c test/utils/*.c test/mocks/*.c 2>/dev/null | grep -v '_windows\.c$' | tr '\n' ' ')
COMMON_SRC=$(find common -name '*.c' | tr '\n' ' ')
LINUX_SRC=$(ls linux/*.c linux/$ARCH/*.c linux/apps/nginx/*.c 2>/dev/null | tr '\n' ' ')
LANG_SRC=$(find linux/lang -name '*.c' | tr '\n' ' ')

include c.mmk

all : build

## Generate the policy header from its source config.
file "$CONFIG_HEADER" : "$CONFIG_SRC" {
    go run ./tools/gen-policy-header "$CONFIG_SRC" "$CONFIG_HEADER"
}

# Component libraries — defbody dep clause handles .o discovery per source=.
c_library libcommon.a  source=./common :
c_library liblinux.a   source=./linux :
c_library libarch.a    source=./linux/$ARCH :
c_library liblang.a    source=./linux/lang recursive=1 :
c_library libnginx.a   source=./linux/apps/nginx :

# linux/entry.o is produced by the stdlib '(.*)\.o' pattern rule automatically.

## Link libwidget.so and patch its libc dependency for the musl build.
c_shared_lib "$SO_FILENAME" : linux/entry.o libcommon.a liblinux.a libarch.a liblang.a libnginx.a {
    mkdir -p "$OUT_DIR"
    gcc -shared -o "$SO_FILENAME" linux/entry.o \
        -Wl,--whole-archive libcommon.a liblinux.a libarch.a liblang.a libnginx.a -Wl,--no-whole-archive \
        $CFLAGS $LDFLAGS $C_BUILD_OPTIONS
    patchelf --replace-needed $(ldd "$SO_FILENAME" | grep -o 'libc\.musl-[^ ]*') libc.so.6 "$SO_FILENAME"
}
build : "$SO_FILENAME"

## Build the ASan/UBSan-instrumented test binary (custom: different compiler and flags).
file test/test_bin : "$CONFIG_HEADER" $COMMON_SRC $LINUX_SRC $LANG_SRC $TEST_SRC {
    clang -fuse-ld=lld -o test/test_bin \
        $TEST_SRC $COMMON_SRC $LINUX_SRC $LANG_SRC \
        $CFLAGS -I./test/ $ASAN_CFLAGS $LDFLAGS $ASAN_LDFLAGS $TEST_LDFLAGS $C_BUILD_OPTIONS \
        -DTESTS=1
}

[test all] : test/test_bin {
    ASAN_OPTIONS=abort_on_error=1:symbolize=1:detect_leaks=1 test/test_bin
}
```

### What stays custom, deliberately

- `CFLAGS`, `LDFLAGS`, `C_BUILD_OPTIONS` — project-specific flags.
- The generated header target and `C_EXTRA_DEPS` wiring — codegen is
  inherently project-specific.
- The `c_shared_lib` link body — `--whole-archive` and the `patchelf` musl
  fixup are specific enough to this build that a generic type shouldn't try
  to guess them.
- The ASan test binary — different compiler, different flags, a hand-built
  source list rather than going through `.o` pattern rules.

### What the stdlib eliminates

- The `ls | grep | sed | tr` source-discovery dance and the exclusion it
  required for the entry-point file.
- The `.o` pattern rule itself.
- Five hand-written `c_library`-shaped stanzas collapse to five one-line
  declarations.

### Windows cmake update

Once the move lands, `windows/CMakeLists.txt` needs:

- `${CMAKE_CURRENT_SOURCE_DIR}/../<file>.c` → `../common/<file>.c` for every
  shared file.
- The `lang/jvm/cmdline.c` reference and include path similarly repointed
  at `../common/lang`.

No other cmake changes needed — this is a mechanical consequence of the
file move, not a build-logic change.

### One thing worth verifying before moving anything

Some files exist in both `windows/` and shared/root form with disjoint
symbol sets (platform-specific implementations of the same interface, not
duplicate code) — the Windows cmake build compiles both `windows/foo.c` and
the root-level `foo.c` as separate translation units in the same binary.
Confirm this is intentional before the move, since it's exactly the kind of
thing a "just move files" refactor can silently break if it turns out to be
a latent bug rather than deliberate platform-specific dispatch. The move
itself doesn't change this behavior — it just renames `../foo.c` to
`../common/foo.c` — but it's worth eyes-on before, not after.

## Sequenced implementation plan

Applying stdlib types to an existing, working, cross-platform build is not
a place to move fast. Each step should be verified working before the next
begins — a broken build three steps in is much harder to bisect than one
broken at step N. `lib/go.mmk`, `lib/c.mmk`, and `lib/cmake.mmk` — along
with the `defbody` dep clause they depend on — already ship (see
[language-extensions.md](language-extensions.md) and the other stdlib
docs), so a migration like this one starts at Step 1 below, not from a
language or stdlib prerequisite.

### Step 1 — restructure the source tree, mmkfile untouched otherwise

File moves only (`git mv`), plus updating the *existing* hand-written glob
paths and Windows cmake references to match the new layout. The build
logic itself doesn't change yet — this step is purely "does the new layout
still produce an identical build," isolated from any stdlib-migration risk.

### Step 2 — verify both platform builds against the restructured tree

Gate before proceeding: both the Linux build and the Windows cmake build
must pass exactly as before, with no stdlib types in use yet.

### Step 3 — migrate the Linux build to stdlib types

Swap the hand-written `mmkfile` for the stdlib-based version. Requires
steps 1–2 complete. Verify build/test/fmt/clean all still pass.

### Step 4 — migrate the Windows cmake build to native mmk types

Requires steps 1–3 complete. Replace `CMakeLists.txt`/`CMakePresets.json`
with `cmake_project` (for genuinely external cmake-based dependencies) and
native mmk types for the project's own DLL and test executable where that's
viable; verify build/test/clean/install all pass before removing the old
cmake files.
