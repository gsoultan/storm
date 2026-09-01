---
tags: [storm, example, gokit, microservice]
updated: 2026-08-27
---

# orders — a Go kit service on storm

An order-fulfilment service: a catalogue, checkout that **reserves stock under
concurrency**, order retrieval, and a finance report. It is a separate Go
module with its own `go.mod`, so it is a genuine outsider — nothing here shares
storm's assumptions, and go-kit never enters storm's dependency graph.

It is also the whole adopter path with **nothing to bootstrap**: there is no
`cmd/storm`, no `model.All()`. `storm generate` finds the models by parsing.

## Run it

```console
$ make db                                    # or any Postgres
$ export STORM_DSN='postgres://storm:storm@127.0.0.1:5433/storm'

$ go install github.com/gsoultan/storm/cmd/storm@latest
$ go get github.com/gsoultan/storm/tool      # once per module

$ storm models                               # what discovery concluded
$ storm generate store                       # 6 packages from 5 tables
$ go test ./orders/                          # the proof, against real Postgres
$ go run ./cmd/ordersd                       # :8080
```

## The business case

| Concern | How it is handled | Why it is the interesting part |
|---|---|---|
| **Overselling** | `StockItem.Version` + `t.Col(&s.Version).Version()` | Two checkouts racing for the last unit cannot both win. The loser gets `runtime.ErrStaleWrite`, mapped to **409 with `retryable: true`** — it lost a race, it did not send something wrong. |
| **Money** | `storm.Decimal` → `numeric(12,2)` | 2 × 19.99 + 5.25 is `45.23`, not `45.229999999999997`. A float64 cannot represent 0.10; an accounting system that rounds is a defect, not a tolerance. |
| **Atomicity** | one `pgx` transaction; `store.NewUnit()` for the graph write | A line that fails leaves *no* reservation behind from the lines before it. `TestPartialFailureReservesNothing` asserts exactly that. |
| **N+1** | `store.OrderWithLines()` | An order and its lines in **two round trips**, whatever the line count — asserted with `runtime.CountingExecutor`. Reading `.Lines` off a plain `order.Row` does not compile. |
| **List endpoints** | `Projections` → `AllCard` | Three columns instead of the row, a covering index away from an index-only scan. |
| **Reporting** | `storm.SQL[DailyRevenueRow]` | Ordinary SQL, verbatim — validated against the model at *generate* time and given a generated scanner. |
| **Layering** | Go kit service → endpoint → transport | `orders/service.go` is the only file that imports storm. HTTP handlers never see a row type; the persistence choice cannot leak upward. |

## What the tests prove

```
TestPlaceAndReadOrder                    happy path, exact money
TestOrderDetailCostsTwoRoundTrips        the plan's claim, counted
TestOutOfStockIsRefused                  no oversell, nothing reserved on failure
TestPartialFailureReservesNothing        the transaction actually rolls back
TestConcurrentReservationsDoNotOversell  12 goroutines, 20 units, 2 per order
TestCatalogueUsesTheProjection           the narrow read
TestDailyRevenueReport                   the declared raw query, for real
```

The concurrency test is the one worth reading. Twelve goroutines each try to
reserve 2 units from a stock of 20:

```
4/12 orders won, 8 lost the race, 8/20 units reserved — nothing oversold
```

Some lose — that is the point. The invariant asserted is `reserved <= on_hand`,
plus `reserved == wins × 2`, so a bug that let two writers both commit would
fail loudly rather than showing up in a stock reconciliation months later.

## Two storm bugs this example found

Being the first outsider to use two ordinary features turned up two real
defects, both now fixed with regression tests in storm:

1. **Every predicate on a `bool` column failed.** `bool` shared the `int64`
   value arena, so it reached pgx as `*int64`, which has no encode plan for
   OID 16. No fixture in storm's repository had a bool column, so nothing
   caught it. Fixed by giving `bool` its own arena — the same fix
   `TimeOfDay` had already needed for the same reason.

2. **A declared plan could silently collide with the free one.** Every relation
   already gets a plan, and `Order.Lines` generates `OrderWithLines`. Declaring
   `p.Named("WithLines")` generated the *same type name twice*, and the failure
   landed in the adopter's build. `storm generate` now refuses, names the
   conflict, and says to delete the declaration.

That is why this example declares **no** `Plans` method: `store.OrderWithLines()`
already exists. Declared plans are for *combinations* the automatic tier does
not cover, and one relation is not a combination.

## Layout

```
model/model.go       5 models + 1 mixin. No tags, no DSL, no All().
model/queries.go     storm.SQL[T] / storm.SQLExec declarations.
store/               GENERATED — `storm generate store`. Commit it.
orders/service.go    the business layer; the only file that imports storm
orders/endpoints.go  go-kit endpoints
orders/transport.go  HTTP + JSON, and the error→status mapping
orders/middleware.go the logging decorator
cmd/ordersd/         wiring
```

`model.Audited` is a **mixin**: exported, with a `Schema` method, embedded into
`StockItem` and `Order`. It looks exactly like a model. storm classifies it as a
mixin because something embeds it — run `storm models` to see it reported as
skipped, with the reason.
