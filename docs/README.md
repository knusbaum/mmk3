# mmk design docs

This directory tracks design decisions for mmk itself and for the stdlib of
includable `.mmk` files shipped under `lib/`. It's a living set of documents —
update the relevant file as decisions change, rather than leaving history to
rot in a single monolithic doc.

- [language-extensions.md](language-extensions.md) — core mmk language/runtime
  features: implemented ones worth recording the rationale for, and proposed
  ones not yet built.
- [tool-stdlib.md](tool-stdlib.md) — `tool`, a generic type for "ensure some
  versioned CLI is available," independent of any particular language.
- [go-stdlib.md](go-stdlib.md) — `go.mmk`: wrapping the Go toolchain, plus
  planned extensions (version injection, cross-compile matrices, automatic
  discovery of `main` packages).
- [c-stdlib.md](c-stdlib.md) — `c.mmk`: types for C libraries, shared
  libraries, and executables.
- [cmake-stdlib.md](cmake-stdlib.md) — `cmake.mmk`: wrapping cmake-based
  subprojects and fetching external sources at build time.
- [case-study.md](case-study.md) — a worked example showing how a
  real-shaped (but disguised) mixed C/Go project with cross-compilation
  restructures around the stdlib types above, plus the sequenced plan for
  carrying out a migration like it safely.

## Motivation

Ship a set of includable `.mmk` files alongside mmk so users get correct,
parallel builds for common languages and patterns without writing
boilerplate. A project should be able to say:

```bash
include go.mmk
go_exe bin/myapp pkg=./cmd/myapp :
```

and have `mmk`, `mmk test`, `mmk fmt`, `mmk clean`, `mmk update` all work
correctly — and ideally not even need the `go_exe` line, if `myapp`'s `main`
package can be discovered automatically.

The design throughout is grounded in real project shapes (mixed-language
builds, cross-compilation matrices, generated code, pinned local tooling),
not toy examples — see [case-study.md](case-study.md).
