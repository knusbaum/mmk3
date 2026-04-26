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
- User-extensible via `deftype` (NeedsRun strategies) and `defrunner`
  (execution environments).

### Grammar

```
<type>? <target> (on <runner>)? (: <deps...>)? {
    <body>
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

# File type, with deps — NeedsRun() defined by `file` type.
file main.o : main.c lib.h {
    cc -c main.c -o main.o
}

# File type, with runner — built inside ubuntu container.
file main.o on ubuntu : main.c lib.h {
    cc -c main.c -o main.o
}
```

### Type and runner definitions

```bash
# A type defines NeedsRun() for targets that opt into it.
# Body has $target and $deps available.
# Exit 0 = up-to-date (skip), non-zero = needs build.
deftype file {
    [[ -f "$target" ]] || return 1
    for dep in $deps; do
        [[ "$dep" -nt "$target" ]] && return 1
    done
    return 0
}

deftype docker {
    image_time=$(docker inspect -f '{{.Created}}' "$target" 2>/dev/null) || return 1
    image_epoch=$(date -d "$image_time" +%s)
    for dep in $deps; do
        dep_epoch=$(stat -c %Y "$dep" 2>/dev/null) || return 1
        (( dep_epoch > image_epoch )) && return 1
    done
    return 0
}

# A runner defines the execution wrapper.
# `$@` expands to the actual invocation (bash + sourced generated.sh + function call).
defrunner ubuntu {
    docker run --rm -v "$PWD:/work" -w /work ubuntu:latest "$@"
}

defrunner remote {
    ssh build-host "$@"
}
```

### Defaults

- **No type specified** → always-run. `NeedsRun()` returns `true`. No magic
  default; users opt in to freshness checks explicitly. (`file` is a
  user-definable type, not a built-in — though we'll likely ship a stdlib
  of common types.)
- **No runner specified** → run locally in bash, no wrapper.

### Body environment

Every target body has these env vars set when invoked:

- `$target` — the target name as written in the source (unmangled).
- `$deps` — space-separated string of dependency names.

Same for `deftype` bodies.

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
   - `defrunner` blocks (name, body)
   - Passthrough lines (verbatim bash — variable assignments, loops, etc.)
2. **Validate**:
   - Referenced types and runners exist.
   - No cycles in the dep graph (also caught at exec time, but earlier is
     better).
   - Target names don't collide.
3. **Generate** a temp `generated.sh` containing:
   - Each `deftype` body as a bash function.
   - Each `defrunner` body as a bash function.
   - Each concrete target body as a bash function.
   - Passthrough lines verbatim.
   - Pattern-instantiated targets are appended on demand during DAG
     construction.
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

### Failure semantics

- A node whose `Run()` fails marks itself failed; downstream nodes see the
  failure and abort without running.
- `NeedsRun()` returning true due to a failed type check is treated as
  "needs build" — the build proceeds rather than failing on a freshness
  check error.
- Fail-fast on the first failure; in-flight nodes complete but no new ones
  start. (Future option: `--keep-going`.)

### Cycle detection

Done during graph construction. Reports the cycle path.

---

## Part 4: CLI

```
mmk [-j <n>] [target]
```

- `-j <n>` — parallelism (default: 0 = unlimited).
- Target defaults to `all` if not specified.
- Mmkfile is named `Mmkfile` or `mmkfile` in the current directory.

---

## Part 5: Out of Scope for v1 (Future Work)

These are deliberately deferred so v1 stays small:

- **Variable expansion in dependency lists.** `bunchac : $OFILES { ... }`
  requires running the file through bash to expand variables. v1: literal
  strings only.
- **Includes / multi-file mmkfiles.** Single-file v1.
- **`failok` / per-rule error tolerance.** v1 is fail-fast.
- **`-k` / keep-going mode.**
- **Watch mode.** Re-run on file change.
- **Content-based fingerprinting.** Bazel/Nix style content hashes as an
  alternative to modtime (would be a `deftype` users could write).
- **A standard library of built-in types and runners** (`file`, `phony`,
  `docker`, `ubuntu`, etc.). Likely a stdlib `.sh` file the tool implicitly
  prepends. v1: users define their own.

---

## Part 6: Build Plan

Suggested implementation order:

1. **`dag/` package** — `Node[T]` interface, `Step[T]`, `Semaphore`,
   `Execute()`. Port and clean up from mmk2's `graph.go`. Write tests with
   trivial in-memory nodes (no DSL involvement).
2. **Parser** — `cmd/mmk/parse/`. Read the DSL into an AST. Cover all
   forms in the grammar (with/without type, runner, deps).
3. **Generator** — `cmd/mmk/gen/`. AST → `generated.sh`. Includes name
   validation.
4. **Runtime bridge** — `cmd/mmk/runtime/`. A `Node` implementation
   wrapping a parsed target, calling `NeedsRun()` / `Run()` via bash
   invocations of the generated functions.
5. **CLI** — `cmd/mmk/main.go`. Argument parsing, glue.
6. **End-to-end test** — port `mmkfile3` from mmk2 to the new syntax and
   make it work.

---

## Appendix: Comparison Table

| Concern              | mmk / mmk2                   | mmk3                                |
|----------------------|------------------------------|-------------------------------------|
| Core executor        | `Node[T,U]` with `Modtime`   | `Node[T]` with `NeedsRun()`         |
| Shared context       | `U` threaded through methods | Closures over node state            |
| Multi-rule targets   | One `Target`, many rules     | One node per (target, rule)         |
| DSL                  | Custom mmkfile syntax        | Annotated bash                      |
| Body execution       | Lines fed to bash one by one | Full bash function in own process   |
| Freshness            | Built-in modtime / `created` | User-defined `deftype`              |
| Execution env        | Always local bash            | User-defined `defrunner`            |
| Variable expansion   | Custom var system            | (v2) bash-native                    |
| Pattern targets      | Yes                          | Yes (single-quoted regex)           |
| Passthrough bash     | No                           | Yes (variable defs, loops, etc.)    |
