# Adaptive memory control

GoTransit derives its memory policy from the running network and machine. It
does not use an operator count, graph-size preset or country-specific RAM
threshold.

Every 30 seconds the controller samples the live and reclaimable Go heap,
allocation rate, recent GC CPU share, cgroup or host memory pressure, and the
measured cost of its previous release. Graph and timetable rebuilds are marked
as busy so reclamation never competes with a snapshot swap.

The release threshold is the larger of 1/32 of the live heap and the bytes
likely to be reused over the next two seconds. Its proportional component is
bounded between 1 MiB and 512 MiB, allowing both small city networks and very
large regional graphs to return useful amounts. Memory pressure lowers the
threshold to 1 MiB.

Release cooldown derives from the previous operation's duration and targets
0.1% background wall time, bounded to 1–30 minutes. Pressure shortens it to 30
seconds. A collection observed during high allocation remains pending until
reuse slows.

When `GOGC` is not explicitly configured, its target adjusts gradually. GC
below 0.5% of available CPU reduces heap headroom; GC above 2% increases it.
The target remains between 10 and 200. Explicit `GOGC` and `GOMEMLIMIT`
settings always win. Without a memory-limit override, a finite cgroup limit
becomes a Go soft limit with 10% reserved for non-heap process memory.

Reusable NearSearch, RoadSearch and RAPTOR states account their real slice
capacities. Each cache keeps only entries fitting 1/32 of the live heap and
never more than GOMAXPROCS; one hot entry survives while memory is healthy.
Pressure drops every idle entry.

With `[cache]`, a replication-capable OSM source becomes the restart/update
image and remote GTFS ZIPs remain file-backed. The larger PBF is removed after
a successful image write. Sources without supported live updates retain the
revalidatable PBF instead of an extra source copy. Without `[cache]`, the
updateable source stays in RAM and the engine remains fully ephemeral.

Routing arrays, algorithms and response semantics are unchanged. Policy tests
cover small and 100 GiB-class heaps, allocation churn, pressure, cooldown cost,
GOGC hysteresis and byte-scaled cache budgets. Streaming source tests require
the file and in-memory encodings to be byte-identical.
