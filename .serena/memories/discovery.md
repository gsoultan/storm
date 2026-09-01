# Model discovery replaces the bootstrap main (v0.3.0, 2026-08-27)

`cmd/storm` is no longer a stub. It parses the adopter's module, finds their
models, synthesizes the bootstrap `main` they used to write, `go run`s it, and
removes it. `go install github.com/gsoultan/storm/cmd/storm@latest` now works.

Decision and its reasoning: [ADR-0006](../../docs/adr/0006-discovery-replaces-the-bootstrap.md).

## The constraint that did NOT change

Field pointers (`t.Col(&u.Email)`) resolve **by field offset**, at runtime, on
a live value. The generator must therefore link against the models. Discovery
does not touch that — it answers one *static* question (which types are models,
and where) and everything semantic still happens by running the user's code.

Syntactic on purpose (`go/parser`, stdlib only). Type-checking with
`golang.org/x/tools` would add a dependency and fail whenever the adopter's
module does not compile — including the first run, before any store exists.

## The rule that was not obvious

**A type embedded in another struct is a mixin, not a model.** A mixin matches
every other rule a model matches: `internal/testmodel.Auditable` and
`SoftDelete` are exported, have `Schema` methods, and are not tables. Being
embedded is the only signal, and it is only knowable after the whole module is
read — so discovery is two passes, and the second one subtracts.

The first draft of the rule set was "embeds storm.Model", which is wrong twice:
it misses models with a natural key (embedding is optional, `types.go:44`;
`t.PrimaryKey(...)` declares the key inside `Schema`) and it says nothing about
mixins. Final rule set: embeds `storm.Model` · has `Schema`/`Plans`/
`Projections` taking the storm type · `//storm:model`; minus embedded types;
minus `//storm:ignore`; minus unexported (unreachable from another package, and
the usual cause is a mixin); minus `// Code generated ... DO NOT EDIT.` files
(otherwise the second `generate` discovers what the first one wrote).

## Things verified rather than assumed

- `go run ./.storm-bootstrap-<pid>` works: the go tool ignores dot-prefixed
  dirs for `./...` patterns but accepts an explicit path. So a crash that skips
  cleanup leaves something inert instead of a `package main` that breaks their
  build.
- Nested `internal` really does constrain shim placement — `foo/internal/model`
  is importable only from under `foo/`. `Result.ShimDir` computes the deepest
  common ancestor and reports the conflict by name when none exists.
- Discovery over storm's own repo reproduces all three hand-maintained
  registries exactly: `testmodel.All()` (8), `benchmodel.All()` (1),
  `blog/model.All()` (2), plus `TopPerOrg`.

## The one new failure, by construction

Nothing in an adopter's source imports `storm/tool` any more, so `go mod tidy`
cannot record its deps and the first run dies with go's generic "updates to
go.mod needed" — pointing at a file they did not write. `missingToolDep` in
`tool/bootstrap` detects that string and names the fix
(`go get github.com/gsoultan/storm/tool`), once per module.

## Gate

`scripts/check/outsider.sh` grew a **second stranger** with no bootstrap and no
registry. It exists because everything the first stranger exercises would keep
passing if discovery were broken — the tool it uses is one the test wrote. The
load-bearing assertion is that both paths generate **byte-identical** code.

See [[m6_first_adopter]] for the failure this closes at its cause, and
[[core]] for the P4 stranger test that found it.
