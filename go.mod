// A collage plugin that exports the framework's metrics to Prometheus: render and
// fragment timings, cache events, HTTP responses by route, invalidations. The same
// value is the application's Metrics and the plugin serving /metrics.
module github.com/Elagoht/collage-prometheus

go 1.26

require (
	github.com/Elagoht/collage v0.50.0
	github.com/prometheus/client_model v0.6.2
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_golang v1.24.1
	github.com/prometheus/common v0.70.1 // indirect
	github.com/prometheus/procfs v0.21.1 // indirect
	golang.org/x/sys v0.47.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

retract v0.2.4 // tagged by mistake on the previous release's code; use v0.2.5 or later
