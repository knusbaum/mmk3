# mmk3

A make-like build tool with a bash-native DSL.

`mmk` reads an `mmkfile` describing your build as annotated bash. Each target's
body is a regular bash function. The tool figures out what's stale, runs the
DAG in parallel, and (optionally) executes target bodies inside Docker
containers. Beyond what `make` offers, `mmk` adds:

- **Pluggable freshness types.** `file`, `source`, and `image` are built in;
  declare your own with `deftype` to teach `mmk` how to date any artifact.
- **Container runners.** `on <image>` runs a target's body inside a container
  built from another target. Build, test, and shell-in-image are one
  declaration each.
- **Parameterized targets.** `for x in [...]` produces a matrix of combos. Pair
  with `group`/`into` to fan producers into a pool that downstream consumers
  project over without naming each producer.
- **Verbs.** `[clean prog]`, `[fmt all]`, etc. — one rule, many invocations,
  with sensible inheritance from the default rule.
- **Subprojects.** Delegate parts of a build to nested mmkfiles, addressed
  with `mmk verb sub/target`.

## Install

```
go install github.com/knusbaum/mmk3/cmd/mmk@latest
```

Or build from source:

```
git clone https://github.com/knusbaum/mmk3 && cd mmk3 && go build ./cmd/mmk
```

## Quick start

Create an `mmkfile` (or `Mmkfile`) in your project directory:

```bash
all : hello

file hello : hello.c {
    cc -o hello hello.c
}
```

Then:

```
mmk          # build the default target ('all')
mmk hello    # build 'hello' specifically
mmk clean    # clean verb on 'all' (deletes 'hello' via the built-in file/clean)
mmk -list    # show user-facing targets (those with a ## docstring) plus 'all'
mmk -graph   # print the dep tree for 'all'
```

`mmkfile` may also be named `Mmkfile`. Both are checked, in that order.

## A worked example

```bash
# Plain bash variables — emitted into the generated script and visible everywhere.
CC=gcc
CFLAGS='-O2 -Wall'

# 'all' has no type, so it always runs after its deps. Used as a phony aggregator.
all : prog

# Build a typed file from a list of object deps. Built-in 'file' freshness:
# rebuild only if any dep is newer than $target.
file prog : main.o util.o {
    $CC -o $target main.o util.o
}

# Pattern rule: any '.o' file is built from the matching '.c' file.
# Single-quoted regex; $1 is the first capture group.
file '(.*)\.o' : $1.c {
    $CC $CFLAGS -c $1.c -o $target
}

# An image target. Body defaults to `docker build -t $target -f Dockerfile .`
# (the first dep is the Dockerfile).
image build:1 : Dockerfile

# 'on build:1' runs this body inside a container of build:1. mmk starts the
# container once, exec's each `on build:1` body into it, and tears it down at
# the end of the build.
file packaged on build:1 : prog {
    tar czf packaged prog
}

# A verb. `mmk run` invokes [run all]; this rule lets `mmk run` actually do
# something — execute the program after building it.
[run all] : prog {
    ./prog
}
```

What this gives you out of the box:

- `mmk` — builds `prog` if any of its sources changed.
- `mmk packaged` — builds `prog`, then tars it inside the container.
- `mmk clean` — removes `prog` and its `.o` files (the built-in `file` clean
  verb), and removes the `build:1` Docker image (the built-in `image` clean
  verb), in the right order.
- `mmk run` — builds `prog` and runs it.
- `mmk -list` — shows targets you've documented with a `##` docstring,
  plus `all`. Add `-all` to see everything (including pattern rules and
  internal aggregators).

The rest of this document describes each piece in turn.

## Targets and rules

A target rule looks like:

```
[type] target [on runner] [: dep ...] [{ body }]
```

The pieces:

- **type** (optional) — tells `mmk` what kind of artifact this is, which
  controls how freshness is checked. Built-in: `file`, `source`, `image`.
  Omit for an always-run target.
