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
suppress inheritance:

```bash
# Inherits deps from 'prog' rule, runs clean on each
[clean prog]

# Explicit empty list — no deps, runs in isolation
[clean prog] : {
    rm -f prog
}
```

### Variable expansion in deps

Variables defined in passthrough bash are available in dep lists:

```bash
SRCS=$(ls *.c)
OBJS=$(echo $SRCS | sed 's/\.c/.o/g')

file prog : $OBJS {
    gcc -o prog $OBJS
}
```

Word-splitting applies: a variable holding `foo.o bar.o` expands to two
deps. Only tokens that start with `$` are expanded; literal names are
used as-is.

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
