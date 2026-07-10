# GoTransit — architecture

Everything below was chosen to serve two constraints: **tiny at runtime**
(RAM allowed only at build time) and **updatable at runtime** (no rebuild
from scratch, no downtime, no restarts). When those two conflicted with
textbook choices, the textbook lost.

## Package map

```
cmd/gotransit        boot (ephemeral: temp PBF → build → delete), signals
internal/toml        TOML subset parser (zero deps, line-numbered errors)
internal/config      one-file config, defaults for everything
internal/geo         E7 fixed-point math, encoded polylines
internal/osm         PBF decoder (hand-written protobuf), osc (osmChange), state.txt
internal/graph       street graph: build, CSR arrays, source store, searches, turns
internal/gtfs        zip + RFC-4180 CSV (parallel fast path), feed model
internal/transit     timetable compile, transfers on the street net, RAPTOR
internal/engine      atomic snapshots (graph/timetable bundles + search pools), planner
internal/api         HTTP JSON + embedded map debug UI (/)
internal/updater     GTFS conditional-GET poller, Geofabrik osc poller (all in-RAM)
```

Data flows one way: `osm/gtfs → graph/transit (immutable snapshots) → engine
(atomic pointers) → api`. Updates construct new snapshots off to the side and
swap a pointer; readers never lock.

## Units

- Coordinates: **E7 fixed-point int32** (OSM native precision, 8 B/point).
- Road costs: **deciseconds uint32**; bounded searches use uint16 (≤ 1.8 h).
- Transit times: **seconds since service-day midnight uint32**, values > 86400
  are the GTFS convention for after-midnight trips of the previous day.
- Speed factors: 16.16 fixed point of `36/kmh`, so `ds = meters * f >> 16`.

## Street graph (`internal/graph`)

**Build** (once per import, twice-scanned PBF):

1. *Ways pass* — parallel PBF block decode; `classifyWay` maps
   highway/access/oneway/maxspeed tags into per-direction mode flags
   (`car|bike|foot`) + car speed. Kept ways: id, refs, flags, name.
2. Node id resolution: sort all refs (~20 M int64), unique them; a node is a
   *routing node* if referenced ≥2 times or a way endpoint; everything else is
   edge geometry.
3. *Nodes pass* — coordinates for referenced nodes only (binary search into
   the sorted unique ids; no giant hashmaps).
4. *Assemble* — walk each way, cut chains at routing nodes (and at extract
   holes), emit undirected staged edges, then a directed **CSR**:
   `FirstEdge[u] .. FirstEdge[u+1]` → `EdgeTarget/Meters(u16)/Flags(u8)/
   Speed(u8)/Name(u32)/GeomOff(u32)`. Intermediate geometry is
   zigzag-delta-varint bytes in one blob, stored once per undirected pair
   (the reverse edge sets a `GeomRev` flag). Per-mode union-find components
   mark snap-safe nodes (kills parking-lot islands — a Duomo ZTL alley almost
   swallowed Florence). A uniform grid (0.003°, CSR cells → canonical edge
   ids) serves point snapping.

centro Italia: 380 MB PBF → 1.78 M nodes, 4.44 M directed edges, 44 MB
geometry blob, ~8 s cold.

**Source store** (92 MB blob): ways+refs+ids+coords+names, varint delta
encoded, flate(1). It lives **in RAM** — the temp PBF is deleted right after
parsing and the engine never writes a file. Live diffs decode this blob,
fold changes in, re-encode. Local PBFs are read in place and never touched.

**Searches** — all allocation-free on pooled, epoch-stamped state:

- `NearSearch`: bounded multi-source Dijkstra (uint16 ds). Powers stop
  access/egress, transfer precompute, transfer-leg reconstruction.
- `RoadSearch`: point-to-point **weighted A\*** (binary heap on packed
  uint64, geometric lower-bound heuristic × ε). ε=1.2 car / 1.1 bike / 1.05
  walk in "fast" mode; ε=1 is provably optimal. **Deliberately not CH/ALT**:
  contraction and landmarks are preprocessing that daily osc diffs would
  invalidate; plain A* works on the live arrays and hits the latency budget
  (Roma→Firenze 275 km: 29 ms core).
- Turn-by-turn: name/roundabout grouping + bearing-delta modifiers over the
  path edges.

## Timetable (`internal/transit`)

GTFS feeds are merged with per-feed offsets (stops keep feed-qualified public
ids, `roma:70431`). Trips collapse into **patterns** (route + exact stop
sequence); trips sort by first departure inside each pattern; times live in
two flat `Arr/Dep` uint32 arrays. Stop→(pattern, position) adjacency is CSR.
Calendars unify `calendar` + `calendar_dates`; per-date service bitsets are
built lazily and cached.

**Transfers** are real street-network walks: every stop is snapped to the
foot graph (two anchor nodes + partial-edge costs), then a bounded Dijkstra
per stop harvests neighbors within `transfer_radius_m` (400 m default) —
21 k stops in ~1 s parallel. The same anchor index answers query-time access
searches (`NSNode/NSStop/NSExtra`).

**Coverage guard**: trips whose stops or shape leave the extract bbox
(+2 km margin) are dropped at compile with a per-route report — logged at
startup, exposed in `/v1/status`. No routing on data the graph can't back.

**RAPTOR** (`raptor.go`): classic round-based scan with route queue, local +
target pruning, and per-round parent labels for reconstruction. Two
service-day layers per query (yesterday shifted −86400 s, today) make
after-midnight trips board correctly — departures shifted before "now" are
compared in int64, never wrapped (that bug bit once; there's a regression
test). Pareto extraction per ride count. The hot path reads times through an
atomic `RTOverlay` pointer (per-trip delay/cancel) — the GTFS-RT hook is
already wired, just unfed.

