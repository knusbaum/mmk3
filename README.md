# mmk3

A make-like build tool with a bash-native DSL. Target definitions look like
annotated bash functions; the tool generates a plain bash script and drives
a parallel DAG executor.

## Installation

```
go install github.com/knusbaum/mmk3/cmd/mmk@latest
```

Or build from source:

```
cd mmk3 && go build ./cmd/mmk
```

## Quick start

Create an `mmkfile` (or `Mmkfile`) in your project directory:

```bash
CC=gcc
CFLAGS='-O2 -Wall'

file prog : main.o util.o {
    $CC -o prog main.o util.o
}

file '(.*)\.o' : $1.c {
    $CC $CFLAGS -c $1.c -o $target
}
```

Run it:

```
mmk          # build 'all' (default target)
mmk prog     # build a specific target
mmk clean    # run the clean verb on 'all'
```

## Mmkfile syntax

### Target rules

```
[type] target [on runner] [: dep ...] [{ body }]
```

- **type** — optional; tells mmk what kind of artifact the target is. Built-in: `file`,
  `source`, `image`. Controls how mmk determines freshness. Omit for always-run targets.
- **target** — the name to build. Any character except bash function-name
  invalids (`$()<>\`"'\\ \t\n[=`).
- **on image-target** — optional; run the body inside a container started
  from the named `image` target (see Runners).
- **: dep ...** — optional dep list. Omit the colon entirely to inherit
  the target's dep list in verb rules (see Verbs). Use `: ` with an empty
  list to explicitly have no deps.
- **{ body }** — optional bash body.

```bash
all : prog

# file type — skips build if prog is newer than all deps
file prog : main.o util.o {
    gcc -o prog main.o util.o
}

# Typed, no explicit body — uses defbody for the type
file generated.h : schema.json

# Untyped aggregator — no body, no type, just deps
all : prog docs
```

### Pattern rules

Single-quoted regex with capture groups:

```bash
file '(.*)\.o' : $1.c {
    gcc -c $1.c -o $target
}
```

- The regex is anchored (`^` and `$` are added automatically).
- `$1`, `$2`, ... refer to capture groups and are available in the body **and** in dep lists.
- Use `$target` for the full matched name.

### Verb rules

Verbs give a target an alternate behavior. Declare with `[verb target]`:

```bash
[clean prog] {
    rm -f prog
}

[clean all] : [clean prog] [clean main.o] [clean util.o] {
    :
}
```

Run with `mmk clean all` or `mmk clean prog`.

**Dep inheritance**: A verb rule with no colon inherits its target's dep
list (with the verb applied to each dep). Use `: ` (colon, empty list) to
suppress inheritance, or `:+` to inherit *and* add explicit deps:

```bash
# Inherits deps from 'prog' rule, runs clean on each
[clean prog]

# Explicit empty list — no deps, runs in isolation
[clean prog] : {
    rm -f prog
}

# Inherit prog's deps with `clean` applied, AND also add [clean image]
[clean prog] :+ [clean myimage]
```

A verb rule's `on <runner>` clause adds the runner as a build-verb dep so
the body can execute inside it. mmk does **not** automatically apply the
verb to runners inherited from the default rule — the runner is build
infrastructure shared across many targets, and propagating verbs to it
(especially destructive ones like `clean`) would race with consumers.

For runner-typed verbs that *should* be sequenced relative to their
consumers, declare it on the defbody (or a per-target verb rule) with the
`order=` option:

```bash
defbody myimage clean order=after-consumers {
    docker image rm -f "$target"
}
```

`order=after-consumers` sequences `[verb T]` after every `[verb consumer]`
where the consumer uses `T` as its runner. `order=before-consumers` is the
inverse. The edges are **order-only**: when only `[verb T]` is invoked, no
consumers are pulled in — the ordering only kicks in when both nodes are
already in the DAG via independent paths.

The built-in `image` type ships `defbody image clean order=after-consumers`,
so cleaning a docker image automatically runs after every target that uses
that image — no per-mmkfile setup needed. To pull image cleanup into
`mmk clean all`, add the image to `all`'s deps (or augment with `:+`):

```bash
[clean all] :+ [clean myimage]
```

`order=` on a defbody is only valid for types that have a `defrunner` —
you can't have "consumers" without a way to consume.

### Rule options (`key=value`)

Any rule's header may carry `key=value` annotations. Each option is exported
as a bash variable to bodies of the rule it's declared on. The built-in
`image` runner honors four options out of the box:

- `platform=<platform>` — passed as `--platform` to both `docker build` and
  `docker run`.
- `forward_env="VAR1 VAR2 …"` — each name is forwarded into `docker exec`
  with `-e <VAR>` so the value is inherited from the surrounding env.
