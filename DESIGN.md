# mmk3 Design

## Motivation

mmk and mmk2 are make-like build tools. mmk2 partially generalized the
underlying DAG executor (`Node[T, U]` in `graph.go`) but the executor still
carries build-system concepts (`Modtime`, `TargetRule`, threaded `Build`
context).

mmk3 has two goals:

1. **Extract a truly general-purpose DAG executor library.** It should know
   nothing about files, modtimes, shells, or builds — only nodes, dependencies,
   and execution.

2. **Build a make-like tool on top of it using a new bash-derived DSL.** The
   DSL is annotated bash: regular shell function bodies decorated with type,
   runner, and dependency annotations. The tool transforms the DSL back into
   plain bash for execution.

These two pieces are decoupled: the library can host other frontends (Go API,
JSON definitions, etc.); the bash frontend uses the library but is not bound
to it.

---

## Part 1: The Core Library

### The `Node` Interface

```go
type Node[T any] interface {
    Dependencies() []T
    NeedsRun() bool
    Run() error
}
```

- `Dependencies()` returns the upstream nodes this one needs.
- `NeedsRun()` is the node's *local* check: based purely on the node's own
  state, does it need to run? Returns `true` if execution is needed (stale
  or phony), `false` if already up-to-date.
- `Run()` performs the work.

### What was dropped from mmk2's interface, and why

- **`Modtime`** — replaced by `NeedsRun()`. The library no longer prescribes
  *how* a node decides whether to run. Modtime comparison is now a strategy
  callers can implement inside their own `NeedsRun()`.

- **`TargetRule(U) string`** — display/naming concern, not core. The library
  can use a `fmt.Stringer` constraint or a separate `Namer` interface if
  pretty-printing is needed.

- **The `U` type parameter (shared context)** — nodes carry the state they
  need by closing over it at construction time. No need to thread runtime
  context through every method call.

The `T` type parameter remains so `Dependencies()` can return concretely
typed nodes rather than `[]any`.

### `NeedsRun()` is fully owned by the node

The executor does not cascade. Each node decides for itself whether to run,
based on whatever state it cares about:

> A node runs iff its own `NeedsRun()` returns true.

This keeps the executor truly general-purpose. It does not assume that
"upstream ran → I'm stale" — that may be true for file-mtime artifacts but
is wrong for setup nodes (e.g., a node that brings up a container/service
once for the build), where upstream changes are irrelevant to whether the
setup work needs to repeat.

For artifact-style nodes that *do* want freshness propagation (file →
file), the node implements that comparison inside its own `NeedsRun()` —
e.g., "any of my deps have a `Date()` newer than mine."

Concretely, the executor's per-node logic is:

1. Wait for all upstream `Step`s to complete.
2. If any failed → propagate failure, do not run.
3. If `NeedsRun() == true` → call `Run()`. Otherwise skip.

### The Executor

Carry over the working pieces of mmk2's `graph.go`:

- `Step[T]` — per-node execution wrapper with `done` channel, status, and
  upstream waiting.
- `Semaphore` — bounded parallelism.
- A traversal function `Execute[T Node[T]](root T, parallelism int) error`
  that:
  1. Walks the graph from `root`, deduplicating nodes (one `Step` per node).
  2. Detects cycles during traversal.
  3. Wires up upstream/downstream relationships.
  4. Spawns a goroutine per `Step`.
  5. Returns when the root completes (or fails).

### What the library does *not* include

- Anything bash-related.
- Anything file-related.
- Display/logging beyond what's needed for the executor itself (consider an
  optional event-stream interface for callers that want progress reporting).
- DSL parsing or any frontend.

### Package layout

```
mmk3/
  dag/              — the core library (Node, Step, Executor, Semaphore)
    dag.go
    dag_test.go
  cmd/mmk/          — the mmk CLI and everything specific to it
    main.go
    parse/          — DSL parser
    gen/            — bash code generator
    runtime/        — node/runner/type implementations that bridge DSL → dag
```

Anything at the repo root (currently just `dag/`) is part of the
general-purpose library and could be imported by other frontends. Anything
under `cmd/mmk/` is specific to the mmk tool — parser, generator, and
runtime bridge are all consumers of `dag/`, not part of it.

---

