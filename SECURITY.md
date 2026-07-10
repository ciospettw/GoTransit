# Security

GoTransit is a network service that fetches remote data and serves HTTP —
take reports seriously, we do.

- **Reporting**: open a GitHub security advisory (preferred) or a private
  report to the maintainers. Please don't file exploitable issues publicly
  before a fix ships.
- **Scope of interest**: parser robustness (PBF / GTFS zip / GTFS-RT protobuf
  / osmChange XML are all hand-written and consume untrusted remote bytes),
  the WebSocket endpoint, and anything that lets a crafted feed crash or
  wedge the engine.
- **Non-goals**: the debug UI is a development tool; deployments exposing it
  publicly should set `debug_ui = false`. `allow_insecure` disables TLS
  verification by explicit operator choice — that's documented behavior, not
  a vulnerability.
