# elagoht/prometheus

A collage plugin that exports the framework's metrics to Prometheus — render and
fragment timings, cache events, HTTP responses by route, invalidations — and serves
them at `/metrics`.

```go
m := prometheus.NewMetrics(prometheus.Options{Namespace: "collage"})

app, err := collage.New(&collage.Config{
	Observability: collage.ObservabilityConfig{Metrics: m},
	Plugins:       []collage.Plugin{m},
})
```

Requires collage v0.23.0 or later.

## Both lines

The one value is the application's `collage.Metrics` and a plugin, and it needs to
be handed over as both. A plugin cannot set the application's `Config`, so only the
application can give collage its `Metrics`; and only a plugin can serve a path and
read the site's pages, which is what keeps the route label bounded (below).

- Without the `Observability` line, `/metrics` serves nothing collage measured.
- Without the `Plugins` line, the metrics are recorded but not served, every
  response's route is `other`, and a registration that failed goes unnoticed.

## The metrics

With the default namespace, `collage`:

| Metric | Type | Labels |
| --- | --- | --- |
| `collage_render_duration_seconds` | histogram | `page`, `cache_hit` |
| `collage_fragment_duration_seconds` | histogram | `page`, `fragment`, `outcome` (`ok`, `error`) |
| `collage_cache_events_total` | counter | `event` (`hit`, `miss`, `set`, `evict`, `invalidate`, `coalesced`) |
| `collage_http_request_duration_seconds` | histogram | `route`, `status` (`2xx`, `3xx`, `4xx`, `5xx`) |
| `collage_invalidations_total` | counter | — |
| `collage_invalidated_keys_total` | counter | — |

`render_duration_seconds` covers pages and documents alike; `page` is the page's or
document's name, and `page/fragment` for a fragment rendered on its own path. When
`cache_hit` is `true` the duration is the cache read, not a render. A climbing
`coalesced` count is a page expiring faster than it can be re-made — see
`collage.CacheCoalesced`.

`invalidated_keys_total` counts the keys an invalidation issued a removal for,
which is an upper bound on live entries removed: a key whose entry had already
expired is counted like any other.

## Why no label is a path

Every label takes its values from a set the application's code fixes: page,
document and fragment names, event kinds, status classes. None is taken from a
request. A raw path as a label is a new time series for every `/blog/whatever` a
crawler invents, and a Prometheus server holding a million series for one histogram
is one that has stopped answering.

So `route` is the name of the page whose pattern the request's path matched:
`/blog/a` and `/blog/b` are both `post`. The patterns are read from the application's
pages when the plugin starts, a leading locale segment is skipped as the router
skips it, and a document is learned the first time it renders — `robots.txt` is
labelled `robots`. What matches nothing is `other`: a 404, a mounted asset, a
handler of the application's own. `Routes` names prefixes to label as themselves
instead:

```go
prometheus.NewMetrics(prometheus.Options{Routes: []string{"/api/", "/static/"}})
```

The cache key, the invalidated tags and the full path are never labels for the same
reason.

## Serving the metrics

`/metrics` answers with the registry in Prometheus's text format and
`Cache-Control: no-store`. `Path` moves it, and `"-"` serves it nowhere — for an
application that serves its registry itself, on another port. It is answered from
middleware, after the application's own: `Host.Handle` serves prefixes ending in
`/`, and Prometheus scrapes `/metrics` without one by default.

Anyone who can reach the site can read its page names, error rates and traffic
unless `Token` is set. The scrape then has to send it:

```yaml
scrape_configs:
  - job_name: site
    authorization:
      credentials: s3cret
    static_configs:
      - targets: ["example.com"]
```

A request without it, or with another, is answered `401`.

## Options

| Option | Default | |
| --- | --- | --- |
| `Namespace` | `"collage"` | Prefix of every metric name. Go only |
| `Registry` | Prometheus's default registry | Where the metrics are registered and what `/metrics` serves. Go only |
| `Buckets` | `prometheus.DefBuckets` | Histogram bounds, in seconds. Go only |
| `Path` (`path`) | `"/metrics"` | Where the metrics are served; `"-"` nowhere |
| `Token` (`token`) | none | Required as `Authorization: Bearer <token>` |
| `Routes` (`routes`) | none | Path prefixes labelled as themselves |

The default registry is Prometheus's own, so the Go runtime's metrics and anything
the application registers with `promauto` are served too. `Namespace`, `Registry`
and `Buckets` are Go-only because the metrics are registered when `NewMetrics`
returns, before any configuration is read.

## Configuration

```json
{
  "elagoht/prometheus": {
    "path": "/metrics",
    "token": "s3cret",
    "routes": ["/api/"]
  }
}
```

A path or route that does not begin with `/`, a token a header cannot carry, and
metrics that could not be registered — two plugins on one registry, a name already
taken — stop the application from starting.

## Limitations

- A document whose path has parameters cannot be learned — `Host` has no
  `Documents` method, and `Host.URL` cannot build its path without them — so its
  responses are `other` unless a `Routes` prefix covers them.
- `Metrics.HTTPResponse` is given the raw path and the context from before the
  middleware ran, so the route label is worked out by matching the path again
  rather than read from the router's own match. Were collage to hand the resolved
  route to `HTTPResponse`, the matching here would go.
- Mounts and handlers the application registers are not visible to a plugin and
  are `other` until named in `Routes`.