## Part 2: The Frontend DSL

### Goals

- Familiar to shell users — looks like bash with annotations.
- Preprocessable into plain bash so each task body is invoked as a regular
  bash function in its own process.
- User-extensible via `deftype` (NeedsRun strategies) and `defbody`
  (default build and verb behaviors).

### Grammar

```
<type>? <target> ('on' <image-target>)? (':' <deps...>)? {
    <body>
}

[<verb> <target>] ('on' <image-target>)? (':' <deps...>)? {
    <body>
}

deftype <name> {
    <bash — prints timestamp to stdout; non-zero exit = artifact absent>
}

defbody <type> {
    <bash — default build body for targets of this type>
}

defbody <type> <verb> {
    <bash — default body for 'verb' on targets of this type>
}
```

Examples:

```bash
# No type, no deps — always runs.
clean {
    rm -f *.o myprogram
}

# No type, with deps — always runs after deps.
all : myprogram

# File type, with deps — NeedsRun() defined by 'file' type.
file main.o : main.c lib.h {
    cc -c main.c -o main.o
}

# Pattern rule — '$1' is the capture group from the regex.
file '(.*)\.o' : $1.c {
    cc -c $1.c -o $target
}

# Verb rule — 'clean main.o' deletes the file.
[clean main.o] {
    rm -f main.o
}

# File type inside a container from an image target.
image myimage:latest : Dockerfile
file main.o on myimage:latest : main.c lib.h {
    cc -c main.c -o main.o
}
```

### Verbs

A *verb* qualifies a target name and gives it an alternate behavior. The
default verb is the empty string (the standard build). Named verbs are
invoked with `mmk <verb> <target>` on the CLI, or referenced in dep lists
as `[verb deptarget]`.

```bash
[clean all] : [clean main.o] [clean lib.o] {
    rm -f output
}
[clean main.o] {
    rm -f main.o
}
```

`mmk clean all` runs the clean tree rooted at `[clean all]`.

Verb rules on a target inherit the target's dep list by default. Use
`[clean foo] : {` (colon with empty list) to opt out of inheritance and
have an explicit empty dep list. Use `[clean foo] : dep1 dep2 {` to
override with a different list.

### Type and body definitions

```bash
# A deftype body prints the artifact's timestamp to stdout (epoch seconds,
# "epoch.nanos" as emitted by GNU `stat -c %.Y`, or RFC3339). Non-zero exit
# means the artifact doesn't exist yet.
# $target and $deps are available.
deftype file {
    stat -c %Y "$target" 2>/dev/null || return 1
}

deftype myimage {
    docker inspect --format '{{.Created}}' "$target" 2>/dev/null || return 1
}

# A defbody provides the default Run body for targets of a given type.
# Used when a typed target has no explicit body.
defbody file {
    [[ -e "$target" ]] && return 0
    printf 'mmk: %s does not exist and has no rule to create it\n' "$target" >&2; return 1
}

# A defbody with a verb provides the default body for 'verb' on all
# targets of the given type.
defbody file clean {
    rm -f "$target"
}
```

### Runners

Runner support is type-driven rather than user-defined. The `on <target>`
clause names an existing concrete target; the runner strategy is determined
by that target's type. Currently the only supported runner type is `image`.

```bash
image myimage:latest : Dockerfile

# This target's body runs inside a container of myimage:latest.
file prog on myimage:latest : main.c {
    gcc -o prog main.c
}
```

When `on` is used, mmk:
1. Adds the named image target and a synthetic container-startup node as
   implicit deps (so the image is built and the container is running before
   the target executes).
2. Exec's the body into the running container via `docker exec`.
3. Cleans up the container when the build finishes.

There is no `defrunner` keyword; runner behavior is not user-extensible in
the current version. New runner strategies require adding a case in the
runtime's `runOn` dispatch.

### Built-in types

Three types ship with mmk and need not be declared in the mmkfile:

| Type     | NeedsRun strategy                    | Default build body                         |
|----------|--------------------------------------|--------------------------------------------|
| `file`   | `stat -c %Y "$target"` (mtime)       | error if absent (no rule to create it)     |
| `source` | `stat -c %Y "$target"` (mtime)       | error if absent (no rule to create it)     |
| `image`  | `docker inspect --format {{.Created}}` | `docker build -t "$target" -f "${deps%% *}" .` |

