# ADR-0006 — Models are discovered, not registered

**Status:** Accepted · 2026-08-27

## Context

Until now, adopting storm meant writing a file storm told you to write:

```go
package main

import (
	"github.com/gsoultan/storm/tool"
	"example.com/app/model"
)

func main() { tool.Main(model.All(), model.Queries()) }
```

Plus the `model.All()` registry it reads, kept in step with the model package
by hand.

The requirement behind it is real and does not go away. storm resolves field
pointers — `t.Col(&u.Email)` — **by field offset**, which is a runtime
operation on a live value. The generator therefore has to *link against* the
models rather than read them, and a binary installed from storm's repository
cannot link against code it has never seen. `cmd/storm` was a stub that existed
only to say so.

The cost of that was not the five lines. It was that the tool became something
each adopter re-derived:

- The first adopter (M6) hand-rolled a `main` around `codegen.Package` and
  therefore never got `verify -pending`, `lint` or `explain` **at all** —
  recorded in `docs/PRODUCTION-READINESS.md` and `.serena/memories/m6_first_adopter.md`.
  The generalisable finding there is the one that matters: *an adopter who has
  to re-implement your tool is telling you the tool is unreachable.*
- `model.All()` is a second list of the same facts, and the failure mode of
  forgetting an entry is a silently missing table.
- `go install github.com/gsoultan/storm/cmd/storm@latest` — the thing every Go
  developer tries first — produced a binary whose only function was to reject
  them.

## Decision

**The installed binary discovers the models and writes the bootstrap itself.**

`storm generate` now:

1. finds the module root, and parses it with `go/parser` to answer one static
   question — which types are models and where do they live;
2. synthesizes the same bootstrap main that used to be hand-written;
3. `go run`s it, forwarding argv verbatim;
4. removes it.

Field pointers still resolve by offset at runtime, in the adopter's own
process, exactly as before. **Nothing semantic moved into the parser.**

A type is a model when it embeds `storm.Model`, or has a `Schema`, `Plans` or
`Projections` method taking the corresponding storm type, or carries
`//storm:model`. `//storm:ignore` opts out.

**A type that is embedded in another struct is a mixin, not a model.** This is
load-bearing and was not obvious: a mixin matches every other rule a model
matches — `internal/testmodel` has `Auditable` and `SoftDelete`, both exported,
both with `Schema` methods, neither a table. Being embedded is the only thing
that distinguishes them, and it can only be known after the whole module is
read, so discovery is two passes.

**Discovery is syntactic, not type-checked.** Loading the adopter's module with
`go/packages` would mean depending on `golang.org/x/tools` and failing whenever
their code does not compile — including the first run, before any store exists
to compile against. The parser answers one question, and if it answers it
wrongly the failure is a compile error in the synthesized file, not bad SQL.

**The hand-written bootstrap keeps working, unchanged and supported.**
`tool.Main(model.All(), model.Queries())` is not deprecated. Discovery is an
addition; a module that has a bootstrap is left alone. The two paths are
asserted to generate byte-identical output in `scripts/check/outsider.sh`.

## Consequences

**Good.** `go install ...@latest` works. Adopter setup is: write structs, run
`storm generate`. `verify`, `lint`, `explain`, `diff` and `import` become
reachable for every adopter by default rather than for those who wired a
bootstrap — which is the M6 failure, closed at its cause. `model.All()` stops
being a second source of truth that can silently disagree with the first.

**Bad.** storm now owns a source-analysis front end. It is new surface that has
to stay correct across build tags, generics, dotted and aliased imports, and
whatever Go adds next. The scope line in `docs/CONCEPT.md` says *front end → IR
→ back end*, and this is a fourth thing.

**Mitigations.** It lives in one package (`tool/discover`) with its own fixture
corpus under `testdata`, it answers exactly one question, and its output is
golden-tested as whole files. `storm models` prints what it concluded and why,
so a surprise is one command away from an explanation rather than an
archaeology exercise. Nothing under `schema/`, `query/`, `compile/`, `codegen/`
or `runtime/` imports it, so the stdlib-only boundary is untouched.

**One new failure, by construction.** Nothing in an adopter's source imports
`storm/tool` any more, so `go mod tidy` cannot record its dependencies and the
first run fails with go's generic *"updates to go.mod needed"*. storm detects
that specific failure and names the fix (`go get github.com/gsoultan/storm/tool`),
which is a one-time step per module.

**Reversible?** Entirely. `cmd/storm` is a thin shim over two packages nothing
else imports; deleting it restores the stub and the documented bootstrap, which
never stopped working.