Roma+COTRAL: 20 946 stops, 3 742 patterns, 242 928 trips, 7.45 M stop_times,
83 502 transfers — compiled in ~0.9 s, ~5.7 ms per query.

## Planner (`internal/engine`)

`GraphBundle`/`TTBundle` pair each snapshot with `sync.Pool`s of search state
sized for it, behind `atomic.Pointer`s. A query pins both bundles once; a
swap mid-query is invisible.

Transit planning: snap origin/destination → bounded street searches produce
stop seed sets (walk always; bike variants for `bike_transit` capped at
`max_bike_access`) → RAPTOR → assembly. Assembly rebuilds real street paths
for access/egress/transfer legs (bounded searches, so ~ms), slices GTFS
shapes between the projected per-stop shape indices for ride polylines and
distances, and attaches turn-by-turn steps to every street leg.

`bike_transit` realism: variants (bike/walk, walk/bike) must beat the classic
walk plan by `bike_transit_min_saving`; the direct-bike itinerary is included
when competitive (≤ 45 min and not clearly worse) so the comparison is honest.

Arrive-by: forward RAPTOR is a few ms, so the latest feasible departure is a
bracketed binary search over departure time (~1 min resolution), then a final
forward plan filtered to the deadline.

## Updates (`internal/updater`)

- **GTFS loop** (per feed, default every minute): ETag/`If-Modified-Since`
  conditional GET + SHA-256 guard (servers that ignore conditionals get a
  one-time warning). Remote zips live in RAM; local feeds are stat'ed and
  reloaded when their mtime changes. On change: re-parse all feeds, recompile
  the timetable against the *current* graph, swap. The street graph is
  untouched, ever.
- **OSM loop** (Geofabrik only): the extract's replication sequence and diff
  URL come from the PBF header itself (so a *local* Geofabrik extract updates
  too). Poll `state.txt`; for each pending daily `.osc.gz`: parse
  (create/modify/delete of nodes/ways), fold into the in-RAM source blob
  (`ApplyChange` → fresh `SrcData`, old one untouched for safety),
  reassemble, swap, re-encode the blob, then recompile the timetable (stop
  snaps/transfers reference graph node ids). Measured with a real diff:
  ~5 s total. Non-Geofabrik sources get a loud startup warning: no live
  updates.

All state (etags, hashes, replication seq) is in memory: a restart simply
re-downloads and rebuilds — download, parse, destroy, poll.

## GTFS-Realtime (`internal/rt`, `internal/track`)

The RT protobuf reader is hand-written like the PBF one (FeedMessage →
TripUpdate/VehiclePosition, ~300 lines, plus an encoder used by the E2E
harness). One poller per operator (20 s default, SHA-256 change detection);
on change the manager projects **all** feeds onto the current timetable as an
immutable `RTOverlay` — per-trip-per-stop second deltas (GTFS-RT propagation
semantics: a delay holds until the next explicit update), cancellations,
SKIPPED stops, and `Passed[trip]` = the highest stop position the vehicle has
confirmedly cleared (from past STU times and `current_stop_sequence`).
Mixed feeds (vehicles inside a trip-updates URL) are honored. The overlay is
swapped atomically and re-projected after every timetable swap; a version
bump fans out to tracking sessions over a broadcast channel.

RAPTOR reads times *through* the overlay, so plans are RT-adjusted by
construction; boarding/alighting at SKIPPED stops and riding cancelled trips
are impossible. Legs carry `realtime` + `delay_s`; the `live=true` filter
implements the strict rule: first transit leg RT-confirmed and ≤45 min out,
everything in the next hour RT-covered.

**Tracking** (`/v1/track`, WebSocket — RFC 6455 hand-rolled, ~180 lines):
the overlay also carries per-trip vehicle state (position, current pattern
stop, status) so sessions stream `vehicle` events — where your bus is, how
many stops away, its delay — before and during the ride, deduped on change.
no GPS, only clock + feeds. The virtual user walks legs by duration, is
considered on board once `Passed ≥ boarding position` ("se il GTFS-RT dice
che è passato, ci fidiamo"), alights the same way. Every RT change (or 5 s
tick) re-evaluates: feasibility (missed connections — including buses running
*early* — cancellations, skipped stops) forces a replan; opportunistic
replans fire only when they beat the current arrival by
`reroute_min_saving` (5 min default, 60 s cooldown). Onboard replans seed
RAPTOR with **every downstream stop** of the current vehicle at its RT
arrival time, so "get off two stops early and switch" falls out of the same
search. Schedule-only boardings within `rt_confirm_lead` get a warning and
(in live mode) a reroute onto confirmed service; a trip past its terminus
departure with zero RT trace is flagged `possibly_cancelled` before the
operator's late CANCELED arrives (`cancel_blind`). On non-live itineraries
reroutes only ever touch realtime legs.

The E2E test drives the full loop against a fake evolving RT server:
delay → better-arrival reroute → cancellation reroute → vehicle-confirmed
boarding → early alight → arrival, asserted event by event.

## Known limits (deliberate v1 cuts)

- No OSM turn restrictions / elevation yet (roadmap).
- Weighted A\* "fast" mode trades ≤ ~ε−1 optimality for speed; `"exact"` is
  one config line away.
- osc ways referencing pre-existing nodes that no highway ever referenced
  before fall back to chain-cutting until the next full import (rare;
  self-heals on re-import).
- Arrive-by runs several RAPTOR passes (~130 ms); a reverse RAPTOR would do
  one.
