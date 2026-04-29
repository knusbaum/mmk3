# CLAUDE.md — guide for AI agents writing mmkfiles

This is a quick-reference for agents that need to write or modify an
`mmkfile`. The README has prose explanations and rationale; this file is the
shortest path to "I produce a correct mmkfile on the first try."

If you're trying to understand `mmk` itself rather than write build files
for it, read `README.md` and `DESIGN.md`.

## What an mmkfile is

`mmk` is `make`-like. An `mmkfile` is **annotated bash**:

- Lines that look like `target [: deps] { body }` are *target rules*.
- Anything else is *passthrough bash* — emitted verbatim into the script
  that target bodies source.
- Rule bodies are bash function bodies. They can use any normal bash:
  variables, pipes, subshells, conditionals.

`mmk` parses the file, generates a single bash script of function
definitions, then runs targets in dependency order, sourcing the script
inside each task's subprocess.

## Grammar at a glance

```
# Target rule (concrete name)
[type] target [on runner] [opt=val ...] [for V in [expr] ...] [exclude [k=v ...] ...] [into group ...] [: dep ...] [{ body }]

# Pattern rule (regex with capture groups; $1..$9 in body and deps)
[type] '<regex>' [on runner] [opt=val ...] [: dep ...] [{ body }]

# Verb rule
[verb target] [on runner] [opt=val ...] [: dep ...] [{ body }]
[verb target] [on runner] [opt=val ...] :+ dep ...    # inherit + extend

# Type definitions
deftype TYPE { body that prints epoch-seconds or RFC3339 to stdout; non-zero = absent }
defbody TYPE [opt=val ...] { default body for typed targets with no body }
defbody TYPE VERB [opt=val ...] { default body for [VERB target] on TYPE }

# Custom runner type
defrunner NAME { run-phase body }       # mandatory
defrunner NAME setup { ... }            # optional; stdout = runner state
defrunner NAME cleanup { ... }          # optional

# Subproject delegation
subproject NAME [on runner] [opt=val ...]

# Group declaration / membership / projection
group NAME
<rule> ... into NAME ...
<consumer> : [NAME @ dim1 dim2 ...]    # projection dep
<consumer> : NAME                       # flat dep on all members

# Combo dep on a matrix target
<consumer> : [target @ k=v k=v]

# Passthrough
ANY_OTHER_LINE                          # treated as raw bash
```

## Decision flowchart

| You want to... | Use this |
|---|---|
| Make sure something stays up to date as a file on disk | `file T : deps { ... }` |
| Reference an existing file mmk doesn't build | Just name it as a dep — mmk infers `source` |
| Define an always-run task (no caching, no artifact) | Untyped: `T : deps { ... }` |
| Aggregate several builds under one name | Untyped, deps only: `all : a b c` |
| Repeat the same rule across a list of values | Matrix: `T for v in [1 2 3] { ... }` |
| Repeat across the cross-product of two lists | Two `for` clauses |
| Skip some combos | `... exclude [k=v k=v] ...` |
| Run a target's body inside a Docker container | Declare `image I : Dockerfile`, then `T on I` |
| Same code path locally and in CI | `image I skip_if=auto : Dockerfile` (auto-detects in-container) |
| Allow `mmk clean T` (or another verb) on a target type | `defbody TYPE clean { ... }` |
| Add a one-off `clean`, `run`, etc. to a target | `[verb T] { ... }` |
| Several producers feeding fan-out consumers without naming each producer | `group g`; `into g` on producers; `[g @ dim]` on consumer |
| Delegate part of the build to a sub-directory mmkfile | `subproject NAME [path=DIR]` |

## Idioms

```bash
# Variable assignments at the top, expanded in deps and bodies.
CC=gcc
CFLAGS='-O2 -Wall'

# An aggregator with an explicit default. `mmk` (no args) builds this.
all : prog
```

```bash
# Image runner that works both on a developer laptop and inside a CI container.
# user=host makes bind-mounted artifacts owned by the developer on Linux.
image build:1 skip_if=auto user=host : Dockerfile

file artifact on build:1 : src.c {
    cc -o $target src.c
}
```

