# Live journey continuity

A live replacement retains the occupied trip as its first transit leg, marked
`boarded: true`. Its shape and stop list end at the new alighting stop. A
`riding` progress event follows the reroute immediately; cached replacements
also preserve this state when reconnecting. The tracker only considers
alighting stops at least two positions beyond the rider's known progress,
including GPS progress ahead of stale vehicle updates. If no safe replacement
exists, it keeps the current ride and retries. A walking alternative starts at
a reachable downstream stop, never at a moving rider's GPS position.

Entering a 30-metre circle around the next boarding stop immediately ends all
remaining access legs. The stop stays the routing origin despite GPS jitter.
Changing the chosen bus at that stop does not add another access walk. A
confirmed departure from the stop can release the lock through the existing
left-stop guard. Rail station/platform readiness buffers remain in force.

After reaching a stop, at least two ordered movements along the planned shape,
lasting at least five seconds and reaching 100 metres from the stop, confirm
boarding without a fresh TripUpdate or VehiclePosition. Reverse movement,
leaving the shape corridor, and never having reached the stop do not qualify.

Planning and live evaluation share the same connection model. With default
configuration, the actual walk is followed by 60 seconds of boarding allowance
for initial access, or 90 seconds for a transfer. Already waiting at a boarding
stop needs no access allowance. Reroute candidates require at least 0.8 model
probability on every unboarded transit leg, even when ordinary risk ranking is
disabled. These are model estimates, not calibrated success rates.

Both the incoming vehicle (including the occupied bus) and the outgoing one
contribute timing uncertainty. In addition to learned delay distributions or
analytic fallbacks, each gets a small forecast term:

`min(30, 0.025 × remaining_seconds) + min(15, 0.003 × distance_metres)`

Negative inputs clamp to zero. Distance comes from VehiclePosition, or its
reported stop when coordinates are absent; the time term still works without
vehicle data. Forecast terms combine as independent variance, so physical
walking time and the transfer allowance dominate. The existing three-minute
reroute cooldown also applies to soft unconfirmed-trip replacements, and
reselecting the same rides does not repeatedly announce a new route.

## Validation

`go test ./...`, `go test -race -short ./...`, and `go vet ./...` cover the core.
`BenchmarkLiveRoutingPlan` is a small synthetic fixture, not a city-scale claim.
On an Apple M2, three runs measured baseline 108–119 µs/op and updated
114–131 µs/op (median 116 → 122 µs, 68 allocations/op in both versions).
