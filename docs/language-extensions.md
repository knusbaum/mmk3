# Core language/runtime features

## Implemented: declared groups always get an aggregator (zero-member included)

**Status: done** (`cmd/mmk/runtime/runtime.go`, `createGroupAggregators`).

A consumer that depends on a group shouldn't need to know or care how many
producers currently register `into` it. That's the point of the
group/consumer contract: the group is the interface, membership is an
implementation detail the consumer shouldn't have to reason about.

Originally, `createGroupAggregators` only synthesized an aggregator target
for a group when it had at least one member; a group declared with zero
members had no target at all, so a plain dependency on it
(`thing : some_group`) failed with `some_group does not exist and has no
rule to create it`. This meant a group only worked once something used it —
brittle for the common case of an *optional* extension point.

Fixed: every `group NAME` directive now always gets a synthesized aggregator
target, with zero deps if it has zero members. Depending on an empty group
is simply a no-op.

This makes groups usable as **hook points** — declare a group, have some
number of your own targets (zero or more) depend on it, and let other rules
(possibly written later, possibly by someone else, possibly nonexistent)
optionally plug into it via `into`. See
[go-stdlib.md](go-stdlib.md#pre_build-hook-group) for the motivating case
(the `pre_build` hook that every generated `go_exe` target depends on
unconditionally).

## Existing mechanism: `include $(...)` for generated targets

Not a new feature — worth documenting because the stdlib design leans on it.
Include paths are `$`-expanded by running accumulated passthrough lines plus
`echo <path>` in a bash subprocess (`cmd/mmk/parse/include.go`,
`resolveIncludePath`). The expansion must be exactly one word. This means an
include path can be the *output* of an arbitrary command, not just a
variable substitution:

```bash
include $(some-command-that-prints-a-path)
```

`some-command` can do anything — scan the filesystem, shell out to another
tool, generate an `.mmk` fragment on the fly, write it somewhere, and print
that path. This is the mechanism [go-stdlib.md](go-stdlib.md#automatic-main-package-discovery)
uses to synthesize `go_exe` targets for every `main` package under the
current directory without the user writing anything.

Two properties fall out of how `mmk` resolves the current directory that
matter for this pattern:

- `mmk` never changes its working directory. `findMmkfile` only looks for
  `Mmkfile`/`mmkfile` in the process's cwd (`cmd/mmk/main.go`), and the
  include-path bash subprocess inherits that same cwd with no override. So
  "the directory the Mmkfile is in" and "process cwd" are the same thing,
  recursively through nested includes.
- This is also why dropping an `Mmkfile` in a subdirectory and running `mmk`
  from there just works — any cwd-scoped discovery command scopes correctly
  for free.

## Implemented: `defbody` dep clause

**Status: done** (`cmd/mmk/parse/parse.go`, `DefBody.Deps`;
`cmd/mmk/runtime/runtime.go`, `defBodyDeps`/`expandDefBodyDep`). This was
previously documented here as "proposed, not yet implemented" — that was
wrong; the mechanism already ships. Correcting the record, since it means
the C stdlib types ([c-stdlib.md](c-stdlib.md)) that express "my deps are
all `.o` files found by scanning a `source=` directory" are not blocked on
any new language work.

### What it does

A type like `c_library` can say "compute my deps from my options" via a dep
clause on its build-verb `defbody`, rather than making the user enumerate
every source file by hand.

### Syntax

```
defbody TYPE [opt=val ...] : depexpr ... { body }
```

The dep clause is only supported on the build-verb `defbody` (no `VERB`
token). A verb-specific `defbody TYPE VERB : depexpr { ... }` parses but is
rejected at runtime (`validateDirectives`) — verb deps inherit from the
build defbody via existing verb-rule semantics, so a second dep clause on a
verb would be ambiguous.

The dep expression is a bash `$(...)` expression evaluated at **graph
construction time**, per target instance, in an environment where:

- Per-target options (e.g. `$source`, `$recursive`) are set as bash
  variables.
- Passthrough vars (the generated bash script / genfile) are sourced.

The output is word-split into dep names, identical to how `$VAR` dep
expansion already works in `expandToken`.

### Semantics — four decisions

**DAG-level, not body-level.** Computed deps are real DAG edges. They appear
in `-graph`, the TUI, and `-dag`. Verb inheritance (`mmk clean`, `mmk test`)
propagates through them. If they were only visible inside the body (`$deps`),
`clean`/`test` would silently miss all the compiled objects.

**Augment, not replace.** Type-computed deps are appended to any explicit
deps the user wrote on the target rule. Neither side wins; the body sees all
deps combined in `${dep[@]}`:

```bash
c_library common.a source=./common :          # type-computed only
c_library linux.a  source=./linux  : extra.o  # explicit + type-computed both present
```

**Unconditional on body.** The dep clause fires for every target of the
type, regardless of whether the user wrote a custom body. Deps are a
property of the type, not of which body runs. This means:

- A user can override just the body without losing the type's dep
  computation.
- `mmk clean` correctly recurses into computed deps even when the build body
  is custom.

**Body fires only when no explicit body.** Standard `defbody` behavior is
unchanged on the body side. The dep clause is independent of this.

### Why options, not the dep list, for source dirs

```bash
c_library libio source=./libio :    # correct
c_library libio : ./libio           # wrong
```

The source directory is a *parameter* to the type, not a build dependency.
Passing it through the dep list is positionally fragile (what if there are
also real build deps, like a generated header?), and conflates parameters
with dependencies. Options are already the established mechanism for
per-target type config (`bucket=`, `skip_if=`, `user=`).

The dep list stays available for actual build deps:

```bash
c_library linux.a source=./linux : generated_header.h
```

### Implementation

`expandDefBodyDep` (`runtime.go`) evaluates the dep expression via the same
per-dep bash mechanism `expandToken` uses for `$VAR` dep expansion, with
per-target options additionally bound as bash vars.

## Implemented: `deftype ... into GROUP` (automatic group membership)

**Status: done** (`cmd/mmk/parse/parse.go`, `DefType.Groups`;
`cmd/mmk/runtime/runtime.go`, propagation in `NewBuildFromFile` right after
declared-group collection). Built alongside [tool-stdlib.md](tool-stdlib.md)
so `tool.mmk` could ship in one pass with automatic `tools` group membership
rather than requiring every `tool` target to write its own `into tools`.

### What it does

```
deftype TYPE into GROUP [into GROUP ...] { body }
```

Every concrete or matrix `TargetRule` of `TYPE` (not pattern rules, not verb
rules) is registered into each named group, merged with (not replacing) any
`into` clauses the rule itself declares. Each named group must still be
declared via a top-level `group` directive — a `deftype ... into` clause
naming an undeclared group is a build-time error at graph construction,
before any group expansion runs.

```bash
group tools

deftype tool into tools {
    p=$(which "$target" 2>/dev/null) || exit 1
    stat -c "%Y" "$p" 2>/dev/null || stat -f "%m" "$p" 2>/dev/null
}

tool controller-gen { go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.14.0 }
tool kind           { brew install kind }

setup : tools   # depends on controller-gen and kind, with no per-tool `into` clause
```

This composes with the zero-member-group fix above: a project with zero
`tool` targets still gets a valid (empty) `tools` aggregator, so `setup :
tools` never fails to resolve just because nothing has been declared yet.

## Implemented: matrix target names substitute their own loop variables

**Status: done** (`cmd/mmk/runtime/runtime.go`, `computeComboTargetNames`,
used by `expandMatrixRules` and `expandExplicitMatrixRules`). Built to make
[go-stdlib.md](go-stdlib.md#goosgoarch-cross-compile-matrices)'s GOOS/GOARCH
matrix actually work — the bug surfaced immediately on the first real test.

### The bug

A matrix rule generates one synthetic `TargetRule` per combo. Before this
fix, that synthetic rule's deps, runner, and options were all substituted
with the combo's variable bindings — but its *own target name* never was.
The base name (dollar signs and all) was instead wrapped in bracket
notation for uniqueness, e.g. `[bin/myapp-$goos-$goarch @ goarch=arm64
goos=darwin]`, and that literal bracket string — never substituted — became
`$target` inside the body. Fine for a phony matrix target that never reads
`$target` (the body reads the loop vars directly), broken for a path-valued
type like `go_exe`, whose body does `go build -o "$target"` and needs a
real filesystem path, not a string containing `@`, spaces, and brackets.

### The fix, and the constraint that shaped it

The obvious fix — substitute the matrix vars into the target name before
deciding on a DAG key — creates a new hazard: the target name (what deps
and `mmk <target>` on the CLI refer to) and the DAG key (the aggregator's
internal bookkeeping) could diverge, something that had never previously
been possible in this codebase. That divergence risk was flagged before
implementation, and the resolved design keeps a single namespace instead:

`computeComboTargetNames` substitutes the loop variables into the base name
for every combo in the set, then decides names for the *whole set as a
unit* — not per-combo:

- If every substituted name is pairwise-unique, **and** none collides with
  the literal (unsubstituted) base name the aggregator itself always keeps,
  every combo uses its substituted name directly. DAG key and `$target` are
  the same string; no bracket notation at all. This is the normal case for
  a base like `bin/myapp-$goos-$goarch` that embeds every loop variable.
- If any collision exists — including the degenerate single-combo case,
  where "unique among one item" would otherwise trivially hold while still
  colliding with the aggregator's own name — **every** combo in that rule
  falls back to bracket-notation naming, uniformly. A rule is never allowed
  to mix bare names on some combos and bracketed names on others; the
  decision is made once, for the whole matrix expansion, not combo-by-combo.
  This is what a base like `bin/myapp-$goos` (varying `goarch` without
  encoding it in the name) still gets today, same as before the fix.

`matrixRuleInfo` gained a parallel `names []string` field (index-aligned
with `combos`) so every other place that resolves a specific combo to its
DAG name — explicit `[target @ k=v]` combo-dep fan-out, group-member
registration — looks up the name that was actually decided for that combo,
instead of independently recomputing `comboTargetName` and risking
disagreement with what `computeComboTargetNames` chose.

## Implemented: later target rule wins on duplicate non-verb target

**Status: done** (`cmd/mmk/gen/gen.go`, `ValidateDuplicates`). Built for
[go-stdlib.md](go-stdlib.md#automatic-main-package-discovery): a hand-written
`go_exe` rule needs to be able to override one spliced in by
`include $(_mmk_go_mains)` with the same target name, the same way a project
already overrides a built-in `deftype`/`defbody`/`defrunner` just by
declaring its own.

Previously, `ValidateDuplicates` hard-errored (`duplicate target %q`) on
*any* repeated `(target, verb)` pair among concrete `TargetRule`s, verb
rules and plain rules alike. Testing the discovery feature surfaced this
immediately: writing a `go_exe bin/cmd/server ...` rule to customize
`ldflags` for an already-discovered binary failed the whole build with
`duplicate target "bin/cmd/server"`, even though the doc draft had assumed
"concrete rules just win."

The fix only relaxes the non-verb case. `runtime.go`'s registration loop
already stores concrete rules in a map keyed by target name
(`b.concretes[r.Target] = r`), built by a single forward pass over
`f.Directives` in file order — so once the early-return error for a
duplicate *plain* target is removed, "last declaration wins" falls out for
free with no other code change: whichever `TargetRule` for that name comes
later in the resolved directive list (the user's own line always comes
after anything spliced in via an `include` that precedes it) is the one
left in the map. Duplicate *verb* rules (`[clean foo]` declared twice) are
still a hard error — there's no legitimate reason to declare the same verb
rule for the same target twice, so this class is kept as a typo safety net.

An alternative considered and rejected: have the discovery script itself
scan the project's `Mmkfile` for names it's about to generate and skip
those. Rejected because it means re-implementing a chunk of mmk's own line
parser in bash (quoted vs. bare target names, verb rules, multi-line rules,
matrix/pattern rules, nested includes) with silent-failure risk if the
heuristic mis-detects, versus a two-line change in the one place duplicate
detection already lives.

## Pitfall: a bare `{` on its own line is a target-rule body opener, even inside a passthrough bash function

**Status: existing behavior, documented here because it bit
`_mmk_go_mains`** (the discovery function backing
[go-stdlib.md](go-stdlib.md#automatic-main-package-discovery)) **while
writing it.** `parseDirectiveOrPassthrough` (`cmd/mmk/parse/parse.go`) scans
every physical line independently, including lines inside an already-open
bash function body — the parser has no notion of "we're inside a `name() {
... }` block, treat everything until the matching `}` as passthrough." Only
the function's own declaration line (`name() {`) is special-cased via
`firstWordFollowedByParen`; every line after that is re-examined from
scratch by the same passthrough-vs-directive heuristic used everywhere else.

A line whose first non-whitespace character is `{` — e.g. `{ ... } >
"$file"`, ordinary bash grouping-for-redirection — hits
`lineHasDirectiveMarker`'s very first check and is unconditionally
committed as a target-rule body opener, with no target name before it,
producing `expected target name`. The fix is mechanical: never let bash
block-grouping syntax (`{ cmds; } > file`) start a line on its own inside
mmkfile passthrough bash. A subshell (`( cmds ) > file`) is safe instead —
`firstWordFollowedByParen` catches the bare `(` and treats the whole line
as passthrough — but simplest is usually to avoid multi-command grouping
+ redirection entirely and build output incrementally with `>`/`>>` per
line, which is what `_mmk_go_mains` does.

A related trap in the same function while it still used `local modpath
frag` (declaring multiple locals on one line): a two-bare-word passthrough
line followed *immediately* by a line starting with `{` triggers
`lineHasDirectiveMarker`'s next-non-blank-line lookahead (the "body on the
next line" syntax), misparsing the `local` line itself as a `[type]
target` header. Not `local` specifically — any two-bare-word line
immediately followed by a bare `{` line is at risk.

## Pitfall: typed rules need a `:` or `{` marker

**Status: existing behavior, documented here because it bit doc examples
in this repo.** `parseDirectiveOrPassthrough`'s "commit and error" heuristic
(`cmd/mmk/parse/parse.go`) decides whether a line is a target rule or raw
passthrough bash by scanning for a `:` or `{` on the line (or `{` at the
start of the next non-blank line). A rule with a type, target name, and
options but *no* dep list and *no* body — e.g. `go_exe bin/myapp
pkg=./cmd/myapp` or `c_library libcore.a source=./core` — has neither
marker, so it's silently treated as inert passthrough bash: no parse error,
no target registered, the line just does nothing.

The fix is always the same: add a trailing `:` (empty dep list) if the rule
has no deps of its own:

```bash
go_exe bin/myapp pkg=./cmd/myapp :
```

This is by design (see the "Parser ambiguities" section of
`CLAUDE.md`), not a bug — but it's an easy trap when writing terse
one-line type declarations (`tool`/`c_library`/`go_exe` style), since the
failure mode is silence rather than an error. Worth revisiting whether
`mmk` should warn (or `-dump` should flag) a passthrough line whose first
word matches a known type name, since that's almost always a mistake
rather than intentional bash.

## Proposed, not yet implemented: type docstrings + a discoverability command

**Status: design only.** `mmk -list` already distinguishes user-facing
targets from internal ones via a `##` docstring convention on target rules.
Types (`deftype`/`defbody`) have no equivalent: there's no way to attach a
docstring or a declared parameter list to a `deftype`, and no command that
dumps "every type available in this build — including ones pulled in via
`include` — its docstring, and the options it reads."

This matters most for the stdlib: a project that does `include go.mmk`
today has no way to ask mmk what `go_exe` is, what options it takes, or
what verbs it supports, short of reading `go.mmk`'s source (or this
`docs/` directory). Sketch of the shape:

```bash
## Builds a Go binary. The target name is the output path.
## Options: pkg= (default .), ldflags=, cgo= (default 0).
deftype go_exe {
    ...
}
```

```
$ mmk -types
go_exe       Builds a Go binary. The target name is the output path.
             Options: pkg= (default .), ldflags=, cgo= (default 0).
             Verbs: build (default), clean, test
tool         ...
```

Open questions, not yet resolved:

- Docstring syntax: reuse `##` (parsed today only for target rules), or
  something structured enough to extract an options table mechanically
  rather than free text?
- Whether `defbody TYPE VERB` needs its own docstring, or whether the verb
  list is inferred from which `defbody`s exist (simpler, but loses
  per-verb explanation).
- How deeply to walk `include` — probably flatten to "every type reachable
  from this Mmkfile," same scope as `-list`.