**`source` vs `file`**: Both use mtime-based freshness. The difference is
that `file` gets a built-in `clean` verb (the default `defbody file clean`
runs `rm -f "$target"`). `source` does not — cleaning source files would
destroy them. Any dependency not explicitly declared as `file` is inferred
as `source` by the runtime.

Override any built-in with your own `deftype` / `defbody`:

```bash
# Override stat for macOS
deftype file {
    stat -f %m "$target" 2>/dev/null || return 1
}
```

Run `mmk -builtins` to print all built-in definitions as mmk syntax.

### Defaults

- **No type specified** → always-run. `NeedsRun()` returns `true`. No
  freshness check; the body runs unconditionally (after deps succeed).
- **No runner specified** → run locally in bash, no wrapper.
- **No body, typed target** → `defbody` for that type is used as the body.
  For `file` and `source`, the default body errors if the file is absent.
- **No body, untyped target** → no-op body (`:`) — useful for dep-only
  aggregators.

### Variable expansion in dep lists

Dep lists may contain bare shell variable references (`$VARIABLE` or
`${VARIABLE}`). These are expanded by sourcing the generated script in a
bash subprocess and echoing the token. Word-splitting applies, so a
variable holding a space-separated list of names expands to multiple deps.

This is the same mechanism as passthrough bash — variable assignments in
the mmkfile are emitted verbatim into the generated script, and dep
expansion sources that script to evaluate them.

```bash
C_OBJ=$(ls *.c | sed 's/\.c$/.o/')

file myprogram : $C_OBJ {
    cc -o myprogram $C_OBJ
}
```

Only simple `$VARIABLE` tokens at the start of a dep are expanded. Literal
dep names that do not start with `$` are used as-is.

### Passthrough bash

Any line that is not an mmk directive is passed through verbatim to the
generated script. This includes variable assignments, function definitions,
and any other valid bash.

```bash
CC=gcc
CFLAGS='-O2 -Wall'
LDFLAGS="$CFLAGS -lm"

file prog.o : prog.c {
    $CC $CFLAGS -c prog.c -o prog.o
}
```

Multi-line bash strings (using single or double quotes spanning multiple
lines) are also passed through correctly. The parser detects open-quoted
strings and treats subsequent lines as continuation until the quote closes.

### Body environment

Every target body has these env vars set when invoked:

- `$target` — the target name as written in the source (unmangled).
- `$deps` — space-separated string of dependency names.

Same for `deftype` and `defbody` bodies.

Pattern bodies additionally receive `$1`, `$2`, ... for regex capture
groups, available both in the body and in the dep list.

### Function name validation

Target names are validated against bash's own function-name character rules.
Characters that bash disallows in function names (`$()<>\`"'\\ \t\n[=`) are
rejected at generation time with a clear error. Everything else (`.`, `:`,
`-`, `/`, etc.) is allowed. Users never see the internal function names;
they appear only in `generated.sh`.

---

## Part 3: Execution Model

### Compile-time pipeline

1. **Parse** the annotated source file → AST of:
   - Target blocks (type, name, runner, deps, body)
   - `deftype` blocks (name, body)
   - `defbody` blocks (type, optional verb, body)
   - Passthrough lines (verbatim bash — variable assignments, loops, etc.)
2. **Validate**:
   - Referenced types exist.
   - `on` targets exist and are of type `image`.
   - No cycles in the dep graph (also caught at exec time, but earlier is
     better).
   - Target names don't collide.
3. **Generate** a temp `generated.sh` containing:
   - Built-in deftype / defbody / verb-body functions (suppressed if
     overridden by the user).
   - Each user `deftype` body as a bash function.
   - Each user `defbody` body as a bash function.
   - Each concrete target body as a bash function.
   - Passthrough lines verbatim.
   - Pattern-instantiated targets are appended on demand during DAG
     construction.

   The generated script is written to disk **before** DAG resolution begins.
   This is required for variable expansion in dep lists: dep resolution is
   lazy (happens during DAG walk), and sourcing `generated.sh` at that point
   gives access to all passthrough variable assignments.

