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

Requires collage v0.25.0 or later.

## Both lines

The one value is the application's `collage.Metrics` and a plugin, and it needs to
be handed over as both. A plugin cannot set the application's `Config`, so only the
application can give collage its `Metrics`; and only a plugin can serve a path.

- Without the `Observability` line, `/metrics` serves nothing collage measured.
- Without the `Plugins` line, the metrics are recorded but not served, and a
  registration that failed goes unnoticed.

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

So `route` is what collage resolved the request to, from `collage.RouteOf`: its
kind and the name or prefix it was registered under.

| Request | `route` |
| --- | --- |
| `/blog/a`, `/blog/b`, `/tr/blog/c` | `page:post` |
| `/robots.txt` | `document:robots` |
| `/static/site.css` | `mount:/static/` |
| `/api/users/7` (an `app.Handle("/api/", ...)`) | `handler:/api/` |
| a form posted to an action | `action:subscribe` |
| `/metrics` | `handler:/metrics` |
| a 404, a redirect to a path's canonical form | `other` |

Every value comes from something the application registered, so there are as many
as it has routes. A page is one route in every locale. A handler is labelled by its
prefix, however many paths beneath it are asked for — `/api/users/7` and
`/api/users/8` are one series.

The cache key, the invalidated tags and the full path are never labels for the same
reason.

## Serving the metrics

`/metrics` answers with the registry in Prometheus's text format and
`Cache-Control: no-store`. `Path` moves it, and `"-"` serves it nowhere — for an
application that serves its registry itself, on another port. It is served with
`Host.Handle` as one exact path, inside the application's middleware; a page,
document or handler already at that path stops the application from starting
rather than one of them hiding the other.

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

The default registry is Prometheus's own, so the Go runtime's metrics and anything
the application registers with `promauto` are served too. `Namespace`, `Registry`
and `Buckets` are Go-only because the metrics are registered when `NewMetrics`
returns, before any configuration is read.

## Configuration

```json
{
  "elagoht/prometheus": {
    "path": "/metrics",
    "token": "s3cret"
  }
}
```

A path that does not begin with `/`, a path ending in `/`, a token a header cannot carry, and
metrics that could not be registered — two plugins on one registry, a name already
taken — stop the application from starting.

## Limitations

- A handler the application registers is one series, however different the
  requests beneath its prefix are: `/api/users` and `/api/orders` under
  `app.Handle("/api/", ...)` are both `handler:/api/`. Labelling finer would take
  the handler's own routing, which collage does not see.

## Changes

### v0.2.1

- `collage.json`: the plugin described to editors — its template functions,
  snippets and configuration schema — for the Collage Snippets & Highlighter
  extension and any tool reading it.

### v0.2.0

- The `route` label is what the request resolved to, from collage v0.25.0's
  `collage.RouteOf`, as `kind:name`: `page:post`, `document:robots`,
  `mount:/static/`, `handler:/api/`. It was the bare page or document name, found
  by matching the path against the pages' patterns again. Dashboards and alerts
  that select on `route` need the new values.
- Documents with parameters, mounts and the application's own handlers are
  labelled by their route instead of `other`.
- **`Routes` is removed**: every mount and handler now has its prefix as its route
  without being named. A `routes` key left in the configuration is ignored.
- Requires collage v0.25.0.

### v0.1.1

- `/metrics` is served with `Host.Handle`, which takes an exact path since collage
  v0.24.0, instead of middleware. A `Path` another route already answers, or one
  ending in `/`, now stops the application from starting.
- Requires collage v0.24.0.