```bash
# Matrix + group + projection: each producer combo registers into the group;
# the consumer fans out across distinct dim-tuples.
group cases

file generate for case in [a b c] into cases {
    generate-input $case > "$target"
}

file analyze for case in [cases] : [cases @ case] {
    run-analysis < "$deps"
}
```

```bash
# Custom freshness type, parameterized via the `bucket` option.
# Both the deftype (freshness check) and the defbody (upload) read $bucket,
# so a single type definition serves any number of targets in any number of buckets.
deftype s3_object {
    aws s3api head-object --bucket "$bucket" --key "$target" \
        --query LastModified --output text 2>/dev/null || return 1
}

defbody s3_object {
    aws s3 cp - "s3://$bucket/$target" < "${dep[0]}"
}

s3_object reports/q1.csv  bucket=acme-prod : data/q1.csv
s3_object dev/scratch.csv bucket=acme-dev  : data/scratch.csv
```

```bash
# Verb that augments inherited deps. ':+' adds [clean myimg] without losing
# the [clean main.o] / [clean util.o] that prog's deps would otherwise contribute.
file prog on myimg : main.o util.o { ... }

[clean prog] :+ [clean myimg]
```

## Pitfalls

These are real and have bitten users; check yourself against them before
shipping.

### Parser ambiguities

- A bare line `foo` (no `:`, no `{`) is **passthrough bash**, *not* a target.
  If you want a target with no deps and no body, write `foo :`.
- `IDENT=value` lines are bash variable assignments (passthrough). They are
  never parsed as target rules, even if they contain `:`.
- `name (` with a paren after the first word is a bash function definition
  (passthrough).
- Single-quoted strings (`'...'`) are only legal as the **target name** in a
  pattern rule. Never use them as a dep, runner, or type.
- Double-quoted target names are allowed but only useful when the name
  contains characters that need quoting. Most targets are bare words.

### Naming