4. **Build the DAG** of `dag.Node` instances (one per target). Each node
   closes over: target name, deps, type name (or empty), runner name (or
   empty), and the path to `generated.sh`.

### Runtime pipeline (per node)

`NeedsRun()` for a node with type `T`:

```
MMK_GENFILE=<path> MMK_TARGET=<name> MMK_DEPS="<dep1 dep2 ...>" bash -c '
    . "$MMK_GENFILE"
    target="$MMK_TARGET"; deps="$MMK_DEPS"
    __mmk_type_<T>
'
# Exit code: 0 = up-to-date (NeedsRun=false), non-zero = needs build (NeedsRun=true).
```

Nodes without a type always return `NeedsRun() = true`.

`Run()` for a node:

```
MMK_GENFILE=<path> MMK_TARGET=<name> MMK_DEPS="<dep1 dep2 ...>" bash -c '
    . "$MMK_GENFILE"
    target="$MMK_TARGET"; deps="$MMK_DEPS"
    __mmk_target_<name>
'
```

If a runner `R` is set, `__mmk_runner_<R>` is prepended to the call so the
runner body receives the target function as `$@`.

### Variable expansion at dep resolution time

When a dep string starts with `$`, the runtime expands it by running:

```
bash -c '. "$MMK_GENFILE"; echo $VARIABLE'
```

Word-splitting on the output gives the concrete dep names. This sources the
already-generated script, so all passthrough variable definitions are
available.

### Inferred deps

If a dep name is not declared in the mmkfile and no pattern rule matches it,
the runtime infers a `source` node for it. Inferred `source` nodes:

- Use `deftype source` (mtime-based freshness).
- Use `defbody source` (error if absent).
- Have **no** clean verb — source files are not deleted by `mmk clean`.

This means users never need to explicitly declare source files; they only
declare build artifacts.

### Failure semantics

- A node whose `Run()` fails marks itself failed; downstream nodes see the
  failure and abort without running.
- `NeedsRun()` returning true due to a failed type check is treated as
  "needs build" — the build proceeds rather than failing on a freshness
  check error.
- Fail-fast on the first failure; in-flight nodes complete but no new ones
  start.

### Cycle detection

Done during graph construction. Reports the cycle path.

---

## Part 4: CLI

```
mmk [-j <n>] [-v] [-dump] [-builtins] [[verb] target]
```

- `-j <n>` — parallelism (default: 0 = unlimited).
- `-v` — verbose: log each target as it runs or is skipped.
- `-dump` — print the generated shell script and exit (does not run).
- `-builtins` — print built-in type/body definitions as mmk syntax and exit
  (works without an mmkfile).
- Target defaults to `all` if not specified.
- A single non-option argument is treated as a target if it matches a known
  target, otherwise as a verb (running `<verb> all`).
- Two non-option arguments are `verb` and `target`.
- Mmkfile is named `Mmkfile` or `mmkfile` in the current directory.

---

## Part 5: Out of Scope

These are deliberately deferred:

- **Includes / multi-file mmkfiles.** Single-file only.
- **`-k` / keep-going mode.**
- **Watch mode.** Re-run on file change.
- **Content-based fingerprinting.** Bazel/Nix style content hashes.

---

## Appendix: Comparison Table

| Concern              | mmk / mmk2                   | mmk3                                |
|----------------------|------------------------------|-------------------------------------|
| Core executor        | `Node[T,U]` with `Modtime`   | `Node[T]` with `NeedsRun()`         |
| Shared context       | `U` threaded through methods | Closures over node state            |
| Multi-rule targets   | One `Target`, many rules     | One node per (target, verb)         |
| DSL                  | Custom mmkfile syntax        | Annotated bash                      |
| Body execution       | Lines fed to bash one by one | Full bash function in own process   |
| Freshness            | Built-in modtime / `created` | User-defined `deftype`              |
| Execution env        | Always local bash            | `on <image-target>` (Docker)        |
| Variable expansion   | Custom var system            | Bash-native via generated script    |
| Pattern targets      | Yes                          | Yes (single-quoted regex)           |
| Passthrough bash     | No                           | Yes (variable defs, loops, etc.)    |
| Verb system          | No                           | Yes (`[verb target]`, `defbody`)    |
| Built-in types       | No                           | `source`, `file`, `image`           |
