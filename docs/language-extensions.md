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
`include $(_mmk_go_exes)` with the same target name, the same way a project
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
`_mmk_go_exes`** (the discovery function backing
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
line, which is what `_mmk_go_exes` does.

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

## Implemented: file-level project description (`##!`), printed by `mmk -list`

**Status: done** (`cmd/mmk/parse/parse.go`, `parseLeadingFileDoc`;
`cmd/mmk/runtime/runtime.go`, `Build.Description` / `PrintList`). Requested
by a downstream project using `mmk -list` as its primary front door: a
first-time user running `mmk -list` saw a bare list of target names with no
orientation — what the project is, prerequisites, naming conventions — even
though the project already maintained that text as a comment block at the
top of its mmkfile. Nobody who ran `-list` instead of opening the file ever
saw it.

The existing `##` docstring convention is per-rule (`TargetRule`,
`Subproject`, `Group` only — see `parser.pendingDoc` /
`consumePendingDoc`), and attaches to *whatever directive follows it*.
Overloading `##` for a file-level description would have meant a leading
`##` block either always steals the first target's docstring or never can
become a project description — no way to have both. So this uses a
separate marker, `##!`, recognized only in the run of comments/blank lines
at the very start of the file (`parseLeadingFileDoc`, called once before
`parseFile`'s main loop). A leading `##` block (no `!`) is completely
unaffected and keeps attaching to the first directive exactly as before —
the two markers never compete for the same comment block.

```bash
##! Stand up demo environments for the widget service.
##! Run `mmk -list -all` to see the underlying building blocks.

all : demo
```

`Build.Description` (populated from `parse.File.Description` in
`newBuildFromAST`) is printed by `PrintList` as a header before
`Targets:`, unconditionally — with or without `-all`, since project
orientation is exactly as useful either way.

Two scope decisions, both deliberately narrow for a first cut:

- **`include`d files' own `##!` blocks are discarded.** `resolveIncludes`
  only propagates `Description` from the root file it was called with
  (the one returned to `ParseFile`); an included file's `File.Description`
  is computed during its own recursive parse but never makes it into the
  splice, since only `.Directives` is merged. A project description is a
  property of "the mmkfile someone runs `mmk -list` against," not of every
  file that happens to get spliced into it.
- **Subproject descriptions are not surfaced.** `walkSubprojectTree`
  already re-parses each subproject's mmkfile as a `*parse.File` for
  `-list -all`'s target/verb harvesting, so a subproject's own `##!` block
  is technically available at that point — but `PrintList`'s subproject
  rows are single-line tabular entries, and a project description is
  designed to be multi-line prose. Threading it through would mean
  redesigning that part of the listing's layout, not just plumbing a
  string. Left as future work if a nested-subproject use case shows up.

## Implemented: type docstrings + a discoverability command

**Status: implemented.** `deftype` and `defbody` now accept the same `##`
docstring convention target rules already had. A `##` comment block
immediately preceding a `deftype` documents the type itself; one preceding
a `defbody TYPE` or `defbody TYPE VERB` documents that verb (a `defbody`
with no verb documents the type's default build behavior):

```bash
## Builds a Go binary. The target name is the output path.
deftype go_exe pkg= ldflags= cgo= {
    ...
}

## Removes the built binary.
defbody go_exe clean {
    rm -f "$target"
}
```

`mmk -types` lists every type reachable from the build — built-ins plus
every `deftype` declared in the Mmkfile, flattened through `include` (same
scope as `-list`) — with its docstring, declared options (sourced from the
type's structured `Options`, per the option-declaration feature above),
and its verbs with their own docstrings:

```
$ mmk -types
go_exe  Builds a Go binary. The target name is the output path.
        Options: cgo=, ldflags=, pkg=
        Verbs:
          build (default)
          clean — Removes the built binary.
          test — Runs `go test` on the exe's package.
```

Like `-list`, `-types` hides undocumented types by default; `-types -all`
shows everything, documented or not. `lib/go.mmk`, `lib/c.mmk`, and
`lib/cmake.mmk` now carry `##` docstrings on all their types and verbs, so
`include`-ing any of them makes their types self-describing via `-types`
without reading the source.

## Pitfall: CLI combo target specifiers require alphabetically-sorted keys

**Status: existing behavior, undocumented, worth fixing.** A dependency
on a specific matrix combo (`consumer : [build @ os=linux go=1.21]`) is
parsed structurally — the parser builds a `[]parse.Option` from the
bracket contents, and `runtime.go`'s dep resolution matches it against
each combo's `matrixCombo` map key-by-key (see the `constraints` map built
from `dep.Combo` around `runtime.go:1132-1136`), so key order in the
source is irrelevant.

Invoking the same combo directly from the command line —
`mmk '[build @ os=linux go=1.21]'` — takes a completely different path:
`Resolve`/`findRule` do a **raw string lookup** against `b.concretes` /
`b.nodes`, keyed by whatever `comboTargetName` produced when the combo was
registered. `comboTargetName` sorts keys alphabetically for determinism
(`runtime.go:2124-2151`), so the only string that will ever match is the
one with keys in sorted order. `mmk '[build @ os=linux go=1.21]'` fails
with `unknown verb "[build @ os=linux go=1.21]"` unless `go` happens to
sort before `os`; the working form is `mmk '[build @ go=1.21 os=linux]'`.
Nothing prints or hints at this — the error message looks identical to a
genuinely unknown target.

Fix: parse the CLI target argument the same way a dep-list combo
specifier is parsed (reuse the bracket/option parsing already in
`cmd/mmk/parse`), build a `matrixCombo` from it, and look up
`comboTargetName(base, combo)` instead of matching the raw string. This
makes CLI combo invocation key-order-independent for free, with no syntax
change and no effect on any other invocation path — a self-contained fix,
independent of whatever comes out of the proposal below.

## Implemented: `deftype`/`defbody` option declaration, with strict unknown-key rejection

**Status: implemented.** `deftype` now accepts the same trailing
`key=value ...` header tokens `defbody` already did:

```bash
deftype go_exe pkg= cgo=0 goos= goarch= ldflags= {
    ...
}
```

`DefType` gained an `Options []parse.Option` field, populated by the same
parsing loop `defbody` already used (interleaved with `into GROUP`, same
reserved-key check for `target`/`deps`/`MMK_*`). No new syntax — this is
exactly the syntax `defbody` already had, just also legal on `deftype`.

A type's full accepted-option vocabulary is the union of its
`DefType.Options` and every verb's `DefBody.Options` for that type (across
all verbs, not scoped per-verb) — plus the built-in `image` type's
`skip_if=`/`user=`, now declared the same way via
`gen.BuiltinDefBodyOptions`.

**This declaration is metadata only.** Declaring `pkg=` does not inject a
default or set `$pkg` for you — the body still reads `${pkg:-...}` itself,
exactly as before. There is no `!`/required-marker syntax and no
required-ness enforcement; that's out of scope for this change. The only
behavioral effect is validation:

- **Any `key=value` option set on a rule of a declared type, where `key`
  isn't in that type's accepted vocabulary, is a hard parse/validate-time
  error** — naming the unknown key and listing the type's known options.
- **A type that declares zero options still only accepts zero options.**
  There is no permissive fallback for types that haven't opted in to
  declaring anything; every typed rule is checked.
- Verb rules (`[verb target] key=value { ... }`) have no `Type` of their
  own — validation resolves the base concrete rule's `Type` to find the
  right vocabulary to check against.
- Two option keys are engine-level and exempt from every type's vocabulary,
  because they're genuinely type-agnostic rather than something any single
  type "owns": `order=` (runner-scheduling hint, already special-cased) and
  `tty=` (PTY allocation for `on <runner>` bodies). These are legal on any
  rule of any type, or no type, without being declared anywhere.
- Untyped rules (no `deftype` at all) are unaffected — this mechanism only
  applies to `deftype`/`defbody`-typed rules.

This is an intentional breaking change for any existing typed rule,
stdlib or user-authored, that was setting an option its type never
declared. The stdlib (`go.mmk`, `c.mmk`, `cmake.mmk`) has been updated to
declare every option its bodies actually read.

This closes the prerequisite gap the two proposals below were blocked on:
there is now a real, structured, per-type source of truth for "what
options does this type accept," recoverable from the AST without reading
body source.

## Proposed, not yet implemented: CLI-invocation option overrides for matrix dimensions and plain options

**Status: design only.** This previously waited on a structured, per-type
source of truth for "known option keys" — that groundwork has landed (see
"Implemented: `deftype`/`defbody` option declaration" above): every typed
rule's accepted vocabulary is now recoverable from the AST via
`DefType.Options`/`DefBody.Options`, and unknown keys are already rejected
at declaration time. What's proposed below is a separate, additive
surface — overriding an already-declared option's value from argv at
invocation time — not a new validation mechanism; the design is otherwise
unchanged from the original sketch.

Requested by a downstream project that stands up
the same environment in a handful of intentional variants — e.g. a service
brought up with caching on or off, with tracing on or off, or restricted
to a subset of a plugin list — and wants to pick a variant at invocation
time without it becoming a maintenance burden.

### What exists today, and where it falls short

- **Matrix targets** (`T for k in [...] for k2 in [...]`) are the correct
  primitive for orthogonal on/off-style knobs, but two things make them
  clunky for this use case once the sorted-key pitfall above is fixed:
  cross-products generate combos that don't make sense together and have
  to be pruned with `exclude`, and running the bare aggregator (`mmk T`)
  builds *every surviving combo*, which is never what an interactive user
  wants when they just meant "give me the default one."
- **Plain `key=value` rule options** (`T cache=off : ... { ... }`) are
  read inside the body as bash variables, but they're fixed at
  declaration time — there is no mechanism, CLI or otherwise, to override
  one at invocation (confirmed: no env-var convention, no flag, nothing in
  `main.go`'s arg handling touches rule options).
- **A knob whose value is "a subset of a list"** (e.g. which plugins are
  active) doesn't fit a matrix dimension at all without an explosion of
  combos for every subset a user might want — it's much more naturally a
  single option whose value is a comma/space-separated list, read and
  split inside the body.
- Environment variables (`CACHE=off mmk webapp`) work today without any
  mmk change, but are explicitly the thing being avoided: invisible in
  `-list`, global and easy to leave set by accident, and undiscoverable
  short of reading the mmkfile.

### Sketch

Extend CLI argument parsing to recognize trailing `KEY=VALUE` arguments
after the verb/target. This is unambiguous and backward-compatible: `=` is
already forbidden in target, verb, and runner names (see "Naming" in
`CLAUDE.md`), so any argument containing `=` can never have been a valid
target or verb — today it always falls into the `default: usage error`
arm of `main.go`'s `flag.NArg()` switch, so no existing invocation can
break.

```bash
mmk webapp cache=off tracing=on
```

Resolution semantics, unifying two cases so callers don't need to know
which one applies:

- If `key` matches one of the target's matrix dimensions, treat it as
  selecting (narrowing) a specific combo — sugar for
  `mmk '[webapp @ cache=off tracing=on]'`, minus the brackets and minus
  the sorted-key trap above once that's fixed. Under-specifying still
  fans out exactly like today's `[T @ k=v]` dep syntax does when some
  keys are unconstrained.
- If `key` matches a plain declared option instead, override that
  option's value for this invocation only — not cached, not a new named
  target, just a one-off bash-variable override for the body about to
  run. This is the mechanism a `plugins=` subset knob would use.
- **Scope is strictly the top-level invoked target — no cascading into
  dependencies.** An override is equivalent to writing that
  `key=value` directly on the target's own rule header (or selecting
  that combo directly), nothing more; it never reaches into a
  dependency's rule even if that dependency happens to declare the same
  key. Decided, not left open — see below.
- An unrecognized key is a hard error naming the target's known
  dimensions/options.
  - Matrix dimension keys come from the rule's `ForClauses` — mechanical
    today, no declaration mechanism needed.
  - Plain option keys: now also mechanical, via the type's declared
    vocabulary (`DefType.Options` ∪ every verb's `DefBody.Options`) — see
    "Implemented: `deftype`/`defbody` option declaration" above. The CLI
    override's own validation can reuse that same vocabulary rather than
    inventing a second source of truth.

### Why this satisfies the stated constraints

- **Keeps `-list` clean.** Overrides are argv, not declarations — no new
  target is registered, so `-list`'s menu doesn't grow with every
  combination a user might type.
- **Validity checking is mechanical for both key kinds.** Matrix
  dimension keys come from `ForClauses`; plain option keys come from the
  type's declared vocabulary (`DefType.Options`/`DefBody.Options`) — both
  answerable from the AST with no body-source reading required.
- **No env vars, no brackets** for the common interactive case, while the
  bracket form still exists underneath for scripting/dep-list use where
  precision matters more than brevity.

### Decided, not open

- **No multi-target invocation** (`mmk a b` meaning "build both"). This
  isn't legal syntax today, it doesn't help this proposal's actual use
  case, and a second argv-level way to do what a one-line aggregator
  (`all2 : a b`) already does is more confusion than it's worth. Out of
  scope, not a future addition.

### Open questions, not yet resolved

- Interaction with matrix `exclude`: if an override selects a combo that
  `exclude` has pruned, should that be "no such combo" (consistent with
  today's `[T @ k=v]` dep behavior) or a clearer override-specific error?
  Leaning toward reusing the existing error for consistency, but flagging
  it since the message would need to explain *why* the combo doesn't
  exist, not just that it doesn't.
- Once the type-docstring proposal above lands `mmk -types`, revisit this
  proposal's error messages to make sure the two designs' "known options"
  presentation actually compose (e.g. reuse whatever
  key/description/legal-values shape `-types` ends up defining, rather
  than inventing its own) — not blocking, since the underlying vocabulary
  is now shared regardless of how each surfaces it.