- Targets may use `/`, `.`, `:`, `-`, `@`, `,`. Forbidden: ``$()<>`"'\\ \t\n[=``.
  These are the characters bash forbids in function names.
- Reserved option keys: `target`, `deps`, anything starting with `MMK_`.
- Subproject targets may not have bodies. The body is generated for you.

### Variables and expansion

- Variables defined in passthrough bash are visible in dep lists, target
  names (concrete only), runner names, and bodies.
- Pattern target names (single-quoted) are **not** variable-expanded.
- Target names and runner names must expand to **exactly one word**.
  Multi-word expansion is an error.
- Dep lists allow multi-word expansion (each word becomes a dep).
- Passthroughs run **once** at parse time. They cannot reference per-target
  state like `$target`. Use them for static values and helper functions.
- `key=value` options on a rule header are bash vars in **every body the
  type contributes** — the target body, the type's `defbody`/`deftype`,
  and any `defrunner` phases. A plain `key=value` inside `{ ... }` is only
  visible to that one body. Use options when a `deftype` / `defbody` /
  `defrunner` needs to read per-target config (their bodies run instead
  of, or before, the target body and can't see its locals).

### Pattern rules

- Anchored automatically (`^...$`). Don't add anchors yourself.
- Captures `$1`...`$9` are available in the body **and** in deps.
- Concrete rules with the same name win over patterns.
- Pattern rules **cannot** carry `for ... in [...]` matrix clauses. If you
  need both, define a base rule with the matrix and don't use a pattern.

### Verb rules

- Default behavior with no `:` is to inherit the default rule's deps with
  the verb applied. Use `: ` (colon, empty list) to opt out.
- `:+` is **only** valid on verb rules. Using it elsewhere is an error.
- Verb rules **do not** inherit `on <runner>` from the base rule. If you
  need the verb to run inside the runner, declare `[verb T] on R` explicitly.
- To make a runner-verb fire after consumers automatically, give it
  `order=after-consumers` (only valid for types that have a `defrunner`).

### Matrix and groups

- `for V in [...]` clauses cross-product. Two clauses with 3 and 4 values
  produce 12 combos.
- `exclude [k=v]` partial-matches: drops every combo with that key=value.
- All combos excluded → error.
- A plain dep on a matrix target name resolves to the **aggregator** —
  consumer depends on every combo.
- `[T @ k=v]` resolves to a specific combo (or fans out if some keys are
  unconstrained). Zero matches → error.
- `$var` substitution works inside combo values: `[T @ os=$os]` lets a
  matrix consumer pin to its own combo's value.
- Group projection deps `[g @ dim ...]` require that **at least one
  member** has all the projected dims. If members each contribute different
  dims separately (disjoint), use multiple `[g @ dim1] [g @ dim2]` deps
  instead.
- Members lacking a projected dim are silently excluded from that
  projection; they still appear in flat (`: g`) deps.

### CLI surprise

- `mmk T` runs `T` if it's a known target; otherwise treats `T` as a verb
  and runs `<verb> all`. Spelling a target name wrong therefore looks like
  "unknown verb" rather than "unknown target."
- `mmk -dump` is the right tool for "did mmk parse what I expected?" — it
  prints the generated bash script.
- `mmk -list` shows targets, patterns, and verbs with descriptions.
- `mmk -graph` and `mmk -dag` show the dep graph.

## Body environment cheatsheet

These are set in every target body, `deftype`, and `defbody`:

| Variable | Meaning |
|---|---|
| `$target` | The target name as written in the source. |
| `$deps` | Space-separated dep list. |
| `${dep[@]}` | Same, as a bash array (`read -ra dep <<< "$deps"` already happened). |
| `$1`..`$9` | Pattern capture groups (pattern rules only). |
| each `key=value` option | Set as a bash variable. |
| each matrix `for V in [...]` | `$V` set to the combo's value. |
| `$MMK_GENFILE` | Path to the generated bash script (rarely needed by user code). |
| `$MMK_VERBOSE` | `1` if `mmk -v`; useful in custom runners. |

In **runner run-phase** bodies (custom `defrunner NAME { ... }`), additionally:

| Variable | Meaning |
|---|---|
| `$MMK_TARGET`, `$MMK_DEPS` | Consumer's target name and deps. |
| `$MMK_EXECUTE` | Self-contained snippet you should `eval` (or pass to a remote shell) to run the consumer's body. |
| `$MMK_RUNNER_STATE` | Whatever the setup phase printed to stdout. |

## A complete sample

```bash
## Build a tiny Go service with golangci-lint and a fan-out integration test.

CC_IMG=goci:1
TEST_INPUTS="alpha beta gamma"

all : svc

image $CC_IMG skip_if=auto : .ci/Dockerfile

file svc on $CC_IMG : main.go {
    go build -o $target ./...
}

[lint svc] on $CC_IMG {
    golangci-lint run ./...
}

group test_inputs

file gen for input in [$TEST_INPUTS] into test_inputs {
    ./tools/generate $input > "$target"
}

run_test for input in [test_inputs] : svc [test_inputs @ input] {
    ./svc < "${dep[1]}" > "out/$input.actual"
    diff "expected/$input.expected" "out/$input.actual"
}

test : run_test
```

What this gives the user:

- `mmk` → builds the image (locally if not in container) and `svc`.
- `mmk lint` → `[lint svc]`.
- `mmk test` → fans out across all generated inputs in parallel.
- `mmk clean` → removes `svc`, all `gen` outputs, and the `goci:1` image
  (in the right order — image after consumers).

## When in doubt

- `mmk -dump` — see the generated bash. If the script looks wrong, the
  mmkfile is wrong.
- `mmk -list` — confirm every target, pattern, verb, and group you intended
  to declare is registered.
- `mmk -graph T` / `mmk -dag T` — confirm the dep tree matches what you
  expected.
- `mmk -builtins` — see how `file` / `source` / `image` are implemented in
  mmk syntax. Useful when defining your own `deftype` / `defrunner`.
- `example/mmkfile` — exercises every major feature.
- `README.md` — prose explanation with rationale.
