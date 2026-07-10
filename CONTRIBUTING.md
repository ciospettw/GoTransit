# Contributing to GoTransit

Glad you're here. This project has a strong personality — reading this page
first will save your PR a round-trip. 🚋

## The house rules

1. **Zero dependencies.** The `go.mod` has no `require` block and it stays
   that way. Protobuf, WebSocket, TOML, polylines — everything is hand-rolled
   on the stdlib. If your feature "needs" a library, it needs a smaller
   design instead. (`_ "time/tzdata"` is the one blessed import.)
2. **Nothing touches the disk at runtime.** Remote data is downloaded,
   parsed, destroyed. What must survive lives compressed in RAM. Local file
   sources are read in place and never modified. If your change writes a
   file, it's wrong.
3. **Updates never block queries.** All live state is immutable snapshots
   behind `atomic.Pointer`. Build the new thing off to the side, swap, let
   the GC collect the old one. No locks on the query path — ever.
4. **The engine never leaves the user alone.** Anything that can invalidate
   a journey (delay, cancellation, missed connection, skipped stop) must
   produce an event and, when the plan is broken, a replacement plan. Silent
   degradation is a bug.
5. **Measure, don't vibe.** Query latency and RSS are features. If a PR
   touches a hot path, include before/after numbers in the description
   (`X-Query-Ms` and `/v1/status` heap make this easy).
6. **Fail loudly at the edges, never at runtime.** Bad GTFS rows, shapes
   outside the graph, servers that ignore ETags: count them, log them once,
   keep serving.

## Getting started

```bash
go build ./cmd/gotransit
./gotransit init && ./gotransit     # boots against real Italian feeds
go test ./...                       # full suite, E2E included (~2 min)
go test -short ./...                # skip the wall-clock E2E
```

For the real-data suite, drop captures in a directory and point
`GOTRANSIT_TEST_DATA` at it (see [`tests/realdata_test.go`](tests/realdata_test.go)
for the expected file names). Nothing in CI needs it — it's for local deep
verification.

## Tests

All tests live in [`tests/`](tests/), black-box against exported APIs.
If you need to test something unexported, that's usually a hint the API is
missing a seam — prefer exposing a small, honest surface over white-box
poking. New features come with tests; realtime features come with a scenario
in the E2E harness (`tests/harness_test.go` has the synthetic world and the
mutable GTFS-RT server — fabricating a delay or a cancellation is three
lines).

## Style

- `gofmt` is law; `go vet ./...` must be clean (CI checks both).
- Comments explain *constraints*, not narration. If the code can say it,
  the comment shouldn't.
- Flat arrays over pointer graphs, indices over references, deciseconds and
  E7 coordinates over floats — see [ARCHITECTURE.md](ARCHITECTURE.md) for
  the units table before inventing a new one.

## PRs

- One concern per PR. Small is fast.
- Describe the *user-visible* behavior change first, the implementation
  second.
- Breaking the config schema is fine pre-1.0, but call it out in bold.

## Bugs

Open an issue with: your `gotransit.toml` (redact URLs if private), the
startup log, and — for routing weirdness — the full `/v1/plan` URL that
misbehaves. The debug UI at `/` usually turns "it feels wrong" into a
screenshot with coordinates.
