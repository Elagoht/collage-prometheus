// Package prometheus is a collage plugin that exports the framework's metrics to
// Prometheus, and serves them at /metrics.
//
//	m := prometheus.NewMetrics(prometheus.Options{Namespace: "collage"})
//	app, err := collage.New(&collage.Config{
//		Observability: collage.ObservabilityConfig{Metrics: m},
//		Plugins:       []collage.Plugin{m},
//	})
//
// The one value is both halves, and it needs both lines. A plugin cannot set the
// application's Config, so only the application can hand collage its Metrics; and
// only a plugin can serve a route, which is where the metrics are scraped from.
//
// # Labels
//
// Every label takes its values from a set the application's code fixes: page and
// fragment names, cache event kinds, status classes. None comes from a request. A
// raw path as a label is a new time series for every /blog/whatever a crawler
// invents, and a Prometheus server holding a million series for one histogram is
// one that has stopped answering. So the HTTP metric labels a request with what
// collage.RouteOf says it resolved to — "page:post", "document:feed",
// "mount:/static/" — and "other" when it resolved to nothing.
package prometheus

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Elagoht/collage/pkg/collage"
	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Name is the plugin's name, and the key its configuration is found under.
const Name = "elagoht/prometheus"

// OtherRoute is the route label of a request that resolved to no route: a 404, a
// redirect to a path's canonical form, a request middleware answered before
// routing.
const OtherRoute = "other"

// Options configures the plugin.
type Options struct {
	// Namespace prefixes every metric name: "collage" gives
	// collage_render_duration_seconds. Default "collage". It can only be set
	// from Go: the metrics are registered when NewMetrics returns, before any
	// configuration is read.
	Namespace string `json:"-"`
	// Registry is where the metrics are registered and what /metrics serves.
	// Unset, it is Prometheus's default registry, so metrics the application
	// registers with promauto and the Go runtime's own are served alongside.
	// It can only be set from Go.
	Registry *prom.Registry `json:"-"`
	// Buckets are the histograms' upper bounds, in seconds. Default
	// prometheus.DefBuckets, 5ms to 10s. It can only be set from Go.
	Buckets []float64 `json:"-"`
	// Path is where the metrics are served. Default "/metrics"; "-" serves
	// them nowhere, for an application that serves its registry itself.
	Path string `json:"path"`
	// Token, when set, is required as "Authorization: Bearer <token>" on the
	// metrics path. Without it anyone who can reach the site can read its
	// metrics — page names, error rates, traffic.
	Token string `json:"token"`
}

// Plugin is the application's collage.Metrics and the plugin that serves them.
type Plugin struct {
	opts     Options
	gatherer prom.Gatherer
	// regErr is a registration that failed in NewMetrics, which cannot return
	// it; Init does, so the application does not start without its metrics.
	regErr error

	render        *prom.HistogramVec
	fragment      *prom.HistogramVec
	cacheEvents   *prom.CounterVec
	http          *prom.HistogramVec
	invalidations prom.Counter
	invalidated   prom.Counter
}

// New returns the plugin, with its metrics registered. See NewMetrics.
func New(opts Options) *Plugin { return NewMetrics(opts) }

// NewMetrics returns the plugin with its metrics registered: hand the same value
// to Config.Observability.Metrics and to Config.Plugins.
func NewMetrics(opts Options) *Plugin {
	if opts.Namespace == "" {
		opts.Namespace = "collage"
	}
	if len(opts.Buckets) == 0 {
		opts.Buckets = prom.DefBuckets
	}
	var reg prom.Registerer = prom.DefaultRegisterer
	var gatherer prom.Gatherer = prom.DefaultGatherer
	if opts.Registry != nil {
		reg, gatherer = opts.Registry, opts.Registry
	}
	ns := opts.Namespace
	p := &Plugin{
		opts:     opts,
		gatherer: gatherer,
		render: prom.NewHistogramVec(prom.HistogramOpts{
			Namespace: ns, Name: "render_duration_seconds",
			Help:    "How long a page or document took to render, or to be read from the cache when cache_hit is true.",
			Buckets: opts.Buckets,
		}, []string{"page", "cache_hit"}),
		fragment: prom.NewHistogramVec(prom.HistogramOpts{
			Namespace: ns, Name: "fragment_duration_seconds",
			Help:    "How long a fragment's data handler took, by page, fragment and outcome.",
			Buckets: opts.Buckets,
		}, []string{"page", "fragment", "outcome"}),
		cacheEvents: prom.NewCounterVec(prom.CounterOpts{
			Namespace: ns, Name: "cache_events_total",
			Help: "Cache operations, by event: hit, miss, set, evict, invalidate, coalesced.",
		}, []string{"event"}),
		http: prom.NewHistogramVec(prom.HistogramOpts{
			Namespace: ns, Name: "http_request_duration_seconds",
			Help:    "How long a response took, by route (kind:name, as page:post or mount:/static/, or \"other\") and status class.",
			Buckets: opts.Buckets,
		}, []string{"route", "status"}),
		invalidations: prom.NewCounter(prom.CounterOpts{
			Namespace: ns, Name: "invalidations_total",
			Help: "Calls to invalidate cache entries by tag.",
		}),
		invalidated: prom.NewCounter(prom.CounterOpts{
			Namespace: ns, Name: "invalidated_keys_total",
			Help: "Cache keys invalidations issued a removal for; an upper bound on live entries removed.",
		}),
	}
	for _, c := range []prom.Collector{p.render, p.fragment, p.cacheEvents, p.http, p.invalidations, p.invalidated} {
		if err := reg.Register(c); err != nil {
			p.regErr = errors.Join(p.regErr, err)
		}
	}
	return p
}