- `skip_if=<bash>` — bypass docker entirely and run target bodies in the
  current shell when the snippet returns 0. The magic value `skip_if=auto`
  expands to a built-in detection of common in-container signals
  (`/.dockerenv`, `/run/.containerenv`, `$KUBERNETES_SERVICE_HOST`,
  `/proc/1/cgroup`). Useful when the same mmkfile runs both on a developer
  laptop and inside a CI job that's already in the build container.
- `user=<value>` — passed as `--user` to `docker run` and `docker exec`.
  The magic value `user=host` expands to `$(id -u):$(id -g)` on Linux and
  to nothing on macOS/BSD (where the host UID typically doesn't exist
  inside the container). Use it when you want bind-mounted artifacts to
  end up owned by the developer rather than root.

```bash
image winbuild:1 platform=linux/amd64 forward_env="VERSION TAG" : Dockerfile.windows
image build:1   skip_if=auto                               : .ci/Dockerfile

build on winbuild:1 { cmake --build . }
```

Quoted values are supported for keys that need spaces (like `forward_env`);
bare values are fine for everything else.

For additional knobs, override the runner and read the option as a bash
variable:

```bash
image myimg:1 platform=linux/amd64 user=1000:1000 : Dockerfile

defrunner image setup {
    docker run -d --rm \
        ${platform:+--platform "$platform"} \
        ${user:+--user "$user"} \
        -v "$(pwd):/work" -w /work \
        "$target" sleep infinity
}
```

Options may appear before or after the optional `on <runner>` clause and in
any order. Values may contain `:`, `/`, and `=`; values with spaces require
that you put the value into a passthrough variable and reference it (option
values are bare words).

When a target uses a runner, the runner's options *and* the target's options
are both in scope during the runner-run phase. If both define the same key,
the target's value shadows the runner's:

```bash
mytarget on myimg:1 user=root { ... }      # `user` overrides image-level value
```

The runner author decides which keys to honor at which phase by referencing
them in the relevant body. Keys named `target`, `deps`, or beginning with
`MMK_` are reserved.

### Variable expansion in deps, target names, and runners

Variables defined in passthrough bash are available in dep lists, concrete
target names, and `on` runner clauses:

```bash
SRCS=$(ls *.c)
OBJS=$(echo $SRCS | sed 's/\.c/.o/g')
IMG=injector-build:1

image $IMG : Dockerfile

file prog on $IMG : $OBJS {
    gcc -o prog $OBJS
}
```

Only tokens that start with `$` are expanded; literal names are used as-is.

For deps, word-splitting applies: a variable holding `foo.o bar.o` expands to
two deps. For target names and runners, the expansion must produce exactly
one word — multi-word values are an error.

Pattern target names (single-quoted regexes) are not expanded.

### Subprojects

A `subproject` directive declares a directory containing its own mmkfile that
the parent delegates to:

```bash
subproject src on $DOCKER_IMAGE
subproject preload_go on $DOCKER_IMAGE
```

At parse time, mmk reads the subproject's mmkfile, harvests its top-level
verbs (from explicit `[verb target]` rules, defbody verbs, and built-in
defbodies for the types it uses), and auto-generates corresponding top-level
rules:

- `<name>` (default-build) — `(cd <path> && mmk)`.
- `[<verb> <name>]` for each harvested verb — `(cd <path> && mmk <verb>)`.

So `mmk fmt src` becomes `(cd src && mmk fmt)`, wrapped in the runner clause
if `on <runner>` was specified. `mmk -list` shows the subproject's verbs as
if they were declared at the top level.

Sub-targets are addressable via slash syntax:

```
mmk fmt src/foo            # cd src && mmk fmt foo
mmk src/lib/util           # cd src && mmk lib/util (recursive subprojects)
```

Options:

- `path=<dir>` — directory to delegate to, if different from the target name.

Subprojects don't auto-include in `all`'s deps; list them explicitly:

```bash
all : src preload_go
subproject src on $DOCKER_IMAGE
subproject preload_go on $DOCKER_IMAGE
```

### Passthrough bash

Any line that is not an mmk directive is passed through verbatim to the
generated script. Use this for variable assignments, helper functions, or
any setup code:

```bash
CC=gcc
CFLAGS='-O2 -Wall -Wextra'
LDFLAGS="$CFLAGS -lm"

get_version() { git describe --tags --always; }

file prog : main.o {
    $CC $LDFLAGS -o prog main.o
}
```

Multi-line quoted strings are also passed through:

```bash
OBJS='main.o
    lexer.o
    parser.o'
```

## Types

Types are used to reuse behavior across multiple targets.

There are a few use cases. The primary use is controlling when artifacts are
built and rebuilt by telling mmk how to determine when an artifact is stale.

A secondary use is to define default rule bodies for a type and for various
verbs of that type.

A type defines how to determine when an artifact of the type was built, and 
mmk uses that information to compare the build time of a dependency artifact
against the build time of a dependent artifact, and determine whether the
dependent needs to be rebuilt.