- **target** — the name to build. Any character bash allows in a function
  name (`/`, `.`, `:`, `-`, `@`, `,`, etc.). Forbidden: ``$()<>`"'\\ \t\n[=``.
- **on runner** (optional) — run the body in a container. See [Runners](#runners).
- **`: deps`** (optional) — space-separated list of dependencies. Omitting
  the colon entirely has a special meaning on verb rules (see [Verbs](#verbs)).
- **`{ body }`** (optional) — bash code to run. May appear on the same line
  or following lines. Braces inside strings or comments don't count.

Examples:

```bash
all : prog                       # untyped aggregator, no body
file prog : main.o { gcc -o prog main.o }
file generated.h : schema.json   # typed, no body — uses the type's defbody
clean { rm -f *.o prog }         # untyped, always runs
```

A bare word with no `:` and no `{` is **passthrough bash**, not a target.
Write `T :` if you want a body-less, dep-less target.

### Pattern rules

Single-quoted target name = regex with capture groups:

```bash
file '(.*)\.o' : $1.c {
    cc -c $1.c -o $target
}
```

- The regex is anchored (`^` and `$` are added automatically).
- `$1`, `$2`, ... are the capture groups, available in the **body** and in
  **deps**.
- `$target` is always the matched name.
- A concrete target with the same name takes precedence over any pattern.

### Variable expansion in deps and target names

Variables defined in passthrough bash are usable in dep lists, concrete
target names, and `on` runner clauses:

```bash
SRCS=$(ls *.c)
OBJS=$(echo $SRCS | sed 's/\.c$/.o/g')

file prog : $OBJS {
    cc -o prog $OBJS
}
```

- Only tokens that **start with `$`** are expanded; literal names are taken
  as-is.
- Word-splitting applies in dep lists: `$VAR` holding `a.o b.o` produces two
  deps.
- Target names and runners must expand to **exactly one** word.
- Pattern target names (single-quoted) are not expanded.

Passthroughs run **once** at parse time, in a single bash subprocess. The
resulting variable values and function definitions are frozen into the
generated script that target bodies source — they are not re-evaluated per
target.

### Rule options (`key=value`)

Options are how rules **parameterize a generic body**. They're most useful
with `defbody`, `defrunner`, and `deftype` — a single shared body adapts to
each target by reading the target's options as bash variables.

```bash
# A type with a generic body that reads `bucket` from each target.
deftype s3_object {
    aws s3api head-object --bucket "$bucket" --key "$target" \
        --query LastModified --output text 2>/dev/null || return 1
}

defbody s3_object {
    aws s3 cp - "s3://$bucket/$target" < "${dep[0]}"
}

# Each target shares the deftype/defbody pair above; they differ only in `bucket`.
s3_object reports/q1.csv  bucket=acme-prod : data/q1.csv
s3_object dev/scratch.csv bucket=acme-dev  : data/scratch.csv
```

Options are visible in **every body that runs on behalf of a rule**: the
target's own body, the type's `deftype` (freshness check), the type's
`defbody` (when no body is set), and the runner's `defrunner` phases. A
plain assignment inside the target body (`bucket=acme-prod` *inside* `{ ... }`)
wouldn't reach the other phases — they run instead of, or before, the target
body.

When a target uses `on R`, both the runner's options and the target's
options are in scope. On collision the **target's** value shadows the
runner's; matrix variables (`for V in [...]`) shadow both.

#### Syntax

Options may appear anywhere in the rule header — before or after `on R`,
interspersed with `for V in [...]` clauses, etc. Bare values are
word-bounded; values with spaces use a quoted form:

```bash
image winbuild platform=linux/amd64 forward_env="VERSION TAG" : Dockerfile
```

Values may contain `:`, `/`, `=`. Reserved keys (would shadow `mmk`'s own
variables): `target`, `deps`, and anything starting with `MMK_`.

### Body environment

Every target body has these variables when it runs:

- `$target` — target name as written in the source.
- `$deps` — space-separated dep list.
- `dep` — bash array form of `$deps` (so `${dep[0]}`, `"${dep[@]}"`, etc.).
- `$1`, `$2`, ... — pattern capture groups (pattern rules only).
- Each `key=value` option from the rule header (and runner header, see
  [Runners](#runners)).
- Each matrix variable from a `for V in [...]` clause (see [Matrix targets](#matrix-targets)).

These are also available to `deftype` and `defbody` bodies.

### Docstrings (`##` comments)

Lines starting with `##` immediately preceding a target rule, subproject, or
group attach as a description. `mmk -list` displays the first line:

```bash
## Build the C launcher.
file launcher : main.c { gcc -o $target main.c }

## All built-in test cases.
group tests
```

Multiple `##` lines concatenate (newline-separated). A regular `#` comment
or any non-comment, non-blank line resets the pending docstring.

Docstrings also act as a **public/private marker** for `mmk -list`:
without `-all`, only docstringed targets (plus `all`) are listed.
Skipping the docstring is how you say "this target is internal —
necessary for the build, but I don't expect users to invoke it directly."
Pattern rules, image-runner aggregators, intermediate `.o` files, etc.
are typically left undocumented; user-facing entry points get a
docstring.

### File-level description (`##!` comments)

Lines starting with `##!` at the very top of the file — before any other
directive or passthrough bash — attach as the file's description, printed
as a header above `Targets:` by `mmk -list` (with or without `-all`):

```bash
##! Stand up demo environments for the widget service.
##! Run `mmk -list -all` to see the underlying building blocks.

all : demo
```

```
$ mmk -list
Stand up demo environments for the widget service.
Run `mmk -list -all` to see the underlying building blocks.

Targets:
  all   → demo
  demo  ...
```

`##!` is deliberately distinct from the per-rule `##` docstring, so a
leading `##` block still attaches to the first target rule as it always
has — the two markers never compete for the same comment block. Only the
root mmkfile's `##!` block is honored: an `include`d file's own `##!`
lines are not merged into the parent's description.

## Types and freshness

A type tells `mmk` how to determine when an artifact was last built. Built-in:

| Type        | Freshness check                          | Default body                                | Clean verb         |
|-------------|------------------------------------------|---------------------------------------------|--------------------|
| `source`    | mtime (`stat`)                           | error if file absent                        | none               |
| `file`      | mtime (`stat`)                           | error if file absent                        | `rm -f "$target"`  |
| `directory` | exists (`test -d`); fixed-low mtime once present | `mkdir -p "$target"`                   | `rm -rf "$target"` |
| `image`     | `docker inspect --format '{{.Created}}'` | `docker build -t $target -f ${deps%% *} .`  | `docker image rm`  |

`source` vs `file`: both are mtime-based. `source` is inferred for any dep
with no rule and no matching pattern — it represents an existing file `mmk`
didn't create and shouldn't delete (no clean verb). `file` is for build
artifacts.

`directory` is for "this directory needs to exist before a consumer's body
runs." Once present, its freshness check returns a fixed-low timestamp
(epoch 1) instead of the dir's actual mtime — otherwise every file added
to or removed from the dir would churn every consumer that depends on it.
Use it to declare build-tree directories without sprinkling `mkdir -p` in
every recipe:

```bash
directory build/src :
file build/src/foo.o : foo.c build/src { cc -c foo.c -o $target }
```

Print all built-in definitions as mmk syntax with `mmk -builtins`.

### `deftype` — defining your own type

```bash
deftype my_artifact {
    # Body prints epoch seconds, "epoch.nanos" (e.g. `stat -c %.Y`),
    # or RFC3339 to stdout.
    # Non-zero exit means "doesn't exist yet".
    my-tool stat "$target" 2>/dev/null || return 1
}
```

`$target` and `$deps` are available. Once defined, `my_artifact T : ...` uses
this freshness check.

### `defbody` — default build body for a type

```bash
defbody my_artifact {
    my-tool build --output "$target" $deps
}
```

Used when a typed target has no explicit body. Override any built-in by
declaring your own `defbody` with the same name.

A `defbody` becomes much more useful when the body reads per-target options
as bash variables — that's how a single shared `defbody` serves many
targets that differ in configuration. See [Rule options](#rule-options-keyvalue)
for the worked example with `bucket=`.

### `defbody` for verbs

```bash
defbody my_artifact clean {
    my-tool delete "$target"
}
```

Now every `my_artifact` target automatically supports `mmk clean <target>`.

## Verbs

A *verb* is an alternate behavior for a target. Default verb is the empty
string (the standard build). Named verbs are run with `mmk <verb> <target>`,
or referenced in a dep list as `[verb deptarget]`.

```bash
[run prog] : prog {
    ./prog
}

[clean all] : [clean prog] [clean main.o] [clean util.o]
```

`[verb target]` is a valid dep in **any** rule, not just in verb rules. Use
it when the prerequisite is an action rather than an artifact:

```bash
# A plain (non-verb) rule that depends on a verb action.
deploy : [verify all] artifact {
    upload-tool artifact
}

# A verb rule whose deps mix verbs and plain targets.
[teardown stack] : [teardown app-layer] auth-token {
    teardown-tool stack
}
```

Saying `deploy : [verify all]` is the right way to express "running deploy
implies first running verify" when the prerequisite is a side-effecting
action rather than something that produces a file you'd dep on directly.

### Dep inheritance for verb rules

A verb rule's dep list defaults to **the target's default deps with the verb
applied**:

```bash
file prog : main.o util.o { gcc -o prog main.o util.o }

# No colon — inherits prog's deps. mmk fills in [clean main.o] [clean util.o].
[clean prog]
```

The three forms:

- **No colon**: inherit the default rule's deps, verb applied to each.
- **`:` (colon, possibly empty list)**: explicit replacement. Use `: ` with
  no entries to suppress inheritance entirely.
- **`:+`**: inherit + extend. `[clean foo] :+ [clean myimg]` is "clean foo's
  normal deps, plus also clean myimg."

### Runners and verb rules

A verb rule's `on <runner>` clause adds the runner as an implicit dep so the
body can execute inside it. **Verbs do not inherit `on` from the default
rule** — the runner is shared infrastructure, and applying e.g. `clean` to it
along with its consumers would race or cycle.

To sequence a runner-verb relative to its consumers, use `order=` on the
defbody:

```bash
defbody myimg clean order=after-consumers {
    docker image rm -f "$target"
}
```

`order=after-consumers` makes `[clean T]` run after every target with
`on T` (including `[verb consumer] on T`). `order=before-consumers` is the
inverse. **Order edges are order-only**: they only kick in when both nodes
are independently in the DAG. Invoking `[clean T]` alone does not pull
consumers in.

The built-in `image` type ships `defbody image clean order=after-consumers`,
so cleaning a runner image automatically waits for every target that used
it — no per-mmkfile setup required. To pull image cleanup into
`mmk clean all`, add the image to `all`'s dep list explicitly:

```bash
[clean all] :+ [clean myimg]
```

`order=` is only valid on a defbody whose type has a `defrunner` — there are
no consumers to order against otherwise.

## Runners

Use `on <image>` to run a target's body inside a Docker container started
from a named `image` target:

```bash
image build:1 : Dockerfile

file prog on build:1 : main.c {
    cc -o prog main.c
}

[shell all] on build:1 tty=true : prog {
    PS1='(build) $ ' bash -i
}
```

Lifecycle:

1. `mmk` builds `build:1` if stale.
2. `mmk` runs the runner's `setup` phase once: `docker run -d` with the
   working directory bind-mounted at `/work`, plus `$MMK_GENFILE` mounted at
   `/mmk-generated.sh:ro`. The container ID is the runner state.
3. Each `on build:1` body is exec'd into that container.
4. `mmk` runs the runner's `cleanup` phase at the end of the build,
   removing the container.

Multiple targets sharing the same runner share one container.

### Built-in image runner options

Among the built-in types (`file`, `source`, `image`), only `image` reads
any options — the table below enumerates them. `file` and `source` ignore
options entirely. To introduce options for your own targets, write a
`deftype` / `defbody` / `defrunner` that references them as bash variables;
see [Rule options](#rule-options-keyvalue) for an example.

The built-in `image` runner honors these options on the image target *or*
on consumer rules:

| Option              | Meaning |
|---------------------|---------|
| `platform=...`      | Passed as `--platform` to both `docker build` and `docker run`. |
| `forward_env="A B"` | Each var name is forwarded into `docker exec` via `-e`. |
| `skip_if=<bash>`    | If the snippet exits 0, skip docker entirely and run bodies in the local shell. The magic value `skip_if=auto` checks for common in-container signals (`/.dockerenv`, `/run/.containerenv`, `$KUBERNETES_SERVICE_HOST`, `/proc/1/cgroup`). |
| `user=<value>`      | Passed as `--user`. The magic value `user=host` expands to `$(id -u):$(id -g)` on Linux and to nothing on macOS/BSD. Use it so bind-mounted artifacts end up owned by the developer. |
| `tty=true`          | On the rule (or runner): allocate a PTY for `docker exec` (`-t`) and forward host stdin. Use for interactive shells. Default off. |

```bash
image dev:1 platform=linux/amd64 user=host skip_if=auto : Dockerfile
file prog on dev:1 : main.c { cc -o prog main.c }
```

When a target with `on R` runs, both the runner's options and the target's
options are in scope. On collision the **target's** value shadows the
runner's.

### Custom runners (`defrunner`)

The runner type is determined by the target's type (`image` is the only
built-in). To define your own runner type, use `defrunner`. There are up to
three phases:

```bash
deftype kvm_vm {
    virsh dominfo "$target" 2>/dev/null | awk '/CPU.time/{print "0"}' || return 1
}

defrunner kvm_vm setup {
    # Run once at the start of the build. Stdout is captured as the runner state,
    # passed back to run/cleanup as $MMK_RUNNER_STATE.
    virsh start "$target"
    printf '%s' "$target"
}

defrunner kvm_vm {
    # The mandatory 'run' phase. Receives:
    #   $MMK_RUNNER_STATE — what setup printed.
    #   $MMK_TARGET, $MMK_DEPS — the consumer's target/deps.
    #   $MMK_EXECUTE — the consumer's body, ready to eval.
    ssh "root@$MMK_RUNNER_STATE" "MMK_TARGET=$MMK_TARGET MMK_DEPS=$MMK_DEPS bash -c '$MMK_EXECUTE'"
}

defrunner kvm_vm cleanup {
    virsh shutdown "$MMK_RUNNER_STATE"
}

kvm_vm builder.local :
file prog on builder.local : main.c { cc -o prog main.c }
```

The setup and cleanup phases are optional; if you supply either, you must
also supply the run phase.

#### Runner dep clause (`defrunner T : depexpr ... { ... }`)

A `defrunner`'s run-stage form may carry an optional dep list, mirroring
the dep clause on `defbody`:

```bash
defrunner TYPE [opts] : <depexpr> ... { run body }
```

Each token is a raw bash expression (commonly `$(...)`) evaluated at graph
construction time, per runner instance, with the runner's options bound as
bash variables and `$target` set to the runner target's name. The output is
word-split and the resulting names are **appended to the dep list of every
target that says `on T`** — augmenting, not replacing, the consumer's own
explicit deps.

Three forms, with distinct semantics:

| Form | Behavior |
|---|---|
| No `:` clause | Historical default. `on T` adds the runner target itself. |
| `:` followed by tokens | Output of the tokens replaces the auto-add. |
| `:` followed by nothing | Explicit "no deps." Useful for opting consumers out entirely. |

The built-in `image` runner uses this to elide the consumer→image edge
when `skip_if` matches (see `mmk -builtins`). A custom runner type can do
the same to inject prereq-of-the-runner targets:

```bash
deftype remote_shell { ... }
defrunner remote_shell : $target ssh_key.pem { ... }
remote_shell vm.example.com :
file prog on vm.example.com : main.c { cc -o prog main.c }
# prog effectively depends on: main.c, vm.example.com, ssh_key.pem
```

A dep clause on `defrunner T setup { ... }` or `defrunner T cleanup { ... }`
is a parse error — setup/cleanup are runner lifecycle, not contributors of
consumer deps.

## Matrix targets

A `for VAR in [expr]` clause expands a single rule into one combo per value:

```bash
file build for go in [1.20 1.21 1.22] {
    go build -o "build-$go" ./...
}
```

This generates three concrete targets — `[build @ go=1.20]`, `[build @ go=1.21]`,
`[build @ go=1.22]` — plus an aggregator `build` that depends on all three.

Multiple `for` clauses cross-product:

```bash
file test for os in [linux macos] for go in [1.20 1.21] {
    go test ./...
}
```

Six combos. Use `exclude [...]` to drop some:

```bash
file test for os in [linux macos windows] for go in [1.20 1.21 1.22]
    exclude [os=windows go=1.20]
    exclude [os=macos]
{
    go test ./...
}
```

`exclude` clauses partial-match: `exclude [os=macos]` drops every combo with
`os=macos`.

The bracketed expression after `in` is bash, evaluated at build time.
Anything bash splits on whitespace works:

```bash
PLATFORMS="linux darwin"
build for os in [$PLATFORMS] { ... }
build for v in [$(seq 1 5)] { ... }
build for word in [a b "c d"] { ... }
```

Inside the body, the matrix variables are exported as bash variables:

```bash
file build for os in [linux macos] for arch in [amd64 arm64] {
    GOOS=$os GOARCH=$arch go build -o "build-$os-$arch" ./...
}
```

Variable substitution also happens in the runner clause and dep list:

```bash
image runner-linux : .ci/Dockerfile.linux
image runner-macos : .ci/Dockerfile.macos

file build for os in [linux macos] on runner-$os : src/$os.c {
    cc -o "build-$os" "src/$os.c"
}
```

### Addressing combos in dep lists

A plain dep on a matrix target name resolves to the **aggregator** —
"depend on all combos":

```bash
release : build           # depends on all build@... combos
```

To depend on a specific combo, use `[target @ k=v ...]`:

```bash
release_linux : [build @ os=linux go=1.21]
```

Combo dep specifiers can fan out by leaving keys unconstrained:

```bash
linux_only : [build @ os=linux]   # depends on every build combo with os=linux
```

`$var` substitution works inside combo values, useful when the consumer is
itself a matrix:

```bash
test for os in [linux macos] : [build @ os=$os] {
    ./run-tests-on $os
}
```

Restrictions:

- `for` clauses are not allowed on **pattern rules** or directly on **verb
  rules** (declare the matrix on the base rule; verbs inherit the matrix
  via the aggregator).
- An explicit combo dep that matches zero combos is an error.

## Groups

Use a group when several producers contribute to a pool that downstream
consumers iterate over without naming each producer.

```bash
group test_inputs

file gen_a for input in [a1 a2 a3] into test_inputs {
    generate-test-input $input > "$target"
}
file gen_b for input in [b1 b2 b3] into test_inputs {
    generate-test-input $input > "$target"
}

# Consumer fans out: one consumer combo per distinct `input` value across
# all members of test_inputs. Result: 6 consumer combos.
file run_test for input in [test_inputs] : [test_inputs @ input] {
    run-test < "$deps"
}
```

The pieces:

- **`group NAME`** declares a pool. Required before anything references it.
- **`into NAME`** on a target rule registers the rule (or all of its combos,
  if it's a matrix rule) into the named group.
- **Plain dep on a group name** (`: g` or `: [g]`) is a flat fan-in — depend
  on every member.
- **Group projection dep** `[g @ dim1 dim2 ...]` fans the consumer out
  across the distinct value-tuples of the projected dimensions among
  members. Each consumer combo receives only the members matching its
  dim-tuple.

A member that doesn't have one of the projected dims is silently excluded
from that projection (but still contributes to flat deps and to other
projections that don't require the missing dim).

Groups can cascade: a consumer that's `into another_group` makes its combos
members of `another_group`, and consumers of *that* group fan out further.
The runtime resolves cascades to a fixed point.

The group aggregator itself is addressable: `mmk g` builds every member;
`mmk clean g` cleans every member.

## Splitting an mmkfile (`include`)

When an mmkfile gets large, split it across files and compose them with
`include`:

```bash
# mmkfile
include lib/build.mmk
include lib/tests.mmk
include ops/deploy.mmk

all : svc tests
```

`include` is a parse-time lexical splice: the included file's directives
are inserted in place of the directive, exactly as if you'd typed them
inline. Result is **one** namespace, **one** DAG, **one** generated bash
script — targets in `lib/build.mmk` can dep on targets in `lib/tests.mmk`,
and variables defined in passthrough above an include are visible inside
the included file.

Properties:

- **Path is relative to the including file.** `include lib/foo.mmk`
  inside `sub/build.mmk` reads `sub/lib/foo.mmk`, not `./lib/foo.mmk`.
- **Each absolute path is included at most once per build.** Re-includes
  and cycles (A includes B, B includes A) are silent no-ops.
- **Variable expansion is supported in the path.** `include $LIBDIR/foo.mmk`
  works, evaluated against passthroughs that have appeared above the
  directive (in this file or in earlier-included files).
- **Both bare-word and quoted forms work.** Quote when the path has
  spaces: `include "lib/with spaces.mmk"`.

By convention, included files use the `.mmk` extension; the parser
doesn't enforce it. `mmk -dump` prints the union of all directives and
is the right tool to confirm the splice is what you expected.

### `include` vs `subproject`

`include` and `subproject` solve different problems. Pick the one whose
behavior matches what you want:

| | `include` | `subproject` |
|---|---|---|
| Number of mmk processes | One | One per subproject (parent shells out) |
| Target namespace | Shared with parent | Isolated; reached via `<name>/<target>` |
| Cross-file deps | Direct: `a : b` works across files | Through the subproject's name |
| Variables from parent | Visible in included files | **Not** visible in subprojects |
| Use when | Splitting one logical build | Composing genuinely separate builds |

## Subprojects

A `subproject` directive delegates part of the build to a nested mmkfile:

```bash
subproject src
subproject docs path=site

all : src docs
```

At parse time, `mmk` reads each subproject's mmkfile and:

- Generates a top-level rule `<name>` whose body is `(cd <path> && mmk)`.
- For every verb the subproject knows about (recursively), generates
  `[verb <name>]` whose body is `(cd <path> && mmk <verb>)`.

So `mmk fmt src` becomes `(cd src && mmk fmt)`. `mmk -list` shows the
sub-targets and verbs as if they were declared at the top level.

Sub-targets are addressable via slash syntax:

```
mmk fmt src/foo            # cd src && mmk fmt foo
mmk src/lib/util           # cd src && mmk lib/util (recursion is fine)
```

Options:

- `path=<dir>` — directory to delegate to, if different from the target name.
- `on <runner>` — wrap each generated rule in `on <runner>`, so subproject
  invocations run inside that container.

Subprojects don't auto-include in `all`'s deps; list them explicitly.

## CLI

```
mmk [flags] [[verb] target]
```

| Flag         | Description |
|--------------|-------------|
| `-j N`       | Parallelism. Default 0 = unlimited. |
| `-v`         | Verbose: log each target as it runs or is skipped. Inherited by sub-mmk invocations via `MMK_VERBOSE=1`. |
| `-replay-failure-output` | Replay the first failed target's captured stdout/stderr in the failure summary. By default, output is shown live only. |
| `-list`      | List user-facing targets and verbs. By default, only targets with a `##` docstring are shown (plus `all`); use with `-all` to show everything. |
| `-list -all` | With `-list`, also show internal/undocumented targets, plus pattern rules and matrix/group/runner aggregators. |
| `-graph`     | Print the dependency tree (text) for the chosen target+verb. |
| `-graph -full` | Recurse into subprojects (one mmk subprocess per subproject) and splice their graphs. |
| `-dag`       | Render the dependency graph as a top-down boxes-and-arrows diagram. |
| `-dag -mgroup` | With `-dag`, collapse matrix combos sharing a base into one box. |
| `-tui`       | Run the build under a live TUI: tree on top, recent log at the bottom, statuses update as targets run. |
| `-dump`      | Print the generated bash script (the result of expanding all directives) and exit. |
| `-builtins`  | Print built-in `deftype` / `defbody` / `defrunner` definitions as mmk syntax. Works without an mmkfile. |

Positional arguments:

- **No args** — build `all`.
- **One arg** — if it's a known target (or a subpath like `src/foo`), build
  it. Otherwise treat it as a verb and run `<verb> all`.
- **Two args** — `<verb> <target>`.

Examples:

```
mmk                  # build 'all'
mmk prog             # build 'prog'
mmk clean            # [clean all]
mmk clean prog       # [clean prog]
mmk -j 4 -v          # 4-way parallel, log each step
mmk -tui             # interactive TUI
mmk -dag -mgroup     # boxes-and-arrows graph, matrix combos collapsed
```

### TUI cancellation

Inside `-tui`, `Ctrl+C` escalates:

1. **First press** — stop scheduling new tasks. Currently-running bodies
   complete normally.
2. **Second press** — `SIGTERM` to running task processes (and their
   process groups, so `docker exec`, `cc`, etc. get the signal too).
3. **Third press** — `SIGKILL`.

Press `q` or `Esc` to quit once the build has finished.

In non-TUI mode, the regular terminal `Ctrl+C` cascades to the running task
subprocess directly.

## Pointers

- `example/mmkfile` — exercises file/image/pattern/verb/matrix/group on a
  small C project.
- `mmk -builtins` — see exactly how `file`, `source`, `image` are defined.
- `DESIGN.md` — internals: the executor library, parser, runtime, and
  generator. Read this if you want to extend `mmk` itself.
- `CLAUDE.md` — quick-reference for AI agents writing or modifying mmkfiles.