func (p *Plugin) Name() string                   { return Name }
func (p *Plugin) Version() string                { return "0.2.2" }
func (p *Plugin) Shutdown(context.Context) error { return nil }

var (
	_ collage.Plugin  = (*Plugin)(nil)
	_ collage.Metrics = (*Plugin)(nil)
)

var tokenChars = regexp.MustCompile(`^[\x21-\x7e]+$`)

// Init reads the configuration and serves the metrics.
func (p *Plugin) Init(_ context.Context, host collage.Host) error {
	if p.regErr != nil {
		return fmt.Errorf("prometheus: registering the metrics: %w", p.regErr)
	}
	if err := host.Config(&p.opts); err != nil {
		return err
	}
	if p.opts.Path == "" {
		p.opts.Path = "/metrics"
	}
	if p.opts.Path != "-" && !strings.HasPrefix(p.opts.Path, "/") {
		return fmt.Errorf("prometheus: path %q must begin with /", p.opts.Path)
	}
	// Host.Handle reads a trailing "/" as a prefix claiming everything beneath
	// it; the metrics are one path.
	if p.opts.Path != "-" && strings.HasSuffix(p.opts.Path, "/") {
		return fmt.Errorf("prometheus: path %q must not end in /: the metrics are one path, not a prefix", p.opts.Path)
	}
	if p.opts.Token != "" && !tokenChars.MatchString(p.opts.Token) {
		return errors.New("prometheus: the token must be printable ASCII without spaces, as a header carries it")
	}
	if p.opts.Path == "-" {
		return nil
	}
	if err := host.Handle(p.opts.Path, p.handler()); err != nil {
		return fmt.Errorf("prometheus: %w", err)
	}
	return nil
}

// handler answers the metrics path. Host.Handle serves it as an exact path, so a
// page or handler of the application's own at the same path is a startup error
// rather than one of the two silently hiding the other.
func (p *Plugin) handler() http.Handler {
	metrics := promhttp.HandlerFor(p.gatherer, promhttp.HandlerOpts{})
	want := []byte("Bearer " + p.opts.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.opts.Token != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="metrics"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// A scrape is never a page anyone else should be served from a cache.
		w.Header().Set("Cache-Control", "no-store")
		metrics.ServeHTTP(w, r)
	})
}

// RenderDuration observes a render, or a cache read when cacheHit is true.
func (p *Plugin) RenderDuration(_ context.Context, page string, d time.Duration, cacheHit bool) {
	p.render.WithLabelValues(page, strconv.FormatBool(cacheHit)).Observe(d.Seconds())
}

// FragmentDuration observes a fragment's data fetch.
func (p *Plugin) FragmentDuration(_ context.Context, page, fragment string, d time.Duration, err error) {
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	p.fragment.WithLabelValues(page, fragment, outcome).Observe(d.Seconds())
}

// CacheEvent counts a cache operation. The key is not a label: it is one per
// cached URL.
func (p *Plugin) CacheEvent(_ context.Context, event collage.CacheEvent, _ string) {
	p.cacheEvents.WithLabelValues(string(event)).Inc()
}

// HTTPResponse observes a response, labelled by the route it resolved to. The
// path is not a label: it is one per URL a crawler invents.
func (p *Plugin) HTTPResponse(ctx context.Context, status int, _ string, d time.Duration) {
	p.http.WithLabelValues(route(ctx), statusClass(status)).Observe(d.Seconds())
}

// route is the route label: the kind and the registered name or prefix collage
// resolved the request to, as "page:post", "document:feed", "mount:/static/",
// "handler:/api/". Both come from what the application registered, so the set is
// as bounded as its routes are.
func route(ctx context.Context) string {
	kind, name := collage.RouteOf(ctx)
	if kind == "" {
		return OtherRoute
	}
	return kind + ":" + name
}

// Invalidation counts an invalidation and the keys it reached. The tags are not a
// label: an application may tag by record ID.
func (p *Plugin) Invalidation(_ context.Context, _ []string, keys int) {
	p.invalidations.Inc()
	p.invalidated.Add(float64(keys))
}

func statusClass(status int) string {
	if status < 100 || status > 599 {
		return "unknown"
	}
	return strconv.Itoa(status/100) + "xx"
}