The built-in types in mmk are defined in terms of the mmk language itself.
This is a powerful feature and gives the user the ability to define their own
types that are just as powerful as the builtins - or to redefine the built-in
types altogether.

Here is the `file` built-in type, for example.
```
deftype file {
	stat -c %Y "$target" 2>/dev/null || return 1
}

defbody file {
	[[ -e "$target" ]] && return 0
	printf 'mmk: %s does not exist and has no rule to create it\n' "$target" >&2; return 1
}

defbody file clean {
	rm -f "$target"
}
```

* The deftype defines the date the artifact was built (using stat)
* The `defbody file` defines the default rule to build a file - If the user does not define a body for a `file` type target, this function runs and fails if the file does not exist.
* The `defboxy file clean` defines the default body for the `clean` verb on the `file` type - `rm -f` the file.

See each of `deftype` and `defbody` for details.


### Built-in types

Three types are built in and need not be declared:

| Type     | Freshness check                        | Default body                             | Clean verb |
|----------|----------------------------------------|------------------------------------------|------------|
| `source` | `stat -c %Y "$target"` (mtime)         | error if file absent                     | none       |
| `file`   | `stat -c %Y "$target"` (mtime)         | error if file absent                     | `rm -f "$target"` |
| `image`  | `docker inspect {{.Created}}`          | `docker build -t "$target" -f …`         | none       |

**source vs file**: Both are mtime-based. `source` is inferred automatically
for any dep that has no rule and matches no pattern — it represents an
existing source file that mmk didn't create and should not delete. `file`
is for build artifacts: it gets a built-in `clean` verb.

Override any built-in by declaring your own `deftype` or `defbody` with the
same name. Print all built-in definitions with `mmk -builtins`.

### deftype — defining your own artifact types

A `deftype` defines a new type, and the body prints the artifact's timestamp
to stdout (epoch seconds or RFC3339). Non-zero exit means the artifact doesn't
exist.

```bash
deftype file {
    stat -c %Y "$target" 2>/dev/null || return 1
}
```

`$target` and `$deps` are available as variables. These will be populated with the
target rule's name and dependency list.

### defbody — default build body

A `defbody` provides the body used when a typed target has no explicit body:

```bash
defbody file {
    [[ -e "$target" ]] && return 0
    printf 'mmk: %s does not exist\n' "$target" >&2
    return 1
}
```

A `defbody` with a verb provides the default body for that verb on all
targets of the given type:

```bash
defbody file clean {
    rm -f "$target"
}
```

With this in place, every `file` target automatically gets a `clean` verb
that deletes it, with no per-target rule needed.


### Runners — executing inside a container

Use `on <image-target>` to run a target's body inside a Docker container.
The `image-target` must be a named target of type `image`. mmk starts the
container once (per image per build) and exec's each `on`-qualified target
body into it.

```bash
image myimage:latest : Dockerfile

file prog on myimage:latest : main.c {
    gcc -o prog main.c
}

shell on myimage:latest : prog {
    PS1='(build shell) $ ' bash -i
}
```

When `mmk` runs, it:
1. Builds `myimage:latest` via `docker build` (if stale).
2. Starts a container with `docker run ... sleep infinity` and bind-mounts
   the working directory and generated script into it.
3. Exec's each `on myimage:latest` target into that container.
4. Removes the container when the build finishes.

The `image` target's default body is `docker build -t "$target" -f "${deps%% *}" .`,
so the first dep should be the Dockerfile.

## CLI reference

```
mmk [-j N] [-v] [-dump] [-builtins] [[verb] target]
```

| Flag        | Description                                         |
|-------------|-----------------------------------------------------|
| `-j N`      | Parallelism (default: 0 = unlimited)                |
| `-v`        | Verbose: log each target as it runs or is skipped   |
| `-dump`     | Print the generated shell script and exit           |
| `-builtins` | Print built-in type definitions as mmk syntax       |

**Positional arguments:**

- No args: build `all`
- One arg: if it matches a known target, build it; otherwise treat it as a
  verb and run `<verb> all`
- Two args: `<verb> <target>`

## Full example

The following mmkfile builds a simple C project:

```bash
CC=gcc
CFLAGS='-ggdb -O0 -Wall'
LDFLAGS="$CFLAGS"
SRCS=$(ls *.c)
C_OBJ=$(ls *.c | sed 's/\.c$/.o/')

# by default, build prog
all : prog

# prog is a file output
file prog : $C_OBJ {
    $CC $LDFLAGS -o $target $C_OBJ
}

# .o files are built from their corresponding .c files.
file '(.*)\.o' : $1.c {
    $CC $CFLAGS -c $1.c -o $target
}

[check '(.*)\.o'] {
       echo Checking $target
}

run : prog {
    ./prog
}
```

```
mmk          # build prog
mmk clean    # delete prog (does not clean .o files — use mmk clean all : ... for that)
mmk run      # build and run
```
